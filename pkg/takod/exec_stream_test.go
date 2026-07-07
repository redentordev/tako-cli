package takod

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takoapi/ptystream"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func interactiveExecRequest(pty bool) ExecRequest {
	return ExecRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Mode:        ExecModeAttach,
		Command:     []string{"sh"},
		Interactive: true,
		PTY:         pty,
	}
}

// streamClient is the test-side end of an in-memory duplex frame transport.
// A reader goroutine drains server frames into incoming from the moment the
// session starts, so the server's pipe writes never block on test ordering.
type streamClient struct {
	writer   *io.PipeWriter
	frames   *ptystream.Writer
	incoming chan ptystream.Frame
	readErr  chan error
}

func newStreamSession(t *testing.T, req ExecRequest) (*streamClient, chan error) {
	t.Helper()
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- RunExecStream(context.Background(), req, serverReader, serverWriter)
		_ = serverWriter.Close()
	}()
	client := &streamClient{
		writer:   clientWriter,
		frames:   ptystream.NewWriter(clientWriter),
		incoming: make(chan ptystream.Frame, 256),
		readErr:  make(chan error, 1),
	}
	go func() {
		defer close(client.incoming)
		for {
			frame, err := ptystream.ReadFrame(clientReader)
			if err != nil {
				client.readErr <- err
				return
			}
			client.incoming <- frame
		}
	}()
	return client, done
}

// collectUntilExit consumes buffered frames until the exit frame, returning
// the merged output, the container name, and the exit code.
func (c *streamClient) collectUntilExit(t *testing.T) (output string, container string, exitCode int) {
	t.Helper()
	var buf strings.Builder
	deadline := time.After(30 * time.Second)
	for {
		select {
		case frame, ok := <-c.incoming:
			if !ok {
				t.Fatalf("stream closed before exit frame (output so far: %q)", buf.String())
			}
			switch frame.Type {
			case ptystream.FrameContainer:
				container = string(frame.Payload)
			case ptystream.FrameStdout, ptystream.FrameError:
				buf.Write(frame.Payload)
			case ptystream.FrameExit:
				code, err := ptystream.DecodeExit(frame.Payload)
				if err != nil {
					t.Fatalf("bad exit frame: %v", err)
				}
				return buf.String(), container, code
			}
		case err := <-c.readErr:
			t.Fatalf("stream ended before exit frame: %v (output so far: %q)", err, buf.String())
		case <-deadline:
			t.Fatalf("timed out waiting for exit frame (output so far: %q)", buf.String())
		}
	}
}

func TestRunExecStreamInteractiveEchoSession(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo_production_web_1\n")
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_INTERACTIVE", "echo")
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_EXIT", "7")

	client, done := newStreamSession(t, interactiveExecRequest(false))
	if err := client.frames.WriteFrame(ptystream.FrameStdin, []byte("hello\n")); err != nil {
		t.Fatalf("stdin frame write failed: %v", err)
	}
	if err := client.frames.WriteFrame(ptystream.FrameStdin, []byte("exit\n")); err != nil {
		t.Fatalf("stdin frame write failed: %v", err)
	}

	output, container, exitCode := client.collectUntilExit(t)
	if err := <-done; err != nil {
		t.Fatalf("RunExecStream returned error: %v", err)
	}
	if container != "demo_production_web_1" {
		t.Fatalf("container frame = %q", container)
	}
	if !strings.Contains(output, "echo:hello") {
		t.Fatalf("echoed stdin missing from output: %q", output)
	}
	if exitCode != 7 {
		t.Fatalf("exit code = %d, want 7 (remote exit must reach the exit frame)", exitCode)
	}
}

func TestRunExecStreamClosesStdinOnZeroLengthFrame(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo_production_web_1\n")
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_INTERACTIVE", "echo")

	client, done := newStreamSession(t, interactiveExecRequest(false))
	if err := client.frames.WriteFrame(ptystream.FrameStdin, []byte("piped\n")); err != nil {
		t.Fatalf("stdin frame write failed: %v", err)
	}
	// EOF signal: the helper's stdin scanner ends and the process exits 0.
	if err := client.frames.WriteFrame(ptystream.FrameStdin, nil); err != nil {
		t.Fatalf("stdin close frame write failed: %v", err)
	}

	output, _, exitCode := client.collectUntilExit(t)
	if err := <-done; err != nil {
		t.Fatalf("RunExecStream returned error: %v", err)
	}
	if !strings.Contains(output, "echo:piped") {
		t.Fatalf("piped stdin missing from output: %q", output)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
}

func TestRunExecStreamPTYSession(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo_production_web_1\n")
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_INTERACTIVE", "echo")
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_EXIT", "3")

	req := interactiveExecRequest(true)
	req.Cols = 120
	req.Rows = 40
	client, done := newStreamSession(t, req)
	if err := client.frames.WriteFrame(ptystream.FrameResize, ptystream.EncodeResize(ptystream.Winsize{Cols: 100, Rows: 30})); err != nil {
		t.Fatalf("resize frame write failed: %v", err)
	}
	if err := client.frames.WriteFrame(ptystream.FrameStdin, []byte("hello\n")); err != nil {
		t.Fatalf("stdin frame write failed: %v", err)
	}
	if err := client.frames.WriteFrame(ptystream.FrameStdin, []byte("exit\n")); err != nil {
		t.Fatalf("stdin frame write failed: %v", err)
	}

	output, container, exitCode := client.collectUntilExit(t)
	if err := <-done; err != nil {
		t.Fatalf("RunExecStream returned error: %v", err)
	}
	if container != "demo_production_web_1" {
		t.Fatalf("container frame = %q", container)
	}
	if !strings.Contains(output, "echo:hello") {
		t.Fatalf("echoed stdin missing from pty output: %q", output)
	}
	if exitCode != 3 {
		t.Fatalf("exit code = %d, want 3", exitCode)
	}
}

func TestRunExecStreamKillsProcessOnClientDisconnect(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo_production_web_1\n")
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_INTERACTIVE", "echo")

	client, done := newStreamSession(t, interactiveExecRequest(false))
	// Simulate the client vanishing mid-session: the frame read loop fails,
	// the session context cancels, and the process is killed.
	_ = client.writer.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunExecStream returned error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("session did not end after client disconnect")
	}
}

func TestRunExecStreamRejectsNonInteractiveRequest(t *testing.T) {
	req := interactiveExecRequest(false)
	req.Interactive = false
	err := RunExecStream(context.Background(), req, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatalf("error = %v, want interactive requirement", err)
	}
}

// shortSocketPath returns a unix socket path short enough for the ~104-byte
// sun_path limit (t.TempDir embeds the full test name and overflows it).
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tk")
	if err != nil {
		t.Fatalf("failed to create socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "t.sock")
}

// TestExecStreamUpgradeEndToEnd runs the real HTTP upgrade path: the takod
// mux on a unix socket, the client-side UpgradeStream handshake, and a full
// framed session with the fake docker helper.
func TestExecStreamUpgradeEndToEnd(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo_production_web_1\n")
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_INTERACTIVE", "echo")
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_EXIT", "5")

	socket := shortSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("failed to listen on test socket: %v", err)
	}
	defer listener.Close()

	server := NewServer(socket, t.TempDir(), "test")
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/exec", server.handleExec)
	httpServer := &http.Server{Handler: mux}
	go func() { _ = httpServer.Serve(listener) }()
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := takodclient.UpgradeStream(ctx, unixDialer{}, socket, takodclient.ExecEndpoint(), interactiveExecRequest(false))
	if err != nil {
		t.Fatalf("UpgradeStream failed: %v", err)
	}
	defer stream.Close()

	writer := ptystream.NewWriter(stream.Conn)
	if err := writer.WriteFrame(ptystream.FrameStdin, []byte("over-socket\n")); err != nil {
		t.Fatalf("stdin frame write failed: %v", err)
	}
	if err := writer.WriteFrame(ptystream.FrameStdin, []byte("exit\n")); err != nil {
		t.Fatalf("stdin frame write failed: %v", err)
	}

	output, container, exitCode := readFramesUntilExit(t, stream.Reader)
	if container != "demo_production_web_1" {
		t.Fatalf("container frame = %q", container)
	}
	if !strings.Contains(output, "echo:over-socket") {
		t.Fatalf("echoed stdin missing: %q", output)
	}
	if exitCode != 5 {
		t.Fatalf("exit code = %d, want 5", exitCode)
	}
}

// TestExecStreamUpgradeRequiresHandshake pins that interactive requests
// without the Upgrade header get a plain HTTP error, not a hijack.
func TestExecStreamUpgradeRequiresHandshake(t *testing.T) {
	socket := shortSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("failed to listen on test socket: %v", err)
	}
	defer listener.Close()

	server := NewServer(socket, t.TempDir(), "test")
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/exec", server.handleExec)
	httpServer := &http.Server{Handler: mux}
	go func() { _ = httpServer.Serve(listener) }()
	defer httpServer.Close()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	response, err := client.Post("http://takod/v1/exec", "application/json",
		strings.NewReader(`{"project":"demo","environment":"production","service":"web","mode":"attach","command":["sh"],"interactive":true}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUpgradeRequired)
	}
}

// readFramesUntilExit reads frames from a real (kernel-buffered) stream
// until the exit frame.
func readFramesUntilExit(t *testing.T, reader io.Reader) (output string, container string, exitCode int) {
	t.Helper()
	var buf strings.Builder
	for {
		frame, err := ptystream.ReadFrame(reader)
		if err != nil {
			t.Fatalf("stream ended before exit frame: %v (output so far: %q)", err, buf.String())
		}
		switch frame.Type {
		case ptystream.FrameContainer:
			container = string(frame.Payload)
		case ptystream.FrameStdout, ptystream.FrameError:
			buf.Write(frame.Payload)
		case ptystream.FrameExit:
			code, err := ptystream.DecodeExit(frame.Payload)
			if err != nil {
				t.Fatalf("bad exit frame: %v", err)
			}
			return buf.String(), container, code
		}
	}
}

// unixDialer satisfies takodclient.UnixSocketDialer with a local dial for
// tests (production uses the SSH direct-streamlocal channel).
type unixDialer struct{}

func (unixDialer) DialUnixSocket(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}
