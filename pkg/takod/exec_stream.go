package takod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/redentordev/tako-cli/pkg/takoapi/ptystream"
)

// execStreamChunk bounds one output frame; PTY reads rarely exceed it.
const execStreamChunk = 32 * 1024

// execStreamIdleTick is how often an interactive session checks its idle
// deadline.
const execStreamIdleTick = 15 * time.Second

// execStreamIO is the duplex frame transport for one interactive session:
// reads come from the hijacked connection's buffered reader, writes go
// through a serializing frame writer.
type execStreamIO struct {
	reader io.Reader
	writer *ptystream.Writer
}

// RunExecStream runs one interactive (optionally PTY-backed) exec session
// over an upgraded frame stream: container frame first, output frames until
// the process ends, then a terminal exit frame. It handles stdin/resize
// frames from the client, enforces absolute and idle timeouts, and cleans up
// the docker process (and oneoff container) when the client disconnects.
func RunExecStream(ctx context.Context, req ExecRequest, reader io.Reader, writer io.Writer) error {
	if err := validateExecRequest(&req); err != nil {
		return err
	}
	if !req.Interactive {
		return fmt.Errorf("exec stream requires an interactive request")
	}

	run, err := prepareExecRun(ctx, req)
	if err != nil {
		return err
	}
	if run.cleanup != nil {
		defer run.cleanup()
	}

	stream := &execStreamIO{reader: reader, writer: ptystream.NewWriter(writer)}
	if err := stream.writer.WriteFrame(ptystream.FrameContainer, []byte(run.container)); err != nil {
		return err
	}

	timeoutSeconds := req.TimeoutSeconds
	if timeoutSeconds == 0 {
		timeoutSeconds = defaultExecStreamTimeoutSeconds
	}
	idleSeconds := req.IdleTimeoutSeconds
	if idleSeconds == 0 {
		idleSeconds = defaultExecStreamIdleTimeoutSeconds
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	exitCode, runErr := runExecStreamProcess(runCtx, req, run, stream, time.Duration(idleSeconds)*time.Second, cancel)
	if req.Mode == ExecModeOneOff && runCtx.Err() != nil {
		removeExecContainer(run.container)
	}
	if runErr != nil {
		_ = stream.writer.WriteFrame(ptystream.FrameError, []byte(runErr.Error()))
	} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		_ = stream.writer.WriteFrame(ptystream.FrameError, []byte(fmt.Sprintf("exec session ended after %ds timeout", timeoutSeconds)))
	}
	return stream.writer.WriteFrame(ptystream.FrameExit, ptystream.EncodeExit(exitCode))
}

// runExecStreamProcess starts the docker process (under a pty when
// requested), pumps frames both ways, and returns the process exit code.
// A non-exit failure (docker missing, pty allocation) reports -1 with the
// error.
func runExecStreamProcess(ctx context.Context, req ExecRequest, run *execRun, stream *execStreamIO, idleTimeout time.Duration, cancel context.CancelFunc) (int, error) {
	cmd := dockerCommandContext(ctx, "docker", run.args...)

	var processIn io.WriteCloser
	var processOut io.Reader
	var ptmx *os.File
	if req.PTY {
		size := &pty.Winsize{Cols: uint16(req.Cols), Rows: uint16(req.Rows)}
		if size.Cols == 0 {
			size.Cols = 80
		}
		if size.Rows == 0 {
			size.Rows = 24
		}
		opened, err := pty.StartWithSize(cmd, size)
		if err != nil {
			return -1, fmt.Errorf("failed to allocate pty: %w", err)
		}
		ptmx = opened
		defer ptmx.Close()
		processIn = ptmx
		processOut = ptmx
	} else {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return -1, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return -1, err
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			return -1, err
		}
		processIn = stdin
		processOut = stdout
	}

	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	touch := func() { lastActivity.Store(time.Now().UnixNano()) }

	// Client frame pump: stdin and resize frames in, session teardown when
	// the client disconnects.
	go func() {
		defer cancel()
		for {
			frame, err := ptystream.ReadFrame(stream.reader)
			if err != nil {
				return
			}
			touch()
			switch frame.Type {
			case ptystream.FrameStdin:
				if len(frame.Payload) == 0 {
					// Zero-length stdin closes the remote input for
					// non-PTY piping (mirrors closing the pipe).
					if !req.PTY {
						_ = processIn.Close()
					}
					continue
				}
				if _, err := processIn.Write(frame.Payload); err != nil {
					return
				}
			case ptystream.FrameResize:
				if ptmx == nil {
					continue
				}
				if size, err := ptystream.DecodeResize(frame.Payload); err == nil {
					_ = pty.Setsize(ptmx, &pty.Winsize{Cols: size.Cols, Rows: size.Rows})
				}
			default:
				// Unknown client frames are ignored so the protocol can
				// grow additively.
			}
		}
	}()

	// Idle watchdog.
	go func() {
		ticker := time.NewTicker(execStreamIdleTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				idleFor := time.Since(time.Unix(0, lastActivity.Load()))
				if idleFor >= idleTimeout {
					_ = stream.writer.WriteFrame(ptystream.FrameError, []byte(fmt.Sprintf("exec session idle for %s; closing", idleFor.Truncate(time.Second))))
					cancel()
					return
				}
			}
		}
	}()

	// Output pump: PTY (or merged pipe) bytes out as stdout frames. A PTY
	// read error after the child exits is the normal EIO close, not a
	// failure.
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		buf := make([]byte, execStreamChunk)
		for {
			n, err := processOut.Read(buf)
			if n > 0 {
				touch()
				if writeErr := stream.writer.WriteFrame(ptystream.FrameStdout, buf[:n]); writeErr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	err := cmd.Wait()
	<-outputDone
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if ctx.Err() != nil {
		return -1, nil
	}
	return -1, err
}
