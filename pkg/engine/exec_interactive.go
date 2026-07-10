package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takoapi/ptystream"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// ExecTerminal carries the local terminal endpoints for an interactive exec
// session. The caller owns raw-mode setup and restoration.
type ExecTerminal struct {
	Stdin  io.Reader
	Stdout io.Writer
	// TTY requests a server-side pseudo-terminal (docker exec -t).
	TTY bool
	// InitialSize seeds the remote PTY; zero fields use server defaults.
	InitialSize ptystream.Winsize
	// Resize delivers live terminal size changes (SIGWINCH). Optional.
	Resize <-chan ptystream.Winsize
}

// ExecInteractive runs a command in a service's context with stdin attached
// over a full-duplex frame stream (SSH direct-streamlocal to the takod
// socket — no remote curl). With terminal.TTY the remote process runs under
// a pseudo-terminal that follows local resizes. Raw terminal bytes flow
// through terminal.Stdin/Stdout, not events; interactive exec is rejected in
// machine output modes at the command layer.
func (e *Engine) ExecInteractive(ctx context.Context, req ExecRequest, terminal ExecTerminal) (*ExecResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if terminal.Stdin == nil || terminal.Stdout == nil {
		return nil, invalidRequestf("interactive exec requires terminal stdin and stdout")
	}
	if req.Config == nil {
		return nil, invalidRequestf("exec request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("exec request requires an environment")
	}
	if strings.TrimSpace(req.Service) == "" {
		return nil, invalidRequestf("exec request requires a service")
	}
	if len(req.Command) == 0 {
		return nil, invalidRequestf("exec request requires a command (tako exec %s -- CMD [ARGS...])", req.Service)
	}
	if req.Replica < 0 {
		return nil, invalidRequestf("replica must be positive")
	}
	if req.Replica > 0 && req.OneOff {
		return nil, invalidRequestf("--replica selects a running container and cannot be combined with --oneoff")
	}

	cfg := req.Config
	envName := req.Environment
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get services: %w", err)
	}
	service, ok := services[req.Service]
	if !ok {
		return nil, invalidRequestf("service '%s' not found in environment %s", req.Service, envName)
	}
	for _, server := range cfg.Servers {
		e.RegisterSecret(server.Password)
	}

	mode := takod.ExecModeAttach
	if req.OneOff {
		mode = takod.ExecModeOneOff
	}
	result := &ExecResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindExecResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Service:     req.Service,
		Mode:        mode,
		Command:     append([]string(nil), req.Command...),
		ExitCode:    -1,
	}

	serverName, serverCfg, err := e.resolveExecServer(ctx, cfg, envName, req.Service, req.Server)
	if err != nil {
		return nil, err
	}
	result.Server = serverName
	result.Host = serverCfg.Host

	takodReq := takod.ExecRequest{
		Project:        cfg.Project.Name,
		Environment:    envName,
		Service:        req.Service,
		Mode:           mode,
		Command:        append([]string(nil), req.Command...),
		Replica:        req.Replica,
		Network:        runtimeid.NetworkName(cfg.Project.Name, envName),
		TimeoutSeconds: int(req.Timeout / time.Second),
		Interactive:    true,
		PTY:            terminal.TTY,
		Cols:           int(terminal.InitialSize.Cols),
		Rows:           int(terminal.InitialSize.Rows),
	}
	if req.OneOff {
		envContent, err := buildExecEnvFileContent(e, envName, &service)
		if err != nil {
			return nil, err
		}
		takodReq.EnvFileContent = envContent
		_, mounts, filesHash, err := deployer.PrepareServiceFilesPayload(cfg.Project.Name, envName, req.Service, &service)
		if err != nil {
			return nil, err
		}
		takodReq.Mounts = append(takodReq.Mounts, mounts...)
		if filesHash != "" {
			takodReq.FileSetID, err = deployer.ServiceFileSetID(filesHash)
			if err != nil {
				return nil, err
			}
		}
	}

	client, err := connectTakodStreamNodeContext(ctx, serverCfg)
	if err != nil {
		return nil, &ConnectivityError{Err: fmt.Errorf("failed to connect to node %s: %w", serverName, err)}
	}
	defer client.Close()
	if req.OneOff && len(service.Files) > 0 {
		if err := ensureServiceFilesCapability(ctx, client, TakodSocketFromConfig(cfg)); err != nil {
			return nil, err
		}
	}

	stream, err := takodclient.UpgradeStream(ctx, client, TakodSocketFromConfig(cfg), takodclient.ExecEndpoint(), takodReq)
	if err != nil {
		return nil, fmt.Errorf("failed to open interactive exec stream (node agent may predate PTY exec; run 'tako upgrade servers'): %w", err)
	}
	defer stream.Close()

	e.emit(events.Event{
		Type:    events.TypeExecStarted,
		Phase:   events.PhaseLogs,
		Level:   events.LevelDebug,
		Service: req.Service,
		Node:    serverName,
		Message: fmt.Sprintf("Using node: %s (%s)\n", serverName, serverCfg.Host),
		Data:    map[string]any{"mode": mode, "node": serverName, "host": serverCfg.Host, "command": takodReq.Command, "pty": terminal.TTY},
	})

	// ctx cancellation tears the connection down, which unblocks the frame
	// read loop below.
	stopWatch := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stopWatch()

	writer := ptystream.NewWriter(stream.Conn)
	go func() {
		buf := make([]byte, 16*1024)
		for {
			n, err := terminal.Stdin.Read(buf)
			if n > 0 {
				if writeErr := writer.WriteFrame(ptystream.FrameStdin, buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				// A zero-length stdin frame signals end-of-input for
				// non-PTY piping.
				_ = writer.WriteFrame(ptystream.FrameStdin, nil)
				return
			}
		}
	}()
	if terminal.Resize != nil {
		go func() {
			for size := range terminal.Resize {
				if err := writer.WriteFrame(ptystream.FrameResize, ptystream.EncodeResize(size)); err != nil {
					return
				}
			}
		}()
	}

	started := time.Now()
	var remoteError string
	exitSeen := false
	var streamErr error
	for {
		frame, err := ptystream.ReadFrame(stream.Reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				streamErr = err
			}
			break
		}
		switch frame.Type {
		case ptystream.FrameContainer:
			result.Container = string(frame.Payload)
		case ptystream.FrameStdout:
			if _, err := terminal.Stdout.Write(frame.Payload); err != nil {
				streamErr = err
			}
		case ptystream.FrameError:
			remoteError = e.redactor.Redact(string(frame.Payload))
			fmt.Fprintf(terminal.Stdout, "\r\n%s\r\n", remoteError)
		case ptystream.FrameExit:
			if code, err := ptystream.DecodeExit(frame.Payload); err == nil {
				result.ExitCode = code
				exitSeen = true
			}
		}
		if exitSeen {
			break
		}
	}
	result.DurationMs = time.Since(started).Milliseconds()

	var opErr error
	switch {
	case streamErr != nil:
		opErr = streamErr
	case !exitSeen && remoteError != "":
		opErr = errors.New(remoteError)
	case !exitSeen:
		opErr = fmt.Errorf("exec stream ended without an exit status")
	}
	if opErr != nil && ctx.Err() != nil {
		opErr = ctx.Err()
	}
	if opErr != nil {
		result.Error = opErr.Error()
	}

	e.emit(events.Event{
		Type:    events.TypeExecCompleted,
		Phase:   events.PhaseLogs,
		Level:   events.LevelDebug,
		Service: req.Service,
		Node:    serverName,
		Data:    map[string]any{"exitCode": result.ExitCode, "durationMs": result.DurationMs, "pty": terminal.TTY},
	})
	return result, opErr
}
