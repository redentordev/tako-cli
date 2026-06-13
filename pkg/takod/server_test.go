package takod

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerStatusOverUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "takod-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "takod.sock")
	dataDir := filepath.Join(dir, "data")

	server := NewServer(socket, dataDir, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
			t.Fatal("takod server did not stop")
		}
	})

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
	}

	var lastErr error
	for i := 0; i < 20; i++ {
		resp, err := client.Get("http://takod/v1/status")
		if err == nil {
			defer resp.Body.Close()
			var status Status
			if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
				t.Fatalf("failed to decode status: %v", err)
			}
			if status.Runtime != "takod" {
				t.Fatalf("unexpected runtime %q", status.Runtime)
			}
			if status.Version != "test" {
				t.Fatalf("unexpected version %q", status.Version)
			}
			return
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}

	t.Fatalf("takod status was not reachable: %v", lastErr)
}

func TestRemoveStaleSocketRejectsNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "takod.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0600); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	if err := removeStaleSocket(path); err == nil {
		t.Fatal("expected non-socket path to be rejected")
	}
}

func TestHandleActualRequiresProjectAndEnvironment(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/actual", nil)
	recorder := httptest.NewRecorder()

	server.handleActual(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleReconcileServiceRequiresPost(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/reconcile-service", nil)
	recorder := httptest.NewRecorder()

	server.handleReconcileService(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleReconcileServiceRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/reconcile-service", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleReconcileService(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleProxyFileRequiresSupportedMethod(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy-file", nil)
	recorder := httptest.NewRecorder()

	server.handleProxyFile(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleProxyFileRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPut, "/v1/proxy-file", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleProxyFile(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleProxyRequiresPost(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/proxy", nil)
	recorder := httptest.NewRecorder()

	server.handleProxy(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleProxyRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleProxy(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}
