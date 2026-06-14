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
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/mesh"
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

func TestNewTakodHTTPServerSetsHeaderTimeout(t *testing.T) {
	server := newTakodHTTPServer(http.NewServeMux())

	if server.ReadHeaderTimeout != takodReadHeaderTimeout {
		t.Fatalf("ReadHeaderTimeout = %s, want %s", server.ReadHeaderTimeout, takodReadHeaderTimeout)
	}
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

func TestHandleEnvBundleRejectsOversizedJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	body := `{"project":"demo","environment":"production","content":"` + strings.Repeat("A", takodMaxJSONBodyBytes) + `"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/env-bundle", bytes.NewBufferString(body))
	recorder := httptest.NewRecorder()

	server.handleEnvBundle(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "request body too large") {
		t.Fatalf("response = %q, want body-size error", recorder.Body.String())
	}
}

func TestHandleEnvBundleRejectsMultipleJSONValues(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPut, "/v1/env-bundle", bytes.NewBufferString(`{} {}`))
	recorder := httptest.NewRecorder()

	server.handleEnvBundle(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "single JSON value") {
		t.Fatalf("response = %q, want single-value error", recorder.Body.String())
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
	if response.UpdatedAt.IsZero() {
		t.Fatal("expected env bundle response to include UpdatedAt")
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

func TestHandleMetadataRequiresPut(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/metadata", nil)
	recorder := httptest.NewRecorder()

	server.handleMetadata(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleMetadataRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPut, "/v1/metadata", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleMetadata(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleMetadataWritesDocuments(t *testing.T) {
	dataDir := t.TempDir()
	server := NewServer("/tmp/takod-test.sock", dataDir, "test")
	req := httptest.NewRequest(http.MethodPut, "/v1/metadata", bytes.NewBufferString(`{"node":{"node":"node-a"},"peers":{"peers":[]}}`))
	recorder := httptest.NewRecorder()

	server.handleMetadata(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "node.json")); err != nil {
		t.Fatalf("expected node metadata to be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "mesh", "peers.json")); err != nil {
		t.Fatalf("expected peer metadata to be written: %v", err)
	}
}

func TestHandleMeshKeyRequiresPost(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/mesh/key", nil)
	recorder := httptest.NewRecorder()

	server.handleMeshKey(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleMeshKeyReturnsPublicKey(t *testing.T) {
	old := ensureMeshNodeKey
	ensureMeshNodeKey = func(ctx context.Context, verbose bool) (string, error) {
		return "node-public-key", nil
	}
	t.Cleanup(func() { ensureMeshNodeKey = old })

	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/mesh/key", nil)
	recorder := httptest.NewRecorder()

	server.handleMeshKey(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response MeshKeyResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if response.PublicKey != "node-public-key" {
		t.Fatalf("unexpected public key response: %#v", response)
	}
}

func TestHandleMeshApplyRejectsInvalidJSON(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/mesh/apply", bytes.NewBufferString("{"))
	recorder := httptest.NewRecorder()

	server.handleMeshApply(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleMeshApplyReconcilesMesh(t *testing.T) {
	old := applyMeshConfig
	applyMeshConfig = func(ctx context.Context, node mesh.Node, peers []mesh.Node, config mesh.WireGuardConfig, verbose bool) (*mesh.Status, error) {
		if node.Name != "node-a" {
			t.Fatalf("unexpected node: %#v", node)
		}
		if len(peers) != 1 || peers[0].Name != "node-b" {
			t.Fatalf("unexpected peers: %#v", peers)
		}
		if config.Interface != "tako" || config.ListenPort != 51820 {
			t.Fatalf("unexpected config: %#v", config)
		}
		return &mesh.Status{Interface: "tako", Up: true, PublicKey: "node-public-key", ListenPort: "51820", Peers: 1}, nil
	}
	t.Cleanup(func() { applyMeshConfig = old })

	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	body, err := json.Marshal(MeshApplyRequest{
		Config: mesh.WireGuardConfig{Enabled: true, Interface: "tako", ListenPort: 51820, NATTraversal: true},
		Node:   mesh.Node{Name: "node-a", Host: "203.0.113.10", Address: "10.210.0.1/24"},
		Peers:  []mesh.Node{{Name: "node-b", Host: "203.0.113.11", Address: "10.210.0.2/24", PublicKey: "peer-key"}},
	})
	if err != nil {
		t.Fatalf("failed to encode request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/mesh/apply", bytes.NewReader(body))
	recorder := httptest.NewRecorder()

	server.handleMeshApply(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response MeshApplyResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !response.Applied || response.Status == nil || response.Status.PublicKey != "node-public-key" {
		t.Fatalf("unexpected mesh apply response: %#v", response)
	}
}

func TestHandleMeshStatusRequiresInterface(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/mesh/status", nil)
	recorder := httptest.NewRecorder()

	server.handleMeshStatus(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleImageExistsRequiresGet(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/images/exists?image=demo/web:abc", nil)
	recorder := httptest.NewRecorder()

	server.handleImageExists(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleImageExportRequiresImage(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/images/export", nil)
	recorder := httptest.NewRecorder()

	server.handleImageExport(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleImageImportRequiresPost(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/images/import?image=demo/web:abc", nil)
	recorder := httptest.NewRecorder()

	server.handleImageImport(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleImageBuildRequiresPost(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/images/build?image=demo/web:abc", nil)
	recorder := httptest.NewRecorder()

	server.handleImageBuild(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleImageBuildRequiresImage(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/images/build", bytes.NewBufferString(""))
	recorder := httptest.NewRecorder()

	server.handleImageBuild(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleLogsRequiresGet(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", nil)
	recorder := httptest.NewRecorder()

	server.handleLogs(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleLogsRejectsInvalidTail(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/logs?project=demo&environment=production&service=web&tail=bad", nil)
	recorder := httptest.NewRecorder()

	server.handleLogs(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleLogsStreamsServiceLogs(t *testing.T) {
	logPath := t.TempDir() + "/commands.log"
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo_production_web_1\n")

	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/logs?project=demo&environment=production&service=web&tail=10", nil)
	recorder := httptest.NewRecorder()

	server.handleLogs(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != "logs\n" {
		t.Fatalf("unexpected log body: %q", recorder.Body.String())
	}
}

func TestHandleStatsRequiresGet(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/stats", nil)
	recorder := httptest.NewRecorder()

	server.handleStats(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleStatsRejectsInvalidAll(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/stats?all=maybe", nil)
	recorder := httptest.NewRecorder()

	server.handleStats(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleStatsReturnsContainerStats(t *testing.T) {
	logPath := t.TempDir() + "/commands.log"
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo_production_web_1\n")
	t.Setenv("TAKO_FAKE_STATS_OUTPUT", `{"Name":"demo_production_web_1","CPUPerc":"1.23%"}`+"\n")

	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/stats?project=demo&environment=production&service=web", nil)
	recorder := httptest.NewRecorder()

	server.handleStats(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response StatsResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode stats response: %v", err)
	}
	if len(response.Stats) != 1 || response.Stats[0].CPUPercent != "1.23%" {
		t.Fatalf("unexpected stats response: %#v", response)
	}
}

func TestHandleMetricsRequiresGet(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", nil)
	recorder := httptest.NewRecorder()

	server.handleMetrics(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleMetricsRejectsInvalidCollect(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/metrics?collect=maybe", nil)
	recorder := httptest.NewRecorder()

	server.handleMetrics(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleMetricsReturnsNodeMetrics(t *testing.T) {
	restore := useTempMetricsFile(t, `{"cpu_percent":"12.5","uptime_seconds":123}`+"\n")
	defer restore()

	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	recorder := httptest.NewRecorder()

	server.handleMetrics(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response MetricsResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode metrics response: %v", err)
	}
	var metrics map[string]any
	if err := json.Unmarshal(response.Metrics, &metrics); err != nil {
		t.Fatalf("failed to decode nested metrics: %v", err)
	}
	if metrics["cpu_percent"] != "12.5" {
		t.Fatalf("unexpected metrics response: %#v", metrics)
	}
}

func TestHandleImageImportRejectsOversizedContentLength(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/images/import?image=demo:latest", strings.NewReader(""))
	req.ContentLength = defaultImageImportMaxBytes + 1
	recorder := httptest.NewRecorder()

	server.handleImageImport(recorder, req)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "image import exceeds maximum size") {
		t.Fatalf("response = %q, want size limit context", recorder.Body.String())
	}
}

func TestHandleAccessLogsRequiresGet(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodPost, "/v1/access-logs", nil)
	recorder := httptest.NewRecorder()

	server.handleAccessLogs(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
}

func TestHandleAccessLogsRejectsInvalidTail(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/access-logs?tail=bad", nil)
	recorder := httptest.NewRecorder()

	server.handleAccessLogs(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestHandleAccessLogsStreamsTail(t *testing.T) {
	restore := useTempAccessLog(t, "one\ntwo\nthree\n")
	defer restore()

	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/access-logs?tail=2", nil)
	recorder := httptest.NewRecorder()

	server.handleAccessLogs(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != "two\nthree\n" {
		t.Fatalf("unexpected access log response: %q", recorder.Body.String())
	}
}
