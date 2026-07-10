package takod

import (
	"bufio"
	"context"
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

	"github.com/redentordev/tako-cli/pkg/takoapi/ptystream"
)

type Server struct {
	socket                  string
	dataDir                 string
	version                 string
	nodeName                string
	actualRefreshInterval   time.Duration
	buildCachePruneInterval time.Duration
	buildCacheKeepStorage   string
	startedAt               time.Time
	server                  *http.Server
	backupScheduler         *BackupScheduler
	jobScheduler            *JobScheduler
	mu                      sync.Mutex
}

const (
	takodReadHeaderTimeout         = 5 * time.Second
	takodMaxJSONBodyBytes          = 16 << 20
	takodMaxServiceJSONBodyBytes   = 384 << 20
	DefaultBuildCachePruneInterval = 24 * time.Hour
	buildCachePruneCommandTimeout  = 30 * time.Minute
	buildCachePruneMaxInitialDelay = 5 * time.Minute
)

func decodeJSONRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeJSONRequestWithLimit(w, r, dst, takodMaxJSONBodyBytes)
}

func decodeJSONRequestWithLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
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
	Runtime      string         `json:"runtime"`
	Version      string         `json:"version"`
	Capabilities []string       `json:"capabilities,omitempty"`
	Hostname     string         `json:"hostname"`
	Socket       string         `json:"socket"`
	DataDir      string         `json:"dataDir"`
	StartedAt    time.Time      `json:"startedAt"`
	Now          time.Time      `json:"now"`
	Node         map[string]any `json:"node,omitempty"`
	Peers        map[string]any `json:"peers,omitempty"`
}

// CapabilityContainerArgvV1 means reconcile and job payloads preserve the
// container command/entrypoint argv fields introduced with config exec form.
const CapabilityContainerArgvV1 = "container.argv-v1"

// CapabilityContainerRuntimeControlsV1 covers user/workdir/stop/init/hosts,
// ulimits, and shm-size fields on service and job payloads.
const CapabilityContainerRuntimeControlsV1 = "container.runtime-controls-v1"

// CapabilityImageBuildOptionsV1 means the streamed image builder consumes
// buildArgs and target from the request preamble.
const CapabilityImageBuildOptionsV1 = "image.build-options-v1"

// CapabilityExecOneOffControlsV1 covers pull auth, entrypoint, labels, and
// runtime controls on deploy-time one-off exec requests.
const CapabilityExecOneOffControlsV1 = "exec.oneoff-controls-v1"

// CapabilityServiceFilesV1 means reconcile, jobs, and one-off execution can
// atomically publish request-scoped operator file bundles and bind mount them.
const CapabilityServiceFilesV1 = "service.files-v1"

func NewServer(socket string, dataDir string, version string) *Server {
	return NewServerWithOptions(socket, dataDir, version, ServerOptions{})
}

type ServerOptions struct {
	NodeName                string
	ActualRefreshInterval   time.Duration
	BuildCachePruneInterval time.Duration
	BuildCacheKeepStorage   string
}

func NewServerWithOptions(socket string, dataDir string, version string, opts ServerOptions) *Server {
	return &Server{
		socket:                  socket,
		dataDir:                 dataDir,
		version:                 version,
		nodeName:                opts.NodeName,
		actualRefreshInterval:   opts.ActualRefreshInterval,
		buildCachePruneInterval: opts.BuildCachePruneInterval,
		buildCacheKeepStorage:   opts.BuildCacheKeepStorage,
		startedAt:               time.Now().UTC(),
		backupScheduler:         NewBackupScheduler(dataDir),
		jobScheduler:            NewJobScheduler(dataDir),
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
	mux.HandleFunc("/v1/service-files", s.handleServiceFiles)
	mux.HandleFunc("/v1/service-files/check", s.handleServiceFilesCheck)
	mux.HandleFunc("/v1/remove-service", s.handleRemoveService)
	mux.HandleFunc("/v1/proxy-file", s.handleProxyFile)
	mux.HandleFunc("/v1/proxy", s.handleProxy)
	mux.HandleFunc("/v1/ports/allocate", s.handlePortAllocate)
	mux.HandleFunc("/v1/cleanup", s.handleCleanup)
	mux.HandleFunc("/v1/state", s.handleState)
	mux.HandleFunc("/v1/lease", s.handleLease)
	mux.HandleFunc("/v1/env-bundle", s.handleEnvBundle)
	mux.HandleFunc("/v1/backups", s.handleBackups)
	mux.HandleFunc("/v1/backups/restore", s.handleBackupRestore)
	mux.HandleFunc("/v1/backups/cleanup", s.handleBackupCleanup)
	mux.HandleFunc("/v1/backup-schedule", s.handleBackupSchedule)
	mux.HandleFunc("/v1/metadata", s.handleMetadata)
	mux.HandleFunc("/v1/mesh/key", s.handleMeshKey)
	mux.HandleFunc("/v1/mesh/apply", s.handleMeshApply)
	mux.HandleFunc("/v1/mesh/status", s.handleMeshStatus)
	mux.HandleFunc("/v1/images/exists", s.handleImageExists)
	mux.HandleFunc("/v1/images/export", s.handleImageExport)
	mux.HandleFunc("/v1/images/import", s.handleImageImport)
	mux.HandleFunc("/v1/images/build", s.handleImageBuild)
	mux.HandleFunc("/v1/logs", s.handleLogs)
	mux.HandleFunc("/v1/exec", s.handleExec)
	mux.HandleFunc("/v1/jobs", s.handleJobs)
	mux.HandleFunc("/v1/jobs/apply", s.handleJobsApply)
	mux.HandleFunc("/v1/jobs/runs", s.handleJobRuns)
	mux.HandleFunc("/v1/jobs/trigger", s.handleJobsTrigger)
	mux.HandleFunc("/v1/stats", s.handleStats)
	mux.HandleFunc("/v1/metrics", s.handleMetrics)
	mux.HandleFunc("/v1/access-logs", s.handleAccessLogs)
	mux.HandleFunc("/v1/discovery/exports", s.handleDiscoveryExports)

	httpServer := newTakodHTTPServer(mux)
	s.mu.Lock()
	s.server = httpServer
	s.mu.Unlock()

	if s.actualRefreshInterval > 0 {
		go s.runActualRefreshLoop(ctx)
	}
	if s.buildCachePruneInterval > 0 {
		go s.runBuildCachePruneLoop(ctx)
	}
	go s.backupScheduler.Run(ctx)
	go s.jobScheduler.Run(ctx)

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

func (s *Server) runBuildCachePruneLoop(ctx context.Context) {
	interval := s.buildCachePruneInterval
	if interval <= 0 {
		return
	}
	keepStorage := s.buildCacheKeepStorage
	if keepStorage == "" {
		keepStorage = DefaultBuildCacheKeepStorage
	}
	if !isSafeBuildCacheKeepStorage(keepStorage) {
		fmt.Fprintf(os.Stderr, "takod build cache pruning disabled: invalid keep-storage value %q\n", keepStorage)
		return
	}

	timer := time.NewTimer(buildCachePruneInitialDelay(interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			pruneCtx, cancel := context.WithTimeout(ctx, buildCachePruneCommandTimeout)
			if _, err := cleanupBuildCache(pruneCtx, keepStorage); err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintf(os.Stderr, "takod build cache prune failed: %v\n", err)
			}
			cancel()
			timer.Reset(interval)
		}
	}
}

func buildCachePruneInitialDelay(interval time.Duration) time.Duration {
	if interval < buildCachePruneMaxInitialDelay {
		return interval
	}
	return buildCachePruneMaxInitialDelay
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
	if jobs := s.jobScheduler.List(project, environment); len(jobs) > 0 {
		actual.Jobs = make(map[string]*JobStatus, len(jobs))
		for i := range jobs {
			job := jobs[i]
			actual.Jobs[job.Name] = &job
		}
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
	if err := decodeJSONRequestWithLimit(w, r, &request, takodMaxServiceJSONBodyBytes); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
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

func (s *Server) handleServiceFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var request ServiceFilesRequest
	if err := decodeJSONRequestWithLimit(w, r, &request, takodMaxServiceJSONBodyBytes); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := PublishServiceFiles(r.Context(), request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"fileSetId": request.FileSetID})
}

func (s *Server) handleServiceFilesCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var request ServiceFilesCheckRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := CheckServiceFiles(request); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"fileSetId": request.FileSetID})
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
	if err := ReleaseServicePortAllocationsExceptRevision(r.Context(), s.dataDir, request.Project, request.Environment, request.Service, request.KeepRevision); err != nil {
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
		removedJobs, err := s.jobScheduler.RemoveProject(request.Project, request.Environment)
		if err != nil {
			response.Warnings = append(response.Warnings, fmt.Sprintf("failed to unschedule jobs: %v", err))
		}
		response.JobsRemoved = len(removedJobs)
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
	case http.MethodDelete:
		defer r.Body.Close()
		var request StateDocumentRequest
		if err := decodeJSONRequest(w, r, &request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err = DeleteStateDocument(r.Context(), s.dataDir, request)
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

func (s *Server) handleBackupSchedule(w http.ResponseWriter, r *http.Request) {
	var (
		response *BackupScheduleResponse
		err      error
	)
	switch r.Method {
	case http.MethodPut:
		defer r.Body.Close()
		var request BackupScheduleRequest
		if err := decodeJSONRequest(w, r, &request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err = s.backupScheduler.Upsert(r.Context(), request)
	case http.MethodDelete:
		response, err = s.backupScheduler.Delete(
			r.Context(),
			r.URL.Query().Get("project"),
			r.URL.Query().Get("environment"),
			r.URL.Query().Get("service"),
		)
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
	dockerfile := r.URL.Query().Get("dockerfile")
	if dockerfile != "" {
		if err := validateDockerfilePath(dockerfile); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	defer r.Body.Close()

	body := io.Reader(http.MaxBytesReader(w, r.Body, defaultBuildContextMaxBytes))
	var auths []RegistryAuth
	var buildArgs map[string]string
	var target string
	if r.URL.Query().Get("auth") == "preamble" || r.URL.Query().Get("options") == "preamble" {
		// Credentials ride the body as a single JSON line ahead of the tar
		// stream: never the query string or argv, never persisted (ADR 10).
		buffered := bufio.NewReader(body)
		line, err := buffered.ReadString('\n')
		if err != nil {
			http.Error(w, "invalid registry auth preamble: "+err.Error(), http.StatusBadRequest)
			return
		}
		var preamble struct {
			RegistryAuths []RegistryAuth    `json:"registryAuths"`
			BuildArgs     map[string]string `json:"buildArgs"`
			Target        string            `json:"target"`
		}
		if err := json.Unmarshal([]byte(line), &preamble); err != nil {
			http.Error(w, "invalid registry auth preamble: "+err.Error(), http.StatusBadRequest)
			return
		}
		auths = preamble.RegistryAuths
		buildArgs = preamble.BuildArgs
		target = preamble.Target
		body = buffered
	}

	response, err := BuildImageWithOptions(r.Context(), image, body, auths, ImageBuildOptions{
		Dockerfile: dockerfile,
		BuildArgs:  buildArgs,
		Target:     target,
	})
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

// handleExec streams a service-context command run. Validation and
// resolution errors return HTTP errors before any output; once the stream
// starts, failures surface as output text and the terminal exit marker.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request ExecRequest
	if err := decodeJSONRequestWithLimit(w, r, &request, takodMaxServiceJSONBodyBytes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateExecRequest(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.Interactive || request.PTY {
		s.handleExecStream(w, r, request)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	counting := &countingWriter{writer: &flushResponseWriter{writer: w}}
	if err := StreamExec(r.Context(), request, counting); err != nil && counting.written == 0 {
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
}

// handleExecStream upgrades the connection to the ptystream frame protocol
// and runs an interactive exec session over it. Validation already passed;
// resolution failures surface as Error frames after the upgrade.
func (s *Server) handleExecStream(w http.ResponseWriter, r *http.Request, request ExecRequest) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), ptystream.Protocol) {
		http.Error(w, fmt.Sprintf("interactive exec requires an Upgrade: %s handshake", ptystream.Protocol), http.StatusUpgradeRequired)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection does not support upgrades", http.StatusInternalServerError)
		return
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to hijack connection: %v", err), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: " + ptystream.Protocol + "\r\n" +
		"Connection: Upgrade\r\n\r\n"
	if _, err := buffered.Writer.WriteString(response); err != nil {
		return
	}
	if err := buffered.Writer.Flush(); err != nil {
		return
	}

	// The request context dies with the hijack; the session owns its own
	// lifecycle (client disconnect, absolute and idle timeouts).
	if err := RunExecStream(context.Background(), request, buffered.Reader, conn); err != nil {
		writer := ptystream.NewWriter(conn)
		_ = writer.WriteFrame(ptystream.FrameError, []byte(err.Error()))
		_ = writer.WriteFrame(ptystream.FrameExit, ptystream.EncodeExit(-1))
	}
}

// handleJobs lists scheduled jobs, optionally filtered by project/environment.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jobs := s.jobScheduler.List(r.URL.Query().Get("project"), r.URL.Query().Get("environment"))
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(map[string]any{"jobs": jobs})
}

// handleJobsApply declaratively replaces this node's job set for one
// project/environment.
func (s *Server) handleJobsApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request JobsApplyRequest
	if err := decodeJSONRequestWithLimit(w, r, &request, takodMaxServiceJSONBodyBytes); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	response, err := s.jobScheduler.Apply(r.Context(), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

// handleJobRuns returns run history for one job or a whole environment.
func (s *Server) handleJobRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runs, err := s.jobScheduler.Runs(
		r.URL.Query().Get("project"),
		r.URL.Query().Get("environment"),
		r.URL.Query().Get("job"),
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(map[string]any{"runs": runs})
}

// handleJobsTrigger runs a job immediately, streaming output framed by the
// exec markers. Pre-stream failures (unknown job, overlap) map to 400.
func (s *Server) handleJobsTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request JobTriggerRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	counting := &countingWriter{writer: &flushResponseWriter{writer: w}}
	if err := s.jobScheduler.Trigger(r.Context(), request.Project, request.Environment, request.Job, counting); err != nil && counting.written == 0 {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

// countingWriter tracks whether any bytes reached the response so handlers
// know if an HTTP error can still be written.
type countingWriter struct {
	writer  io.Writer
	written int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.written += int64(n)
	return n, err
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

func (s *Server) handleDiscoveryExports(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	environment := strings.TrimSpace(r.URL.Query().Get("environment"))
	if environment != "" && !isSafeRuntimeName(environment) {
		http.Error(w, "invalid environment name", http.StatusBadRequest)
		return
	}

	response, err := ListExportDiscovery(r.Context(), environment)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
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
		Runtime:      "takod",
		Version:      s.version,
		Capabilities: []string{CapabilityContainerArgvV1, CapabilityContainerRuntimeControlsV1, CapabilityImageBuildOptionsV1, CapabilityExecOneOffControlsV1, CapabilityServiceFilesV1},
		Hostname:     hostname,
		Socket:       s.socket,
		DataDir:      s.dataDir,
		StartedAt:    s.startedAt,
		Now:          time.Now().UTC(),
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
