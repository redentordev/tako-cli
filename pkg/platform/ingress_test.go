package platform

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takoapi/ptystream"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

type ingressAgent struct {
	mu       sync.Mutex
	requests []string
}

type partialIngressAgent struct{ ingressAgent }

type rejectingIngressAgent struct{ ingressAgent }

type persistenceFailIngressAgent struct {
	ingressAgent
	historyDir string
}

func (a *persistenceFailIngressAgent) StreamOutput(_ context.Context, _ string, _ string, _ io.Reader, _ string, output io.Writer, _ io.Writer) error {
	_, _ = io.WriteString(output, `{"ok":true}`)
	_ = os.RemoveAll(a.historyDir)
	return os.WriteFile(a.historyDir, []byte("not-a-directory"), 0600)
}

type closedIngressWriter struct{}

func (closedIngressWriter) Write([]byte) (int, error) { return 0, net.ErrClosed }

func (a *rejectingIngressAgent) RequestJSON(_ context.Context, method string, endpoint string, _ any) (string, error) {
	body := `{"error":"lease held"}`
	return body, &takodclient.HTTPError{Method: method, Endpoint: endpoint, Status: http.StatusConflict, Body: body}
}

func (a *partialIngressAgent) StreamOutput(_ context.Context, _ string, _ string, _ io.Reader, _ string, output io.Writer, _ io.Writer) error {
	_, _ = io.WriteString(output, `{"partial":`)
	return io.ErrUnexpectedEOF
}

func (a *ingressAgent) Status(context.Context) (*takodclient.AgentStatus, error) { return nil, nil }

func (a *ingressAgent) RequestJSON(_ context.Context, method string, endpoint string, _ any) (string, error) {
	a.mu.Lock()
	a.requests = append(a.requests, method+" "+endpoint)
	a.mu.Unlock()
	return `{"applied":true}`, nil
}

func (a *ingressAgent) StreamOutput(_ context.Context, method string, endpoint string, _ io.Reader, _ string, output io.Writer, _ io.Writer) error {
	_, err := io.WriteString(output, `{"runtime":"takod","endpoint":"`+method+` `+endpoint+`"}`)
	return err
}

func TestWorkerIngressDurablyQueuesJSONMutationAndProxiesReads(t *testing.T) {
	if os.Getegid() <= 0 {
		t.Skip("test requires a non-root group for the protected ingress")
	}
	dir, err := os.MkdirTemp("/tmp", "tako-ingress-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	store, err := NewOperationStore(dir, "node-test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	agent := &ingressAgent{}
	admission, err := NewAdmissionController(DefaultResourcePolicy(), fixedDiskProbe{available: DefaultResourcePolicy().MinimumFreeDiskBytes + 1}, dir)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewOperationEngine(store, AgentOperationExecutor{Agent: agent}, admission, 1)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "worker.sock")
	ingress, err := newWorkerIngress(socket, os.Getegid(), store, agent, admission, dir, "node-test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = engine.Run(ctx) }()
	go func() { _ = ingress.Serve() }()
	t.Cleanup(func() {
		shutdown, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = ingress.Shutdown(shutdown)
	})
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	request, _ := http.NewRequest(http.MethodPost, "http://worker/v1/reconcile-service", strings.NewReader(`{"project":"demo"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(data), `"applied":true`) {
		t.Fatalf("mutation response = %d %q", response.StatusCode, data)
	}
	agent.mu.Lock()
	requests := append([]string(nil), agent.requests...)
	agent.mu.Unlock()
	if len(requests) != 1 || requests[0] != "POST /v1/reconcile-service" {
		t.Fatalf("agent requests = %#v", requests)
	}
	readResponse, err := client.Get("http://worker/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	readData, _ := io.ReadAll(readResponse.Body)
	_ = readResponse.Body.Close()
	if readResponse.StatusCode != http.StatusOK || !strings.Contains(string(readData), `GET /v1/status`) {
		t.Fatalf("read response = %d %q", readResponse.StatusCode, readData)
	}
}

func TestWorkerIngressRejectsNonJSONQueuedMutation(t *testing.T) {
	data, err := readIngressJSON(strings.NewReader(strings.Repeat("x", workerIngressMaxJSONBytes+1)))
	if err == nil || data != nil {
		t.Fatalf("oversized ingress body = %d bytes, %v", len(data), err)
	}
}

func TestWorkerIngressPreservesKnownUpstreamHTTPError(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOperationStore(dir, "node-test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	policy := DefaultResourcePolicy()
	admission, err := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes + 1}, dir)
	if err != nil {
		t.Fatal(err)
	}
	agent := &rejectingIngressAgent{}
	engine, err := NewOperationEngine(store, AgentOperationExecutor{Agent: agent}, admission, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = engine.Run(ctx) }()
	ingress := &workerIngress{
		store: store, agent: agent, admission: admission, nodeID: "node-test", now: time.Now,
		queueSlots: make(chan struct{}, 1), streamSlots: make(chan struct{}, 1), upgradeSlots: make(chan struct{}, 1),
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/lease", strings.NewReader(`{"operation":"deploy"}`))
	request.Header.Set("Content-Type", "application/json")
	ingress.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "lease held") {
		t.Fatalf("known upstream response = %d %q", recorder.Code, recorder.Body.String())
	}
	history, err := os.ReadDir(filepath.Join(dir, operationHistoryDir))
	if err != nil || len(history) != 1 {
		t.Fatalf("durable HTTP rejection history = %d, %v", len(history), err)
	}
	state, err := store.Read(strings.TrimSuffix(history[0].Name(), ".json"))
	if err != nil || state.Status != "failed" || state.ResponseStatus != http.StatusConflict {
		t.Fatalf("durable HTTP rejection outcome = %#v, %v", state, err)
	}
}

func TestWorkerIngressDurablyRecordsDirectStreamMutation(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOperationStore(dir, "node-test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	policy := DefaultResourcePolicy()
	admission, err := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes + 1}, dir)
	if err != nil {
		t.Fatal(err)
	}
	ingress := &workerIngress{
		store: store, agent: &ingressAgent{}, admission: admission, nodeID: "node-test", now: time.Now,
		queueSlots: make(chan struct{}, 1), streamSlots: make(chan struct{}, 1), upgradeSlots: make(chan struct{}, 1),
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/jobs/trigger", strings.NewReader(`{"project":"demo"}`))
	request.Header.Set("Content-Type", "application/json")
	ingress.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("stream response = %d %q", recorder.Code, recorder.Body.String())
	}
	queued, _ := os.ReadDir(filepath.Join(dir, operationQueueDir))
	history, _ := os.ReadDir(filepath.Join(dir, operationHistoryDir))
	if len(queued) != 0 || len(history) != 1 {
		t.Fatalf("durable stream files queue=%d history=%d", len(queued), len(history))
	}
}

func TestWorkerIngressSurfacesTerminalPersistenceFailure(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOperationStore(dir, "node-test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	policy := DefaultResourcePolicy()
	admission, err := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes + 1}, dir)
	if err != nil {
		t.Fatal(err)
	}
	fatalErr := make(chan error, 1)
	ingress := &workerIngress{
		store: store, agent: &persistenceFailIngressAgent{historyDir: store.historyDir}, admission: admission, nodeID: "node-test", now: time.Now,
		queueSlots: make(chan struct{}, 1), streamSlots: make(chan struct{}, 1), upgradeSlots: make(chan struct{}, 1), fatalErr: fatalErr,
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/jobs/trigger", strings.NewReader(`{"project":"demo"}`))
	request.Header.Set("Content-Type", "application/json")
	ingress.ServeHTTP(recorder, request)
	select {
	case err := <-fatalErr:
		if err == nil || !strings.Contains(err.Error(), "persist terminal runtime operation") {
			t.Fatalf("fatal persistence error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal persistence failure did not make ingress unhealthy")
	}
}

func TestWorkerIngressDoesNotAppendHTTPErrorAfterPartialStream(t *testing.T) {
	ingress := &workerIngress{agent: &partialIngressAgent{}, now: time.Now}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://worker/v1/status", nil)
	ingress.proxyStream(recorder, request, "/v1/status")
	if got := recorder.Body.String(); got != `{"partial":` {
		t.Fatalf("partial stream was corrupted by a second response: %q", got)
	}
}

func TestInteractiveRelayRequiresTerminalExitFrame(t *testing.T) {
	var source bytes.Buffer
	if err := ptystream.WriteFrame(&source, ptystream.FrameStdout, []byte("partial")); err != nil {
		t.Fatal(err)
	}
	var destination bytes.Buffer
	if err := relayPTYFrames(&destination, &source); err == nil || !strings.Contains(err.Error(), "without a terminal exit frame") || benignRelayError(err) {
		t.Fatalf("truncated PTY relay error = %v (benign=%v)", err, benignRelayError(err))
	}
	if destination.Len() == 0 {
		t.Fatal("relay discarded frames received before interruption")
	}
	if err := ptystream.WriteFrame(&source, ptystream.FrameExit, ptystream.EncodeExit(0)); err != nil {
		t.Fatal(err)
	}
	destination.Reset()
	if err := relayPTYFrames(&destination, &source); err != nil {
		t.Fatalf("terminal PTY relay failed: %v", err)
	}
	var exitOnly bytes.Buffer
	_ = ptystream.WriteFrame(&exitOnly, ptystream.FrameExit, ptystream.EncodeExit(0))
	if err := relayPTYFrames(closedIngressWriter{}, &exitOnly); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed downstream writer error = %v", err)
	}
	var invalidExit bytes.Buffer
	_ = ptystream.WriteFrame(&invalidExit, ptystream.FrameExit, []byte{1})
	if err := relayPTYFrames(io.Discard, &invalidExit); err == nil || !strings.Contains(err.Error(), "invalid terminal exit frame") {
		t.Fatalf("invalid exit payload error = %v", err)
	}
}

type testUnixDialer struct{}

func (testUnixDialer) DialUnixSocket(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

func TestWorkerIngressProxiesInteractiveUpgradeFrames(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tako-upgrade-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	upstreamSocket := filepath.Join(dir, "takod.sock")
	upstreamListener, err := net.Listen("unix", upstreamSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstreamListener.Close() })
	upstreamDone := make(chan error, 1)
	go func() {
		connection, acceptErr := upstreamListener.Accept()
		if acceptErr != nil {
			upstreamDone <- acceptErr
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		request, readErr := http.ReadRequest(reader)
		if readErr != nil {
			upstreamDone <- readErr
			return
		}
		_, _ = io.ReadAll(request.Body)
		_ = request.Body.Close()
		if request.URL.Path != "/v1/exec" || request.Header.Get("Upgrade") != ptystream.Protocol {
			upstreamDone <- fmt.Errorf("unexpected upgrade request %s %q", request.URL.Path, request.Header.Get("Upgrade"))
			return
		}
		if _, writeErr := fmt.Fprintf(connection, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", ptystream.Protocol); writeErr != nil {
			upstreamDone <- writeErr
			return
		}
		frame, frameErr := ptystream.ReadFrame(reader)
		if frameErr != nil {
			upstreamDone <- frameErr
			return
		}
		writer := ptystream.NewWriter(connection)
		if frame.Type != ptystream.FrameStdin || string(frame.Payload) != "hello" {
			upstreamDone <- fmt.Errorf("unexpected stdin frame %#v", frame)
			return
		}
		if writeErr := writer.WriteFrame(ptystream.FrameStdout, frame.Payload); writeErr != nil {
			upstreamDone <- writeErr
			return
		}
		upstreamDone <- writer.WriteFrame(ptystream.FrameExit, ptystream.EncodeExit(0))
	}()

	upstreamAgent, err := takodclient.NewAgentClient(testUnixDialer{}, upstreamSocket)
	if err != nil {
		t.Fatal(err)
	}
	workerSocket := filepath.Join(dir, "worker.sock")
	workerListener, err := net.Listen("unix", workerSocket)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewOperationStore(dir, "node-test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := NewAdmissionController(DefaultResourcePolicy(), fixedDiskProbe{available: DefaultResourcePolicy().MinimumFreeDiskBytes + 1}, dir)
	if err != nil {
		t.Fatal(err)
	}
	ingress := &workerIngress{
		listener: workerListener, agent: upstreamAgent, store: store, admission: admission, nodeID: "node-test", now: time.Now,
		queueSlots: make(chan struct{}, workerIngressMaxQueuedWaiters), streamSlots: make(chan struct{}, workerIngressMaxStreams), upgradeSlots: make(chan struct{}, workerIngressMaxUpgrades),
	}
	ingress.server = &http.Server{Handler: ingress}
	go func() { _ = ingress.Serve() }()
	t.Cleanup(func() {
		_ = ingress.server.Close()
		upstreamAgent.CloseIdleConnections()
	})

	workerClient, err := takodclient.NewAgentClient(testUnixDialer{}, workerSocket)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := takodclient.UpgradeStream(context.Background(), workerClient, takodclient.DefaultSocket, "/v1/exec", map[string]any{"interactive": true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := ptystream.NewWriter(stream.Conn).WriteFrame(ptystream.FrameStdin, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	stdout, err := ptystream.ReadFrame(stream.Reader)
	if err != nil || stdout.Type != ptystream.FrameStdout || string(stdout.Payload) != "hello" {
		t.Fatalf("stdout frame = %#v, %v", stdout, err)
	}
	exitFrame, err := ptystream.ReadFrame(stream.Reader)
	if err != nil || exitFrame.Type != ptystream.FrameExit {
		t.Fatalf("exit frame = %#v, %v", exitFrame, err)
	}
	if err := <-upstreamDone; err != nil {
		t.Fatal(err)
	}
}
