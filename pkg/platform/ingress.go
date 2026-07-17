package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/takoapi/ptystream"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

const (
	workerIngressMaxJSONBytes     = 16 << 20
	workerIngressPollInterval     = 50 * time.Millisecond
	workerIngressJSONTimeout      = 30 * time.Second
	workerIngressStreamTimeout    = 30 * time.Minute
	workerIngressMaxConnections   = 256
	workerIngressMaxQueuedWaiters = 64
	workerIngressMaxStreams       = 8
	workerIngressMaxUpgrades      = 16
)

type streamingRuntimeAgent interface {
	StreamOutput(context.Context, string, string, io.Reader, string, io.Writer, io.Writer) error
}

type workerIngress struct {
	listener     net.Listener
	server       *http.Server
	store        *OperationStore
	agent        RuntimeAgent
	nodeID       string
	now          func() time.Time
	journal      *Journal
	admission    *AdmissionController
	queueSlots   chan struct{}
	streamSlots  chan struct{}
	upgradeSlots chan struct{}
	fatalErr     chan error
	fatalOnce    sync.Once
}

type ingressResponseWriter struct {
	http.ResponseWriter
	written int64
}

func (w *ingressResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.written += int64(n)
	return n, err
}

func newWorkerIngress(socketPath string, socketGroupGID int, store *OperationStore, agent RuntimeAgent, admission *AdmissionController, stateDir string, nodeID string, now func() time.Time) (*workerIngress, error) {
	if !filepath.IsAbs(socketPath) || socketGroupGID <= 0 || store == nil || agent == nil || admission == nil {
		return nil, fmt.Errorf("worker ingress requires an absolute socket, access group, store, agent, and admission controller")
	}
	dir := filepath.Dir(socketPath)
	if err := secureWorkerRuntimeDir(dir, socketGroupGID); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("worker ingress path %s exists and is not a Unix socket", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("remove stale worker ingress socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect worker ingress socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on worker ingress socket: %w", err)
	}
	cleanup := func(cause error) (*workerIngress, error) {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, cause
	}
	if err := os.Chmod(socketPath, 0660); err != nil {
		return cleanup(fmt.Errorf("secure worker ingress socket: %w", err))
	}
	if err := os.Chown(socketPath, os.Geteuid(), socketGroupGID); err != nil {
		return cleanup(fmt.Errorf("assign worker ingress socket access group: %w", err))
	}
	ingress := &workerIngress{
		listener: newLimitedListener(listener, workerIngressMaxConnections), store: store, agent: agent, admission: admission, nodeID: nodeID, now: now,
		journal:      func() *Journal { journal, _ := NewJournal(filepath.Join(stateDir, DefaultJournalName)); return journal }(),
		queueSlots:   make(chan struct{}, workerIngressMaxQueuedWaiters),
		streamSlots:  make(chan struct{}, workerIngressMaxStreams),
		upgradeSlots: make(chan struct{}, workerIngressMaxUpgrades),
		fatalErr:     make(chan error, 1),
	}
	ingress.server = &http.Server{Handler: ingress, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10}
	return ingress, nil
}

func secureWorkerRuntimeDir(dir string, socketGroupGID int) error {
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("worker runtime path %s is not a directory", dir)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("create worker runtime directory: %w", err)
		}
	} else {
		return fmt.Errorf("inspect worker runtime directory: %w", err)
	}
	if err := os.Chmod(dir, 0750); err != nil {
		return fmt.Errorf("secure worker runtime directory: %w", err)
	}
	if err := os.Chown(dir, os.Geteuid(), socketGroupGID); err != nil {
		return fmt.Errorf("assign worker runtime directory access group: %w", err)
	}
	return nil
}

func (i *workerIngress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.IsAbs() || r.URL.Host != "" || !strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "//") {
		http.Error(w, "invalid runtime endpoint", http.StatusBadRequest)
		return
	}
	endpoint := r.URL.RequestURI()
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		_ = i.proxyStream(w, r, endpoint)
	case http.MethodPost, http.MethodPut, http.MethodDelete:
		if r.URL.Path == "/v1/exec" && isIngressUpgrade(r) {
			i.handleUpgrade(w, r, endpoint)
			return
		}
		if isDirectStreamMutation(r.URL.Path) {
			i.handleStreamMutation(w, r, endpoint)
			return
		}
		i.queueMutation(w, r, endpoint)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func isIngressUpgrade(r *http.Request) bool {
	if r == nil || !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), ptystream.Protocol) {
		return false
	}
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

func (i *workerIngress) proxyUpgrade(w http.ResponseWriter, r *http.Request, endpoint string) (bool, error) {
	dialer, ok := i.agent.(takodclient.UnixSocketDialer)
	if !ok {
		http.Error(w, "runtime protocol upgrades are unavailable", http.StatusBadGateway)
		return false, fmt.Errorf("runtime protocol upgrades are unavailable")
	}
	clearDeadline := setIngressReadDeadline(w, workerIngressJSONTimeout)
	body, err := readIngressJSON(r.Body)
	clearDeadline()
	if err != nil || len(body) == 0 || !json.Valid(body) {
		if err == nil {
			err = fmt.Errorf("upgrade request body must be valid JSON")
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false, err
	}
	upstream, err := takodclient.UpgradeStream(r.Context(), dialer, takodclient.DefaultSocket, endpoint, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		i.audit("runtime.upgrade", endpoint, "failed", err.Error())
		return false, err
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "runtime ingress does not support protocol upgrades", http.StatusInternalServerError)
		return true, fmt.Errorf("runtime ingress does not support protocol upgrades")
	}
	downstream, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return true, err
	}
	if _, err := fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", ptystream.Protocol); err != nil {
		_ = downstream.Close()
		_ = upstream.Close()
		return true, err
	}
	if err := buffered.Flush(); err != nil {
		_ = downstream.Close()
		_ = upstream.Close()
		return true, err
	}

	stop := context.AfterFunc(r.Context(), func() {
		_ = downstream.Close()
		_ = upstream.Close()
	})
	defer stop()
	type relayResult struct {
		direction string
		err       error
	}
	relays := make(chan relayResult, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		relays <- relayResult{direction: "upstream-to-client", err: relayPTYFrames(downstream, upstream.Reader)}
		_ = downstream.Close()
		_ = upstream.Close()
	}()
	go func() {
		defer wg.Done()
		_, copyErr := io.Copy(upstream.Conn, buffered.Reader)
		relays <- relayResult{direction: "client-to-upstream", err: copyErr}
		if closeWriter, ok := upstream.Conn.(interface{ CloseWrite() error }); ok {
			_ = closeWriter.CloseWrite()
		}
	}()
	wg.Wait()
	close(relays)
	var relayErrors []string
	upstreamCompleted := false
	var clientRelayErr error
	for result := range relays {
		if result.direction == "upstream-to-client" {
			if result.err == nil {
				upstreamCompleted = true
			} else {
				relayErrors = append(relayErrors, result.direction+": "+result.err.Error())
			}
		} else {
			clientRelayErr = result.err
		}
	}
	if !upstreamCompleted && len(relayErrors) == 0 {
		relayErrors = append(relayErrors, "upstream did not deliver a terminal exit frame")
	}
	if clientRelayErr != nil && !(upstreamCompleted && benignRelayError(clientRelayErr)) {
		relayErrors = append(relayErrors, "client-to-upstream: "+clientRelayErr.Error())
	}
	if len(relayErrors) > 0 {
		err := fmt.Errorf("interactive relay failed: %s", strings.Join(relayErrors, "; "))
		i.audit("runtime.upgrade", endpoint, "failed", err.Error())
		return true, err
	}
	i.audit("runtime.upgrade", endpoint, "completed", "full-duplex runtime stream completed")
	return true, nil
}

func relayPTYFrames(destination io.Writer, source io.Reader) error {
	for {
		frame, err := ptystream.ReadFrame(source)
		if err != nil {
			// Do not wrap EOF/net.ErrClosed: proxyUpgrade treats those as benign
			// only for the client-input half of the duplex relay. On the framed
			// upstream half, either condition before Exit is an ambiguous outcome.
			return fmt.Errorf("upstream closed without a terminal exit frame: %v", err)
		}
		if frame.Type == ptystream.FrameExit {
			if _, err := ptystream.DecodeExit(frame.Payload); err != nil {
				return fmt.Errorf("invalid terminal exit frame: %v", err)
			}
		}
		if err := ptystream.WriteFrame(destination, frame.Type, frame.Payload); err != nil {
			return err
		}
		if frame.Type == ptystream.FrameExit {
			return nil
		}
	}
}

func benignRelayError(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled)
}

func isDirectStreamMutation(path string) bool {
	switch path {
	case "/v1/images/build", "/v1/images/import", "/v1/exec", "/v1/jobs/trigger":
		return true
	default:
		return false
	}
}

func (i *workerIngress) proxyStream(w http.ResponseWriter, r *http.Request, endpoint string) error {
	agent, ok := i.agent.(streamingRuntimeAgent)
	if !ok {
		http.Error(w, "runtime streaming is unavailable", http.StatusBadGateway)
		return fmt.Errorf("runtime streaming is unavailable")
	}
	contentType := r.Header.Get("Content-Type")
	if r.URL.Path == "/v1/images/export" {
		w.Header().Set("Content-Type", "application/octet-stream")
	} else if strings.HasPrefix(r.URL.Path, "/v1/images/") {
		w.Header().Set("Content-Type", "application/json")
	}
	var errorBody bytes.Buffer
	tracked := &ingressResponseWriter{ResponseWriter: w}
	err := agent.StreamOutput(r.Context(), r.Method, endpoint, r.Body, contentType, tracked, &errorBody)
	if err == nil {
		i.audit("runtime.stream", endpoint, "completed", "structured stream completed")
		return nil
	}
	// Once upstream response bytes have been relayed, emitting a second HTTP
	// status/body would corrupt the structured stream. Closing the response is
	// the only safe signal; the client observes the truncated body/error.
	if tracked.written == 0 {
		var httpErr *takodclient.HTTPError
		if errors.As(err, &httpErr) {
			http.Error(w, strings.TrimSpace(httpErr.Body), httpErr.Status)
		} else {
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
	}
	i.audit("runtime.stream", endpoint, "failed", err.Error())
	return err
}

func (i *workerIngress) handleStreamMutation(w http.ResponseWriter, r *http.Request, endpoint string) {
	if release, ok := acquireIngressSlot(i.streamSlots); !ok {
		http.Error(w, "runtime stream concurrency limit reached", http.StatusTooManyRequests)
		return
	} else {
		defer release()
	}
	clearDeadline := setIngressReadDeadline(w, workerIngressStreamTimeout)
	defer clearDeadline()
	spec, err := i.startStreamOperation(r.Method, endpoint)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	plan, err := (AgentOperationExecutor{}).Plan(spec)
	if err != nil {
		if persistErr := i.store.complete(spec.ID, OperationResult{}, err); persistErr != nil {
			i.reportPersistenceFailure(endpoint, persistErr)
			http.Error(w, "failed to persist runtime operation outcome", http.StatusInternalServerError)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	releaseAdmission, err := i.admission.Admit(r.Context(), plan.OperationEffect)
	if err != nil {
		if persistErr := i.store.complete(spec.ID, OperationResult{}, err); persistErr != nil {
			i.reportPersistenceFailure(endpoint, persistErr)
			http.Error(w, "failed to persist runtime operation outcome", http.StatusInternalServerError)
			return
		}
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer releaseAdmission()
	err = i.proxyStream(w, r, endpoint)
	if err == nil {
		if persistErr := i.store.complete(spec.ID, OperationResult{Status: http.StatusOK}, nil); persistErr != nil {
			i.reportPersistenceFailure(endpoint, persistErr)
		}
		return
	}
	var httpErr *takodclient.HTTPError
	if errors.As(err, &httpErr) {
		if persistErr := i.store.complete(spec.ID, OperationResult{Body: []byte(httpErr.Body), Status: httpErr.Status, KnownFailure: true}, nil); persistErr != nil {
			i.reportPersistenceFailure(endpoint, persistErr)
		}
		return
	}
	if persistErr := i.store.markAmbiguous(spec.ID, "streamed operation ended after dispatch with uncertain outcome: "+err.Error()); persistErr != nil {
		i.reportPersistenceFailure(endpoint, persistErr)
	}
}

func (i *workerIngress) handleUpgrade(w http.ResponseWriter, r *http.Request, endpoint string) {
	if release, ok := acquireIngressSlot(i.upgradeSlots); !ok {
		http.Error(w, "interactive runtime concurrency limit reached", http.StatusTooManyRequests)
		return
	} else {
		defer release()
	}
	spec, err := i.startStreamOperation(r.Method, endpoint)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	releaseAdmission, err := i.admission.Admit(r.Context(), OperationEffect{DiskGrowth: true})
	if err != nil {
		if persistErr := i.store.complete(spec.ID, OperationResult{}, err); persistErr != nil {
			i.reportPersistenceFailure(endpoint, persistErr)
			http.Error(w, "failed to persist runtime operation outcome", http.StatusInternalServerError)
			return
		}
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer releaseAdmission()
	dispatched, upgradeErr := i.proxyUpgrade(w, r, endpoint)
	if upgradeErr == nil {
		if persistErr := i.store.complete(spec.ID, OperationResult{Status: http.StatusSwitchingProtocols}, nil); persistErr != nil {
			i.reportPersistenceFailure(endpoint, persistErr)
		}
	} else if dispatched {
		if persistErr := i.store.markAmbiguous(spec.ID, "interactive operation ended after dispatch with uncertain outcome: "+upgradeErr.Error()); persistErr != nil {
			i.reportPersistenceFailure(endpoint, persistErr)
		}
	} else {
		if persistErr := i.store.complete(spec.ID, OperationResult{}, upgradeErr); persistErr != nil {
			i.reportPersistenceFailure(endpoint, persistErr)
		}
	}
}

func (i *workerIngress) startStreamOperation(method string, endpoint string) (OperationSpec, error) {
	payload, err := json.Marshal(AgentRequestPayload{Method: method, Endpoint: endpoint})
	if err != nil {
		return OperationSpec{}, err
	}
	id, err := newIngressOperationID(i.now())
	if err != nil {
		return OperationSpec{}, err
	}
	spec := OperationSpec{APIVersion: APIVersion, ID: id, Kind: "agent.stream", Payload: payload, CreatedAt: i.now().UTC()}
	return spec, i.store.startExternal(spec)
}

func (i *workerIngress) queueMutation(w http.ResponseWriter, r *http.Request, endpoint string) {
	if release, ok := acquireIngressSlot(i.queueSlots); !ok {
		http.Error(w, "runtime request concurrency limit reached", http.StatusTooManyRequests)
		return
	} else {
		defer release()
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if contentType != "" && contentType != "application/json" {
		http.Error(w, "structured mutations require application/json", http.StatusUnsupportedMediaType)
		return
	}
	clearDeadline := setIngressReadDeadline(w, workerIngressJSONTimeout)
	body, err := readIngressJSON(r.Body)
	clearDeadline()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > 0 && !json.Valid(body) {
		http.Error(w, "request body must be valid JSON", http.StatusBadRequest)
		return
	}
	operationID, err := newIngressOperationID(i.now())
	if err != nil {
		http.Error(w, "failed to allocate operation identity", http.StatusInternalServerError)
		return
	}
	payload, err := json.Marshal(AgentRequestPayload{Method: r.Method, Endpoint: endpoint, Body: body})
	if err != nil {
		http.Error(w, "failed to encode durable operation", http.StatusInternalServerError)
		return
	}
	spec := OperationSpec{APIVersion: APIVersion, ID: operationID, Kind: "agent.request", Payload: payload, CreatedAt: i.now().UTC()}
	if err := i.store.Enqueue(spec); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	ticker := time.NewTicker(workerIngressPollInterval)
	defer ticker.Stop()
	for {
		state, readErr := i.store.Read(operationID)
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusInternalServerError)
			return
		}
		switch state.Status {
		case "succeeded":
			contentType := state.ResponseContentType
			if contentType == "" {
				contentType = "application/json"
			}
			w.Header().Set("Content-Type", contentType)
			status := state.ResponseStatus
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			if state.Response == "" {
				_, _ = io.WriteString(w, "{}\n")
			} else {
				_, _ = io.WriteString(w, state.Response)
			}
			return
		case "failed":
			if state.ResponseStatus > 0 {
				contentType := state.ResponseContentType
				if contentType == "" {
					contentType = "text/plain; charset=utf-8"
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(state.ResponseStatus)
				_, _ = io.WriteString(w, state.Response)
				return
			}
			http.Error(w, state.LastError, http.StatusBadGateway)
			return
		case "ambiguous":
			http.Error(w, state.LastError, http.StatusConflict)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func acquireIngressSlot(slots chan struct{}) (func(), bool) {
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, true
	default:
		return nil, false
	}
}

func setIngressReadDeadline(w http.ResponseWriter, timeout time.Duration) func() {
	if timeout <= 0 {
		return func() {}
	}
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return func() {}
	}
	return func() { _ = controller.SetReadDeadline(time.Time{}) }
}

type limitedListener struct {
	net.Listener
	slots chan struct{}
}

func newLimitedListener(listener net.Listener, maximum int) net.Listener {
	return &limitedListener{Listener: listener, slots: make(chan struct{}, maximum)}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.slots <- struct{}{}
	return &limitedConn{Conn: connection, release: func() { <-l.slots }}, nil
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func readIngressJSON(reader io.Reader) (json.RawMessage, error) {
	if reader == nil {
		return nil, nil
	}
	limited := io.LimitReader(reader, workerIngressMaxJSONBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read structured mutation: %w", err)
	}
	if len(data) > workerIngressMaxJSONBytes {
		return nil, fmt.Errorf("structured mutation exceeds %d bytes", workerIngressMaxJSONBytes)
	}
	data = bytes.TrimSpace(data)
	return json.RawMessage(data), nil
}

func newIngressOperationID(now time.Time) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("runtime-%d-%s", now.UnixNano(), hex.EncodeToString(random[:])), nil
}

func (i *workerIngress) audit(operation string, endpoint string, status string, message string) {
	if i.journal == nil {
		return
	}
	_ = i.journal.Append(OperationRecord{
		OperationID: operation + "-" + fmt.Sprint(i.now().UnixNano()), Operation: operation,
		Phase: endpoint, Status: status, NodeID: i.nodeID, Message: message, Timestamp: i.now().UTC(),
	})
}

func (i *workerIngress) reportPersistenceFailure(endpoint string, err error) {
	if err == nil {
		return
	}
	wrapped := fmt.Errorf("persist terminal runtime operation for %s: %w", endpoint, err)
	log.Printf("platform audit: operation=runtime.persistence phase=%s status=failed error=%v", endpoint, err)
	i.audit("runtime.persistence", endpoint, "failed", err.Error())
	i.fatalOnce.Do(func() {
		if i.fatalErr != nil {
			i.fatalErr <- wrapped
		}
	})
}

func (i *workerIngress) Serve() error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- i.server.Serve(i.listener) }()
	if i.fatalErr == nil {
		return <-serveErr
	}
	select {
	case err := <-serveErr:
		return err
	case err := <-i.fatalErr:
		_ = i.server.Close()
		return err
	}
}

func (i *workerIngress) Shutdown(ctx context.Context) error {
	return i.server.Shutdown(ctx)
}
