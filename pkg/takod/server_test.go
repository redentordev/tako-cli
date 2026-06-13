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

func TestHandleCleanupRequiresPost(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/cleanup", nil)
	recorder := httptest.NewRecorder()

	server.handleCleanup(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleCleanupRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/cleanup", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleCleanup(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleAcmeDNSRequiresPost(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/acme-dns", nil)
	recorder := httptest.NewRecorder()

	server.handleAcmeDNS(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleAcmeDNSRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/acme-dns", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleAcmeDNS(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleAcmeDNSCredentialsRequiresSupportedMethod(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/acme-dns/credentials", nil)
	recorder := httptest.NewRecorder()

	server.handleAcmeDNSCredentials(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleAcmeDNSCredentialsRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPut, "/v1/acme-dns/credentials", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleAcmeDNSCredentials(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleStateRequiresSupportedMethod(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodDelete, "/v1/state", nil)
	recorder := httptest.NewRecorder()

	server.handleState(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleStateRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPut, "/v1/state", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleState(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleStateWritesAndReadsDocument(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	writeBody := `{"project":"demo","environment":"production","document":"desired","revisionId":"rev_1","content":"{\"ok\":true}\n"}`
	writeReq := httptest.NewRequest(http.MethodPut, "/v1/state", bytes.NewBufferString(writeBody))
	writeRecorder := httptest.NewRecorder()

	server.handleState(writeRecorder, writeReq)

	if writeRecorder.Code != http.StatusOK {
		t.Fatalf("expected write 200, got %d: %s", writeRecorder.Code, writeRecorder.Body.String())
	}

	readReq := httptest.NewRequest(http.MethodGet, "/v1/state?project=demo&environment=production&document=desired", nil)
	readRecorder := httptest.NewRecorder()
	server.handleState(readRecorder, readReq)

	if readRecorder.Code != http.StatusOK {
		t.Fatalf("expected read 200, got %d: %s", readRecorder.Code, readRecorder.Body.String())
	}
	var response StateDocumentResponse
	if err := json.NewDecoder(readRecorder.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !response.Found || response.Content != "{\"ok\":true}\n" {
		t.Fatalf("unexpected state response: %#v", response)
	}
}

func TestHandleEnvBundleRequiresSupportedMethod(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/env-bundle", nil)
	recorder := httptest.NewRecorder()

	server.handleEnvBundle(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleEnvBundleRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPut, "/v1/env-bundle", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleEnvBundle(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleEnvBundleWritesAndReads(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	writeBody := `{"project":"demo","environment":"production","content":"ZW5jcnlwdGVk"}`
	writeReq := httptest.NewRequest(http.MethodPut, "/v1/env-bundle", bytes.NewBufferString(writeBody))
	writeRecorder := httptest.NewRecorder()

	server.handleEnvBundle(writeRecorder, writeReq)

	if writeRecorder.Code != http.StatusOK {
		t.Fatalf("expected write 200, got %d: %s", writeRecorder.Code, writeRecorder.Body.String())
	}

	readReq := httptest.NewRequest(http.MethodGet, "/v1/env-bundle?project=demo&environment=production", nil)
	readRecorder := httptest.NewRecorder()
	server.handleEnvBundle(readRecorder, readReq)

	if readRecorder.Code != http.StatusOK {
		t.Fatalf("expected read 200, got %d: %s", readRecorder.Code, readRecorder.Body.String())
	}
	var response EnvBundleResponse
	if err := json.NewDecoder(readRecorder.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !response.Found || response.Content != "ZW5jcnlwdGVk" {
		t.Fatalf("unexpected env bundle response: %#v", response)
	}
}

func TestHandleBackupsRequiresSupportedMethod(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPut, "/v1/backups", nil)
	recorder := httptest.NewRecorder()

	server.handleBackups(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleBackupsRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/backups", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleBackups(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleBackupsListsBackups(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()

	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	request := BackupRequest{Project: "demo", Environment: "production"}
	dir := backupDirectory(request)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatalf("failed to create backup dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data_20240101-120000.tar.gz"), []byte("backup"), 0600); err != nil {
		t.Fatalf("failed to write backup fixture: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/backups?project=demo&environment=production", nil)
	recorder := httptest.NewRecorder()
	server.handleBackups(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response BackupListResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(response.Backups) != 1 || response.Backups[0].Volume != "data" {
		t.Fatalf("unexpected backups response: %#v", response)
	}
}

func TestHandleBackupRestoreRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/backups/restore", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleBackupRestore(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleBackupCleanupRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/backups/cleanup", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleBackupCleanup(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}
