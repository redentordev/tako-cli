package takod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Server struct {
	socket    string
	dataDir   string
	version   string
	startedAt time.Time
	server    *http.Server
	mu        sync.Mutex
}

type Status struct {
	Runtime   string         `json:"runtime"`
	Version   string         `json:"version"`
	Hostname  string         `json:"hostname"`
	Socket    string         `json:"socket"`
	DataDir   string         `json:"dataDir"`
	StartedAt time.Time      `json:"startedAt"`
	Now       time.Time      `json:"now"`
	Node      map[string]any `json:"node,omitempty"`
	Peers     map[string]any `json:"peers,omitempty"`
}

func NewServer(socket string, dataDir string, version string) *Server {
	return &Server{
		socket:    socket,
		dataDir:   dataDir,
		version:   version,
		startedAt: time.Now().UTC(),
	}
}

func (s *Server) Run(ctx context.Context) error {
	if s.socket == "" {
		return fmt.Errorf("socket path is required")
	}
	if s.dataDir == "" {
		return fmt.Errorf("data directory is required")
	}

	if err := os.MkdirAll(filepath.Dir(s.socket), 0755); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}
	if err := removeStaleSocket(s.socket); err != nil {
		return fmt.Errorf("failed to remove stale socket: %w", err)
	}

	listener, err := net.Listen("unix", s.socket)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.socket, err)
	}
	if err := os.Chmod(s.socket, 0660); err != nil {
		listener.Close()
		return fmt.Errorf("failed to chmod socket: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/actual", s.handleActual)
	mux.HandleFunc("/v1/reconcile-service", s.handleReconcileService)
	mux.HandleFunc("/v1/proxy-file", s.handleProxyFile)
	mux.HandleFunc("/v1/proxy", s.handleProxy)
	mux.HandleFunc("/v1/cleanup", s.handleCleanup)
	mux.HandleFunc("/v1/acme-dns", s.handleAcmeDNS)
	mux.HandleFunc("/v1/acme-dns/credentials", s.handleAcmeDNSCredentials)
	mux.HandleFunc("/v1/state", s.handleState)
	mux.HandleFunc("/v1/env-bundle", s.handleEnvBundle)
	mux.HandleFunc("/v1/backups", s.handleBackups)
	mux.HandleFunc("/v1/backups/restore", s.handleBackupRestore)
	mux.HandleFunc("/v1/backups/cleanup", s.handleBackupCleanup)
	mux.HandleFunc("/v1/metadata", s.handleMetadata)

	httpServer := &http.Server{Handler: mux}
	s.mu.Lock()
	s.server = httpServer
	s.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		_ = os.Remove(s.socket)
		return ctx.Err()
	case err := <-errCh:
		_ = os.Remove(s.socket)
		return err
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}` + "\n"))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := s.Status()
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(status)
}

func (s *Server) handleActual(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	project := r.URL.Query().Get("project")
	environment := r.URL.Query().Get("environment")
	if project == "" || environment == "" {
		http.Error(w, "project and environment are required", http.StatusBadRequest)
		return
	}
	actual, err := GatherActualState(r.Context(), project, environment)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(actual)
}

func (s *Server) handleReconcileService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request ReconcileServiceRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	response, err := ReconcileService(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleProxyFile(w http.ResponseWriter, r *http.Request) {
	var (
		response *ProxyFileResponse
		err      error
	)

	switch r.Method {
	case http.MethodPut:
		defer r.Body.Close()
		var request ProxyFileRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err = WriteProxyFile(r.Context(), request)
	case http.MethodDelete:
		response, err = RemoveProxyFile(r.Context(), r.URL.Query().Get("name"))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request ReconcileProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	response, err := ReconcileProxy(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request CleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	response, err := CleanupProject(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleAcmeDNS(w http.ResponseWriter, r *http.Request) {
	var (
		response *ReconcileAcmeDNSResponse
		err      error
	)
	switch r.Method {
	case http.MethodPost:
		defer r.Body.Close()
		var request ReconcileAcmeDNSRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err = ReconcileAcmeDNS(r.Context(), request)
	case http.MethodDelete:
		response, err = RemoveAcmeDNS(r.Context())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleAcmeDNSCredentials(w http.ResponseWriter, r *http.Request) {
	var (
		response *AcmeDNSCredentialsResponse
		err      error
	)
	switch r.Method {
	case http.MethodGet:
		response, err = ReadAcmeDNSCredentials()
	case http.MethodPut:
		defer r.Body.Close()
		var request AcmeDNSCredentialsRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err = WriteAcmeDNSCredentials(request)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	var (
		response *StateDocumentResponse
		err      error
	)

	switch r.Method {
	case http.MethodGet:
		response, err = ReadStateDocument(r.Context(), s.dataDir, StateDocumentRequest{
			Project:     r.URL.Query().Get("project"),
			Environment: r.URL.Query().Get("environment"),
			Document:    r.URL.Query().Get("document"),
		})
	case http.MethodPut:
		defer r.Body.Close()
		var request StateDocumentRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err = WriteStateDocument(r.Context(), s.dataDir, request)
	case http.MethodPost:
		defer r.Body.Close()
		var request StateDocumentRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err = AppendStateEvent(r.Context(), s.dataDir, request)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleEnvBundle(w http.ResponseWriter, r *http.Request) {
	var (
		response *EnvBundleResponse
		err      error
	)

	switch r.Method {
	case http.MethodGet:
		response, err = ReadEnvBundle(r.Context(), s.dataDir, EnvBundleRequest{
			Project:     r.URL.Query().Get("project"),
			Environment: r.URL.Query().Get("environment"),
		})
	case http.MethodPut:
		defer r.Body.Close()
		var request EnvBundleRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err = WriteEnvBundle(r.Context(), s.dataDir, request)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	var (
		response any
		err      error
	)

	switch r.Method {
	case http.MethodGet:
		response, err = ListVolumeBackups(r.Context(), BackupRequest{
			Project:     r.URL.Query().Get("project"),
			Environment: r.URL.Query().Get("environment"),
			Volume:      r.URL.Query().Get("volume"),
		})
	case http.MethodPost:
		defer r.Body.Close()
		var request BackupRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err = CreateVolumeBackup(r.Context(), request)
	case http.MethodDelete:
		err = DeleteVolumeBackup(r.Context(), BackupRequest{
			Project:     r.URL.Query().Get("project"),
			Environment: r.URL.Query().Get("environment"),
			Volume:      r.URL.Query().Get("volume"),
			BackupID:    r.URL.Query().Get("backupId"),
		})
		response = map[string]bool{"deleted": err == nil}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request BackupRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := RestoreVolumeBackup(r.Context(), request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(map[string]bool{"restored": true})
}

func (s *Server) handleBackupCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request BackupRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	response, err := CleanupOldBackups(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request MetadataRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	response, err := WriteMetadata(r.Context(), s.dataDir, request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) Status() Status {
	hostname, _ := os.Hostname()
	status := Status{
		Runtime:   "takod",
		Version:   s.version,
		Hostname:  hostname,
		Socket:    s.socket,
		DataDir:   s.dataDir,
		StartedAt: s.startedAt,
		Now:       time.Now().UTC(),
	}

	status.Node = readJSONMap(filepath.Join(s.dataDir, "node.json"))
	status.Peers = readJSONMap(filepath.Join(s.dataDir, "mesh", "peers.json"))
	return status
}

func readJSONMap(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil
	}
	return value
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	return os.Remove(path)
}
