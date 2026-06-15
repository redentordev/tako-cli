package takod

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	socket                string
	dataDir               string
	version               string
	nodeName              string
	actualRefreshInterval time.Duration
	startedAt             time.Time
	server                *http.Server
	mu                    sync.Mutex
}

const (
	takodReadHeaderTimeout   = 5 * time.Second
	takodMaxJSONBodyBytes    = 16 << 20
	takodMaxStreamInputBytes = 16 << 20
	takodStreamRequestHeader = "X-Tako-Request"
	takodExitCodeTrailer     = "X-Tako-Exit-Code"
)

func decodeJSONRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, takodMaxJSONBodyBytes)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		return err
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body must contain a single JSON value")
		}
		return err
	}
	return nil
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
	return NewServerWithOptions(socket, dataDir, version, ServerOptions{})
}

type ServerOptions struct {
	NodeName              string
	ActualRefreshInterval time.Duration
}

func NewServerWithOptions(socket string, dataDir string, version string, opts ServerOptions) *Server {
	return &Server{
		socket:                socket,
		dataDir:               dataDir,
		version:               version,
		nodeName:              opts.NodeName,
		actualRefreshInterval: opts.ActualRefreshInterval,
		startedAt:             time.Now().UTC(),
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
	mux.HandleFunc("/v1/inspect", s.handleInspect)
	mux.HandleFunc("/v1/reconcile-service", s.handleReconcileService)
	mux.HandleFunc("/v1/remove-service", s.handleRemoveService)
	mux.HandleFunc("/v1/proxy-file", s.handleProxyFile)
	mux.HandleFunc("/v1/proxy", s.handleProxy)
	mux.HandleFunc("/v1/discovery", s.handleDiscovery)
	mux.HandleFunc("/v1/proxy-target", s.handleProxyTarget)
	mux.HandleFunc("/v1/exec-target", s.handleExecTarget)
	mux.HandleFunc("/v1/exec", s.handleExecStream)
	mux.HandleFunc("/v1/run", s.handleRunOneOffStream)
	mux.HandleFunc("/v1/hooks/run", s.handleRunHook)
	mux.HandleFunc("/v1/volumes/inspect", s.handleVolumeInspect)
	mux.HandleFunc("/v1/ports/allocate", s.handlePortAllocate)
	mux.HandleFunc("/v1/cleanup", s.handleCleanup)
	mux.HandleFunc("/v1/acme-dns", s.handleAcmeDNS)
	mux.HandleFunc("/v1/acme-dns/register", s.handleAcmeDNSRegister)
	mux.HandleFunc("/v1/acme-dns/credentials", s.handleAcmeDNSCredentials)
	mux.HandleFunc("/v1/state", s.handleState)
	mux.HandleFunc("/v1/lease", s.handleLease)
	mux.HandleFunc("/v1/env-bundle", s.handleEnvBundle)
	mux.HandleFunc("/v1/backups", s.handleBackups)
	mux.HandleFunc("/v1/backups/restore", s.handleBackupRestore)
	mux.HandleFunc("/v1/backups/cleanup", s.handleBackupCleanup)
	mux.HandleFunc("/v1/metadata", s.handleMetadata)
	mux.HandleFunc("/v1/mesh/key", s.handleMeshKey)
	mux.HandleFunc("/v1/mesh/apply", s.handleMeshApply)
	mux.HandleFunc("/v1/mesh/status", s.handleMeshStatus)
	mux.HandleFunc("/v1/images/exists", s.handleImageExists)
	mux.HandleFunc("/v1/images", s.handleImages)
	mux.HandleFunc("/v1/images/prune", s.handleImagePrune)
	mux.HandleFunc("/v1/images/export", s.handleImageExport)
	mux.HandleFunc("/v1/images/import", s.handleImageImport)
	mux.HandleFunc("/v1/images/build", s.handleImageBuild)
	mux.HandleFunc("/v1/logs", s.handleLogs)
	mux.HandleFunc("/v1/stats", s.handleStats)
	mux.HandleFunc("/v1/metrics", s.handleMetrics)
	mux.HandleFunc("/v1/node/info", s.handleNodeInfo)
	mux.HandleFunc("/v1/node/logs", s.handleNodeLogs)
	mux.HandleFunc("/v1/mesh/rtt", s.handleMeshRTT)
	mux.HandleFunc("/v1/access-logs", s.handleAccessLogs)
	mux.HandleFunc("/v1/volumes", s.handleVolumes)

	httpServer := newTakodHTTPServer(mux)
	s.mu.Lock()
	s.server = httpServer
	s.mu.Unlock()

	if s.actualRefreshInterval > 0 {
		go s.runActualRefreshLoop(ctx)
	}

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

func newTakodHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: takodReadHeaderTimeout,
	}
}

func (s *Server) runActualRefreshLoop(ctx context.Context) {
	if s.nodeName == "" {
		fmt.Fprintln(os.Stderr, "takod actual refresh disabled: node name is required")
		return
	}

	refresh := func() {
		if _, err := RefreshActualStateDocuments(ctx, s.dataDir, s.nodeName); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "takod actual refresh failed: %v\n", err)
		}
	}

	refresh()
	ticker := time.NewTicker(s.actualRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
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
	if !isSafeProjectName(project) {
		http.Error(w, "invalid project name", http.StatusBadRequest)
		return
	}
	if !isSafeRuntimeName(environment) {
		http.Error(w, "invalid environment name", http.StatusBadRequest)
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

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response, err := InspectProject(r.Context(), InspectRequest{
		Project:     r.URL.Query().Get("project"),
		Environment: r.URL.Query().Get("environment"),
		Service:     r.URL.Query().Get("service"),
	}, s.nodeName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleReconcileService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request ReconcileServiceRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	request.ConfigDir = filepath.Join(s.dataDir, "configs")
	if err := validateReconcileServiceRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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

func (s *Server) handleRemoveService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request RemoveServiceRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateRemoveServiceRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	response, err := RemoveService(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := ReleaseServicePortAllocations(r.Context(), s.dataDir, request.Project, request.Environment, request.Service); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handlePortAllocate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request PortAllocationRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	response, err := AllocatePort(r.Context(), s.dataDir, request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		if err := decodeJSONRequest(w, r, &request); err != nil {
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
	if r.Method == http.MethodDelete {
		response, err := DisableProxy(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(response)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request ReconcileProxyRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	normalizeReconcileProxyRequest(&request)
	if err := validateReconcileProxyRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	port := 0
	if raw := r.URL.Query().Get("port"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			http.Error(w, "invalid port", http.StatusBadRequest)
			return
		}
		port = parsed
	}
	roundRobin := false
	if raw := r.URL.Query().Get("roundRobin"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			http.Error(w, "invalid roundRobin value", http.StatusBadRequest)
			return
		}
		roundRobin = parsed
	}

	response, err := ResolveDiscovery(r.Context(), DiscoveryRequest{
		Project:     r.URL.Query().Get("project"),
		Environment: r.URL.Query().Get("environment"),
		Service:     r.URL.Query().Get("service"),
		Port:        port,
		RoundRobin:  roundRobin,
	}, s.nodeName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	response, err := CleanupProject(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.RemoveContainers || request.RemoveTakodState {
		if err := ReleaseProjectPortAllocations(r.Context(), s.dataDir, request.Project, request.Environment); err != nil {
			response.Warnings = append(response.Warnings, fmt.Sprintf("failed to release port allocations: %v", err))
		}
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
		if err := decodeJSONRequest(w, r, &request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		normalizeReconcileAcmeDNSRequest(&request)
		if err := validateReconcileAcmeDNSRequest(request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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

func (s *Server) handleAcmeDNSRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var request AcmeDNSRegisterRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	normalizeAcmeDNSRegisterRequest(&request)
	if err := validateAcmeDNSRegisterRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	response, err := RegisterAcmeDNS(r.Context(), request)
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
		if err := decodeJSONRequest(w, r, &request); err != nil {
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
			Node:        r.URL.Query().Get("node"),
			RevisionID:  r.URL.Query().Get("revisionId"),
		})
	case http.MethodPut:
		defer r.Body.Close()
		var request StateDocumentRequest
		if err := decodeJSONRequest(w, r, &request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err = WriteStateDocument(r.Context(), s.dataDir, request)
	case http.MethodPost:
		defer r.Body.Close()
		var request StateDocumentRequest
		if err := decodeJSONRequest(w, r, &request); err != nil {
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

func (s *Server) handleLease(w http.ResponseWriter, r *http.Request) {
	var (
		response *LeaseResponse
		err      error
	)

	switch r.Method {
	case http.MethodGet:
		response, err = ReadLease(r.Context(), s.dataDir, LeaseRequest{
			Project:     r.URL.Query().Get("project"),
			Environment: r.URL.Query().Get("environment"),
		})
	case http.MethodPost:
		defer r.Body.Close()
		var request LeaseRequest
		if err := decodeJSONRequest(w, r, &request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err = AcquireLease(r.Context(), s.dataDir, request)
	case http.MethodDelete:
		defer r.Body.Close()
		var request LeaseRequest
		if err := decodeJSONRequest(w, r, &request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err = ReleaseLease(r.Context(), s.dataDir, request)
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
		if err := decodeJSONRequest(w, r, &request); err != nil {
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
		if err := decodeJSONRequest(w, r, &request); err != nil {
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
	if err := decodeJSONRequest(w, r, &request); err != nil {
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
	if err := decodeJSONRequest(w, r, &request); err != nil {
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
	if err := decodeJSONRequest(w, r, &request); err != nil {
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

func (s *Server) handleMeshKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response, err := EnsureMeshKey(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleMeshApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request MeshApplyRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateMeshApplyRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	response, err := ReconcileMesh(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleMeshStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	interfaceName := r.URL.Query().Get("interface")
	if interfaceName == "" {
		http.Error(w, "interface is required", http.StatusBadRequest)
		return
	}
	if err := validateMeshStatusRequest(interfaceName); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	response, err := ReadMeshStatus(r.Context(), interfaceName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleMeshRTT(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	count := 0
	if rawCount := r.URL.Query().Get("count"); rawCount != "" {
		parsed, err := strconv.Atoi(rawCount)
		if err != nil {
			http.Error(w, "count must be an integer", http.StatusBadRequest)
			return
		}
		count = parsed
	}
	response, err := MeasureMeshRTT(r.Context(), MeshRTTRequest{
		Target: r.URL.Query().Get("target"),
		Count:  count,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleImageExists(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response, err := ImageExists(r.Context(), r.URL.Query().Get("image"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleNodeInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response, err := NodeInfo(r.Context(), s.nodeName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		response, err := ListImages(r.Context(), ImageListRequest{
			Project:     r.URL.Query().Get("project"),
			Environment: r.URL.Query().Get("environment"),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(response)
	case http.MethodDelete:
		defer r.Body.Close()
		var request ImageRemoveRequest
		if err := decodeJSONRequest(w, r, &request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err := RemoveImages(r.Context(), request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(response)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleImagePrune(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var request ImagePruneRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	response, err := PruneImages(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleImageExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	image := r.URL.Query().Get("image")
	if err := validateImageName(image); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeImageTarHeaders(w, image)
	if err := ExportImage(r.Context(), image, w); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
}

func (s *Server) handleImageImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	image := r.URL.Query().Get("image")
	if err := validateImageName(image); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.ContentLength > defaultImageImportMaxBytes {
		http.Error(w, fmt.Sprintf("image import exceeds maximum size %d bytes", defaultImageImportMaxBytes), http.StatusRequestEntityTooLarge)
		return
	}
	defer r.Body.Close()

	response, err := ImportImage(r.Context(), image, http.MaxBytesReader(w, r.Body, defaultImageImportMaxBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleImageBuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	image := r.URL.Query().Get("image")
	if err := validateImageName(image); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.ContentLength > defaultBuildContextMaxBytes {
		http.Error(w, fmt.Sprintf("build context exceeds maximum size %d bytes", defaultBuildContextMaxBytes), http.StatusRequestEntityTooLarge)
		return
	}
	defer r.Body.Close()

	query := r.URL.Query()
	useBuildx := false
	if rawBuildx := query.Get("buildx"); rawBuildx != "" {
		parsed, err := strconv.ParseBool(rawBuildx)
		if err != nil {
			http.Error(w, "buildx must be a boolean", http.StatusBadRequest)
			return
		}
		useBuildx = parsed
	}
	response, err := BuildImageWithOptions(r.Context(), image, ImageBuildOptions{
		Platform:   query.Get("platform"),
		Dockerfile: query.Get("dockerfile"),
		CacheFrom:  query["cacheFrom"],
		CacheTo:    query["cacheTo"],
		Builder:    query.Get("builder"),
		UseBuildx:  useBuildx,
	}, http.MaxBytesReader(w, r.Body, defaultBuildContextMaxBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tail := 100
	if rawTail := r.URL.Query().Get("tail"); rawTail != "" {
		parsed, err := strconv.Atoi(rawTail)
		if err != nil {
			http.Error(w, "tail must be an integer", http.StatusBadRequest)
			return
		}
		tail = parsed
	}
	follow := false
	if rawFollow := r.URL.Query().Get("follow"); rawFollow != "" {
		parsed, err := strconv.ParseBool(rawFollow)
		if err != nil {
			http.Error(w, "follow must be a boolean", http.StatusBadRequest)
			return
		}
		follow = parsed
	}

	request := LogsRequest{
		Project:     r.URL.Query().Get("project"),
		Environment: r.URL.Query().Get("environment"),
		Service:     r.URL.Query().Get("service"),
		Tail:        tail,
		Follow:      follow,
	}
	if err := validateLogsRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := StreamServiceLogs(r.Context(), request, &flushResponseWriter{writer: w}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
}

func (s *Server) handleNodeLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tail := 100
	if rawTail := r.URL.Query().Get("tail"); rawTail != "" {
		parsed, err := strconv.Atoi(rawTail)
		if err != nil {
			http.Error(w, "tail must be an integer", http.StatusBadRequest)
			return
		}
		tail = parsed
	}
	follow := false
	if rawFollow := r.URL.Query().Get("follow"); rawFollow != "" {
		parsed, err := strconv.ParseBool(rawFollow)
		if err != nil {
			http.Error(w, "follow must be a boolean", http.StatusBadRequest)
			return
		}
		follow = parsed
	}
	request := NodeLogsRequest{
		Unit:   r.URL.Query().Get("unit"),
		Tail:   tail,
		Follow: follow,
	}
	normalizeNodeLogsRequest(&request)
	if err := validateNodeLogsRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := StreamNodeLogs(r.Context(), request, &flushResponseWriter{writer: w}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	all := false
	if rawAll := r.URL.Query().Get("all"); rawAll != "" {
		parsed, err := strconv.ParseBool(rawAll)
		if err != nil {
			http.Error(w, "all must be a boolean", http.StatusBadRequest)
			return
		}
		all = parsed
	}

	response, err := ReadContainerStats(r.Context(), StatsRequest{
		Project:     r.URL.Query().Get("project"),
		Environment: r.URL.Query().Get("environment"),
		Service:     r.URL.Query().Get("service"),
		All:         all,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleProxyTarget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	port := 0
	if rawPort := r.URL.Query().Get("port"); rawPort != "" {
		parsed, err := strconv.Atoi(rawPort)
		if err != nil {
			http.Error(w, "port must be an integer", http.StatusBadRequest)
			return
		}
		port = parsed
	}

	request := ProxyTargetRequest{
		Project:     r.URL.Query().Get("project"),
		Environment: r.URL.Query().Get("environment"),
		Service:     r.URL.Query().Get("service"),
		Port:        port,
	}
	if err := validateProxyTargetRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	response, err := ResolveProxyTarget(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleExecTarget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	slot := 0
	if rawSlot := r.URL.Query().Get("slot"); rawSlot != "" {
		parsed, err := strconv.Atoi(rawSlot)
		if err != nil {
			http.Error(w, "slot must be an integer", http.StatusBadRequest)
			return
		}
		slot = parsed
	}

	request := ExecTargetRequest{
		Project:     r.URL.Query().Get("project"),
		Environment: r.URL.Query().Get("environment"),
		Service:     r.URL.Query().Get("service"),
		Slot:        slot,
	}
	response, err := ResolveExecTarget(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleExecStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var request ExecStreamRequest
	if err := decodeStreamRequestHeader(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stdin, err := finiteCommandInput(r, request.Stdin, request.TTY)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	streamCommandResponse(w, func(writer io.Writer) (int, error) {
		return ExecuteServiceCommand(r.Context(), request, stdin, writer)
	})
}

func (s *Server) handleRunOneOffStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var request RunOneOffRequest
	if err := decodeStreamRequestHeader(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stdin, err := finiteCommandInput(r, request.Stdin, request.TTY)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	streamCommandResponse(w, func(writer io.Writer) (int, error) {
		return RunOneOffCommand(r.Context(), request, stdin, writer)
	})
}

func (s *Server) handleRunHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request RunHookRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	response, err := RunHookCommand(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleVolumeInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request VolumeInspectRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	response, err := InspectVolumes(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleVolumes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		response, err := ListVolumes(r.Context(), VolumeListRequest{
			Project:     r.URL.Query().Get("project"),
			Environment: r.URL.Query().Get("environment"),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(response)
	case http.MethodDelete:
		defer r.Body.Close()
		var request VolumeRemoveRequest
		if err := decodeJSONRequest(w, r, &request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err := RemoveVolumes(r.Context(), request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(response)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func decodeStreamRequestHeader(r *http.Request, dst any) error {
	encoded := r.Header.Get(takodStreamRequestHeader)
	if strings.TrimSpace(encoded) == "" {
		return fmt.Errorf("%s header is required", takodStreamRequestHeader)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("invalid %s header", takodStreamRequestHeader)
	}
	if len(data) > takodMaxJSONBodyBytes {
		return fmt.Errorf("stream request metadata exceeds limit")
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("failed to parse stream request metadata: %w", err)
	}
	return nil
}

func finiteCommandInput(r *http.Request, wantsStdin bool, tty bool) (io.Reader, error) {
	if !wantsStdin {
		return nil, nil
	}
	if tty {
		return r.Body, nil
	}

	limited := io.LimitReader(r.Body, takodMaxStreamInputBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("failed to read command stdin: %w", err)
	}
	if len(data) > takodMaxStreamInputBytes {
		return nil, fmt.Errorf("command stdin exceeds %d bytes", takodMaxStreamInputBytes)
	}
	return bytes.NewReader(data), nil
}

func streamCommandResponse(w http.ResponseWriter, run func(io.Writer) (int, error)) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Trailer", takodExitCodeTrailer)
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	exitCode, err := run(w)
	if err != nil {
		if exitCode == 0 {
			exitCode = 1
		}
		_, _ = fmt.Fprintf(w, "\n%s\n", err)
	}
	w.Header().Set(takodExitCodeTrailer, strconv.Itoa(exitCode))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	collect := false
	if rawCollect := r.URL.Query().Get("collect"); rawCollect != "" {
		parsed, err := strconv.ParseBool(rawCollect)
		if err != nil {
			http.Error(w, "collect must be a boolean", http.StatusBadRequest)
			return
		}
		collect = parsed
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}

	switch format {
	case "json":
	case "prometheus":
		content, err := RenderPrometheusMetrics(r.Context(), PrometheusMetricsRequest{
			Collect:     collect,
			Project:     r.URL.Query().Get("project"),
			Environment: r.URL.Query().Get("environment"),
			Node:        s.nodeName,
			DataDir:     s.dataDir,
			StartedAt:   s.startedAt,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(content))
		return
	default:
		http.Error(w, "invalid metrics format", http.StatusBadRequest)
		return
	}

	response, err := ReadNodeMetrics(r.Context(), collect)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleAccessLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tail := 50
	if rawTail := r.URL.Query().Get("tail"); rawTail != "" {
		parsed, err := strconv.Atoi(rawTail)
		if err != nil {
			http.Error(w, "tail must be an integer", http.StatusBadRequest)
			return
		}
		tail = parsed
	}
	follow := false
	if rawFollow := r.URL.Query().Get("follow"); rawFollow != "" {
		parsed, err := strconv.ParseBool(rawFollow)
		if err != nil {
			http.Error(w, "follow must be a boolean", http.StatusBadRequest)
			return
		}
		follow = parsed
	}
	if err := validateAccessLogTail(tail); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := StreamProxyAccessLogs(r.Context(), tail, follow, &flushResponseWriter{writer: w}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
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

type flushResponseWriter struct {
	writer http.ResponseWriter
}

func (w *flushResponseWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if flusher, ok := w.writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}
