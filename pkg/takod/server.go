package takod

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
	"github.com/redentordev/tako-cli/pkg/takoapi/ptystream"
)

type Server struct {
	socket                  string
	dataDir                 string
	version                 string
	nodeName                string
	identityFile            string
	inventoryFile           string
	membershipFile          string
	installation            *nodeidentity.Installation
	minimumFreeDiskBytes    int64
	dockerDataRoot          string
	buildSlots              chan struct{}
	diskAvailable           func(string) (int64, error)
	diskIdentity            func(string) (string, error)
	actualRefreshInterval   time.Duration
	buildCachePruneInterval time.Duration
	buildCacheKeepStorage   string
	startedAt               time.Time
	server                  *http.Server
	backupScheduler         *BackupScheduler
	jobScheduler            *JobScheduler
	certificateScheduler    *CertificateScheduler
	uploadReadTimeout       time.Duration
	diskReservationMu       sync.Mutex
	diskReservations        map[string]int64
	proxyAuthorityMu        sync.Mutex
	mu                      sync.Mutex
}

const (
	takodReadHeaderTimeout         = 5 * time.Second
	takodMaxJSONBodyBytes          = 16 << 20
	takodMaxServiceJSONBodyBytes   = 384 << 20
	DefaultBuildCachePruneInterval = 24 * time.Hour
	buildCachePruneCommandTimeout  = 30 * time.Minute
	buildCachePruneMaxInitialDelay = 5 * time.Minute
	takodUploadReadTimeout         = 30 * time.Minute
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
	Runtime      string                 `json:"runtime"`
	Version      string                 `json:"version"`
	Capabilities []string               `json:"capabilities,omitempty"`
	Hostname     string                 `json:"hostname"`
	Socket       string                 `json:"socket"`
	DataDir      string                 `json:"dataDir"`
	StartedAt    time.Time              `json:"startedAt"`
	Now          time.Time              `json:"now"`
	Identity     *nodeidentity.Identity `json:"identity,omitempty"`
	// EnrollmentRoles are immutable bootstrap facts. Current control-plane
	// roles are separate mutable state and must not be inferred from this list.
	EnrollmentRoles      []string                    `json:"enrollmentRoles,omitempty"`
	Membership           *nodeidentity.InventoryNode `json:"membership,omitempty"`
	MembershipGeneration uint64                      `json:"membershipGeneration,omitempty"`
	Node                 map[string]any              `json:"node,omitempty"`
	Peers                map[string]any              `json:"peers,omitempty"`
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

// CapabilityImageDescriptorV1 means image reuse can be proven with config
// digest, platform, and Docker daemon identity instead of a mutable tag.
const CapabilityImageDescriptorV1 = "image.descriptor-v1"

const CapabilityNodePlatformV1 = "node.platform-v1"

const CapabilityNodeLifecycleV1 = "node.lifecycle-v1"
const CapabilityNodeMembershipV1 = "node.membership-controller-v1"

// CapabilityExecOneOffControlsV1 covers pull auth, entrypoint, labels, and
// runtime controls on deploy-time one-off exec requests.
const CapabilityExecOneOffControlsV1 = "exec.oneoff-controls-v1"

// CapabilityServiceFilesV1 means reconcile, jobs, and one-off execution can
// atomically publish request-scoped operator file bundles and bind mount them.
const CapabilityServiceFilesV1 = "service.files-v1"

// CapabilityProxyTrustedProxiesV1 means proxy route manifests accept explicit
// trusted proxy CIDRs and render Caddy's trusted client IP handling.
const CapabilityProxyTrustedProxiesV1 = "proxy.trusted-proxies-v1"

// CapabilityProxyRemoteMeshRoutesV1 will be advertised only when the node can
// validate authoritative active allocation generations and revocations. Phase
// 3 intentionally omits it so unsupported multi-node routes fail preflight.
const CapabilityProxyRemoteMeshRoutesV1 = "proxy.remote-mesh-routes-v1"

// CapabilityProxyCertsV1 means the node exposes the validated certificate
// store API and can render store-backed TLS directives safely.
const CapabilityProxyCertsV1 = "proxy.certs-v1"

// CapabilityNodeIdentityV1 means status exposes an immutable installation
// identity separately from mutable project/environment node metadata.
const CapabilityNodeIdentityV1 = nodeidentity.Capability

func NewServer(socket string, dataDir string, version string) *Server {
	return NewServerWithOptions(socket, dataDir, version, ServerOptions{})
}

type ServerOptions struct {
	NodeName                string
	IdentityFile            string
	InventoryFile           string
	MembershipFile          string
	ActualRefreshInterval   time.Duration
	BuildCachePruneInterval time.Duration
	BuildCacheKeepStorage   string
	MinimumFreeDiskBytes    int64
	DockerDataRoot          string
	MaximumConcurrentBuilds int
	DiskAvailable           func(string) (int64, error)
	DiskIdentity            func(string) (string, error)
	UploadReadTimeout       time.Duration
}

func NewServerWithOptions(socket string, dataDir string, version string, opts ServerOptions) *Server {
	server := &Server{
		socket:                  socket,
		dataDir:                 dataDir,
		version:                 version,
		nodeName:                opts.NodeName,
		identityFile:            opts.IdentityFile,
		inventoryFile:           opts.InventoryFile,
		membershipFile:          opts.MembershipFile,
		actualRefreshInterval:   opts.ActualRefreshInterval,
		buildCachePruneInterval: opts.BuildCachePruneInterval,
		buildCacheKeepStorage:   opts.BuildCacheKeepStorage,
		minimumFreeDiskBytes:    opts.MinimumFreeDiskBytes,
		dockerDataRoot:          opts.DockerDataRoot,
		diskAvailable:           opts.DiskAvailable,
		diskIdentity:            opts.DiskIdentity,
		startedAt:               time.Now().UTC(),
		backupScheduler:         NewBackupScheduler(dataDir),
		jobScheduler:            NewJobScheduler(dataDir),
		certificateScheduler:    NewCertificateScheduler(dataDir),
		uploadReadTimeout:       opts.UploadReadTimeout,
		diskReservations:        make(map[string]int64),
	}
	if server.diskAvailable == nil {
		server.diskAvailable = availableDiskBytes
	}
	if server.diskIdentity == nil {
		server.diskIdentity = diskFilesystemIdentity
	}
	if server.uploadReadTimeout <= 0 {
		server.uploadReadTimeout = takodUploadReadTimeout
	}
	if strings.TrimSpace(server.dockerDataRoot) == "" {
		server.dockerDataRoot = server.dataDir
	}
	if strings.TrimSpace(server.inventoryFile) == "" {
		server.inventoryFile = nodeidentity.DefaultInventoryPath
	}
	if strings.TrimSpace(server.membershipFile) == "" {
		server.membershipFile = platform.DefaultMembershipPath(filepath.Join(server.dataDir, "platform"))
	}
	if opts.MaximumConcurrentBuilds > 0 {
		server.buildSlots = make(chan struct{}, opts.MaximumConcurrentBuilds)
	}
	server.backupScheduler.admit = func(...string) error { return server.checkFreeDisk(0, backupRootDir) }
	server.jobScheduler.admit = func(...string) error { return server.checkFreeDisk(0, server.dataDir, server.dockerDataRoot) }
	server.certificateScheduler.admit = func(paths ...string) error { return server.checkFreeDisk(0, paths...) }
	return server
}

func (s *Server) Run(ctx context.Context) error {
	if s.socket == "" {
		return fmt.Errorf("socket path is required")
	}
	if s.dataDir == "" {
		return fmt.Errorf("data directory is required")
	}
	if s.identityFile != "" {
		installation, err := nodeidentity.ReadOptional(s.identityFile)
		if err != nil {
			return fmt.Errorf("failed to load installation identity: %w", err)
		}
		if installation != nil {
			if s.nodeName != "" && s.nodeName != installation.NodeName {
				return fmt.Errorf("configured node name %q does not match installation identity node name %q", s.nodeName, installation.NodeName)
			}
			s.installation = installation
			s.nodeName = installation.NodeName
		}
	}
	if s.installation != nil {
		if _, err := os.Stat(s.membershipFile); err == nil {
			store, storeErr := platform.NewMembershipStore(s.membershipFile, s.inventoryFile)
			if storeErr != nil {
				return fmt.Errorf("open controller membership at startup: %w", storeErr)
			}
			if _, storeErr = store.Read(); storeErr != nil {
				return fmt.Errorf("reconcile controller membership at startup: %w", storeErr)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect controller membership at startup: %w", err)
		}
		deactivatePolicy, err := s.enforceEnrolledProxyPolicy(ctx)
		if err != nil {
			return err
		}
		defer deactivatePolicy()
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
	for _, route := range s.registeredRoutes() {
		mux.HandleFunc(route.path, route.handler)
	}

	httpServer := newTakodHTTPServer(s.enrolledLifecycleHandler(mux))
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
	go s.certificateScheduler.Run(ctx)

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

func (s *Server) enforceEnrolledProxyPolicy(ctx context.Context) (func(), error) {
	deactivate := activateEnrolledProxyPolicy(s.installation.ClusterID, s.inventoryFile)
	fail := func(err error) (func(), error) {
		deactivate()
		return func() {}, err
	}
	s.proxyAuthorityMu.Lock()
	if err := s.reconcileAllStoredProxyAuthority(); err != nil {
		s.proxyAuthorityMu.Unlock()
		if stopErr := stopProxyAndVerifyAbsent(ctx); stopErr != nil {
			return fail(fmt.Errorf("reconcile stored allocation authority before startup: %v; fail-closed proxy stop also failed: %w", err, stopErr))
		}
		return fail(fmt.Errorf("reconcile stored allocation authority before startup; proxy verified stopped: %w", err))
	}
	s.proxyAuthorityMu.Unlock()
	entries, err := os.ReadDir(proxyRoutesDir)
	if os.IsNotExist(err) {
		if _, caddyErr := os.Stat(proxyCaddyfilePath); os.IsNotExist(caddyErr) {
			if stopErr := stopProxyAndVerifyAbsent(ctx); stopErr != nil {
				return fail(fmt.Errorf("verify no in-memory proxy survives clean enrolled startup: %w", stopErr))
			}
			return deactivate, nil
		} else if caddyErr != nil {
			return fail(fmt.Errorf("inspect stored proxy configuration before enrolled startup: %w", caddyErr))
		}
		entries = nil
		err = nil
	}
	if err != nil {
		return fail(fmt.Errorf("inspect stored proxy routes before enrolled startup: %w", err))
	}
	type invalidManifest struct {
		name string
		data []byte
	}
	invalid := make([]invalidManifest, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(proxyRoutesDir, entry.Name()))
		if readErr != nil {
			return fail(fmt.Errorf("read stored proxy route %s before enrolled startup: %w", entry.Name(), readErr))
		}
		if policyErr := validateEnrolledProxyRouteManifest(string(data), s.installation.ClusterID, s.inventoryFile); policyErr != nil {
			invalid = append(invalid, invalidManifest{name: entry.Name(), data: data})
		}
	}
	if len(invalid) == 0 {
		if err := ensureProxyDirectories(); err != nil {
			return fail(err)
		}
		if err := renderAndWriteCaddyfile(ctx); err != nil {
			if stopErr := stopProxyAndVerifyAbsent(ctx); stopErr != nil {
				return fail(fmt.Errorf("publish authoritative enrolled proxy configuration: %v; fail-closed proxy stop also failed: %w", err, stopErr))
			}
			return fail(fmt.Errorf("publish authoritative enrolled proxy configuration: %w", err))
		}
		if err := reloadEnrolledProxyIfRunning(ctx); err != nil {
			return fail(err)
		}
		return deactivate, nil
	}

	// The current Caddyfile may already contain one of the rejected routes.
	// Stop a running proxy before touching manifests so enrollment cannot
	// report ready while historical remote traffic is still being served.
	if err := stopProxyAndVerifyAbsent(ctx); err != nil {
		return fail(fmt.Errorf("stop proxy before withdrawing unsafe enrolled routes: %w", err))
	}
	if err := ensureProxyDirectories(); err != nil {
		return fail(err)
	}
	if err := writeFileAtomic(proxyCaddyfilePath, []byte(emptyProxyCaddyfile()), 0644); err != nil {
		return fail(fmt.Errorf("publish fail-closed proxy configuration: %w", err))
	}
	if err := syncDirectory(filepath.Dir(proxyCaddyfilePath)); err != nil {
		return fail(err)
	}
	quarantineDir := filepath.Join(s.dataDir, "proxy", "quarantine")
	if err := os.MkdirAll(quarantineDir, 0700); err != nil {
		return fail(fmt.Errorf("create proxy route quarantine: %w", err))
	}
	stamp := time.Now().UTC().UnixNano()
	for index, manifest := range invalid {
		quarantinePath := filepath.Join(quarantineDir, fmt.Sprintf("%s.%d.%d.quarantined", manifest.name, stamp, index))
		if err := writeFileAtomic(quarantinePath, manifest.data, 0600); err != nil {
			return fail(fmt.Errorf("quarantine proxy route %s: %w", manifest.name, err))
		}
		if err := os.Remove(filepath.Join(proxyRoutesDir, manifest.name)); err != nil {
			return fail(fmt.Errorf("withdraw proxy route %s: %w", manifest.name, err))
		}
	}
	if err := syncDirectory(proxyRoutesDir); err != nil {
		return fail(err)
	}
	if err := syncDirectory(quarantineDir); err != nil {
		return fail(err)
	}
	if err := renderAndWriteCaddyfile(ctx); err != nil {
		return fail(fmt.Errorf("render safe enrolled proxy configuration: %w", err))
	}
	log.Printf("takod enrolled proxy policy: quarantined=%d remote-or-legacy route manifests; proxy stopped fail-closed", len(invalid))
	return deactivate, nil
}

func reloadEnrolledProxyIfRunning(ctx context.Context) error {
	running, err := runDocker(ctx, "ps", "--filter", "name=^tako-proxy$", "--format", "{{.Names}}")
	if err != nil {
		return fmt.Errorf("inspect proxy before enrolled policy reload: %w", err)
	}
	if strings.TrimSpace(running) != "tako-proxy" {
		return nil
	}
	output, err := runDocker(ctx, "exec", "tako-proxy", "caddy", "reload", "--adapter", "caddyfile", "--config", "/etc/caddy/Caddyfile")
	if err == nil {
		return nil
	}
	if stopErr := stopProxyAndVerifyAbsent(ctx); stopErr != nil {
		return fmt.Errorf("reload authoritative enrolled proxy configuration: %v, output: %s; fail-closed proxy stop also failed: %w", err, output, stopErr)
	}
	return fmt.Errorf("reload authoritative enrolled proxy configuration; proxy verified stopped fail-closed: %w, output: %s", err, output)
}

func stopProxyAndVerifyAbsent(ctx context.Context) error {
	running, err := runDocker(ctx, "ps", "--filter", "name=^tako-proxy$", "--format", "{{.Names}}")
	if err != nil {
		return fmt.Errorf("inspect tako-proxy state: %w", err)
	}
	if strings.TrimSpace(running) == "tako-proxy" {
		output, removeErr := runDocker(ctx, "rm", "-f", "tako-proxy")
		if removeErr != nil {
			return fmt.Errorf("remove running tako-proxy: %w, output: %s", removeErr, output)
		}
	}
	running, err = runDocker(ctx, "ps", "--filter", "name=^tako-proxy$", "--format", "{{.Names}}")
	if err != nil {
		return fmt.Errorf("verify tako-proxy stopped: %w", err)
	}
	if strings.TrimSpace(running) == "tako-proxy" {
		return fmt.Errorf("tako-proxy is still running after removal")
	}
	return nil
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
	paths := make([]string, 0, 2)
	if len(request.Containers) > 0 {
		paths = append(paths, s.dockerDataRoot)
	}
	if len(request.Files) > 0 {
		paths = append(paths, serviceFilesRoot)
	}
	if len(paths) > 0 {
		if !s.requireFreeDisk(w, paths...) {
			return
		}
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
	if err := validateServiceFilesRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.requireFreeDisk(w, serviceFilesRoot) {
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
	if request.Kind == "" {
		request.Kind = PortAllocationKindMeshUpstream
	}
	if err := validatePortAllocationRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.installation != nil {
		inventory, err := nodeidentity.ReadInventory(s.inventoryFile)
		if err != nil {
			http.Error(w, "trusted cluster inventory is unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		node, ok := inventory.Node(s.installation.NodeID)
		if inventory.ClusterID != s.installation.ClusterID || !ok || node.MeshIP == "" || node.MeshIP != request.HostIP {
			http.Error(w, "mesh allocation host IP is not assigned to this enrolled node", http.StatusBadRequest)
			return
		}
	}
	if !s.requireFreeDisk(w, s.dataDir) {
		return
	}
	response, err := AllocatePort(r.Context(), s.dataDir, request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.installation != nil {
		response.ClusterID = s.installation.ClusterID
		response.NodeID = s.installation.NodeID
		if err := SignPortAllocation(response, s.installation); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handleProxyFile(w http.ResponseWriter, r *http.Request) {
	s.proxyAuthorityMu.Lock()
	defer s.proxyAuthorityMu.Unlock()
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
		if !s.requireFreeDisk(w, proxyRoutesDir, proxyDynamicDir) {
			return
		}
		if s.installation != nil {
			if err := s.authorizeProxyFileCandidate(request.Name, request.Content); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := validateEnrolledProxyRouteManifest(request.Content, s.installation.ClusterID, s.inventoryFile); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		response, err = WriteProxyFile(r.Context(), request)
		if err != nil && s.installation != nil {
			if manifest, parseErr := ParseProxyRouteManifest(request.Content); parseErr == nil {
				_ = s.reconcileStoredProxyScope(manifest.Project, manifest.Environment, manifest.ClusterID)
			}
		}
	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		var removedManifest *ProxyRouteManifest
		if validated, validateErr := validateProxyFileName(name); validateErr == nil {
			if data, exists, readErr := readFileIfExists(filepath.Join(proxyRoutesDir, validated)); readErr == nil && exists {
				removedManifest, _ = ParseProxyRouteManifest(string(data))
			}
		}
		response, err = RemoveProxyFile(r.Context(), name)
		if err == nil && s.installation != nil && removedManifest != nil {
			err = s.reconcileStoredProxyScope(removedManifest.Project, removedManifest.Environment, removedManifest.ClusterID)
		}
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
	if !s.requireFreeDisk(w, proxyDynamicDir, proxyLogDir, proxyCertStoreDir, s.dockerDataRoot) {
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

func (s *Server) handleProxyCertificates(w http.ResponseWriter, r *http.Request) {
	var (
		response any
		err      error
	)
	switch r.Method {
	case http.MethodGet:
		response, err = ListProxyCertificates(r.Context())
	case http.MethodPut:
		defer r.Body.Close()
		var request ProxyCertificatePushRequest
		if decodeErr := decodeJSONRequest(w, r, &request); decodeErr != nil {
			http.Error(w, "invalid JSON body: "+decodeErr.Error(), http.StatusBadRequest)
			return
		}
		if !s.requireFreeDisk(w, proxyCertStoreDir) {
			return
		}
		response, err = PushProxyCertificate(r.Context(), request)
	case http.MethodDelete:
		response, err = RemoveProxyCertificate(r.Context(), r.URL.Query().Get("domain"))
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

func (s *Server) handleProxyACMEDNS(w http.ResponseWriter, r *http.Request) {
	var (
		response any
		err      error
		secrets  map[string]string
	)
	switch r.Method {
	case http.MethodPut:
		defer r.Body.Close()
		var request ACMEDNSReconcileRequest
		if decodeErr := decodeJSONRequest(w, r, &request); decodeErr != nil {
			http.Error(w, "invalid JSON body: "+decodeErr.Error(), http.StatusBadRequest)
			return
		}
		if !s.requireFreeDisk(w, proxyDynamicDir, proxyCertStoreDir) {
			return
		}
		secrets = request.Credentials
		response, err = PrepareACMEDNS(r.Context(), request)
	case http.MethodPost:
		if !s.requireFreeDisk(w, proxyDynamicDir, proxyCertStoreDir) {
			return
		}
		response, err = FinalizeACMEDNS(r.Context(), r.URL.Query().Get("project"), r.URL.Query().Get("environment"))
	case http.MethodDelete:
		response, err = RemoveACMEDNSConfiguration(r.Context(), r.URL.Query().Get("project"), r.URL.Query().Get("environment"))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		status := http.StatusBadRequest
		payload := map[string]any{"code": "invalid_request", "error": redactACMEDNSError(err, secrets).Error()}
		var operationErr *ACMEDNSError
		if errors.As(err, &operationErr) {
			status = http.StatusBadGateway
			if operationErr.Code == "cooldown" || operationErr.Code == "rate_limited" {
				status = http.StatusTooManyRequests
			}
			payload["code"] = operationErr.Code
			payload["domain"] = operationErr.Domain
			payload["retryAfter"] = operationErr.RetryAfter.UTC()
			if len(operationErr.Completed) > 0 {
				payload["completed"] = operationErr.Completed
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
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
		if !s.requireFreeDisk(w, s.dataDir) {
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
		if !s.requireFreeDisk(w, s.dataDir) {
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
		if !s.requireFreeDisk(w, s.dataDir) {
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
		if !s.requireFreeDisk(w, s.dataDir) {
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
		if err := validateBackupRequest(request, true, false); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !s.requireFreeDisk(w, backupRootDir) {
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
	if err := validateBackupRequest(request, true, true); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.requireFreeDisk(w, s.dockerDataRoot) {
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
		if err := validateBackupScheduleRequest(request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !s.requireFreeDisk(w, s.dataDir) {
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
	if !s.requireFreeDisk(w, s.dataDir) {
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
	if !s.requireFreeDisk(w, "/etc/wireguard") {
		return
	}

	response, err := EnsureMeshKey(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if s.installation != nil {
		inventory, inventoryErr := nodeidentity.ReadInventory(s.inventoryFile)
		if inventoryErr != nil || inventory == nil || inventory.ClusterID != s.installation.ClusterID {
			http.Error(w, "local WireGuard key does not match platform membership", http.StatusForbidden)
			return
		}
		node, ok := inventory.Node(s.installation.NodeID)
		if !ok || node.MeshPublicKey != strings.TrimSpace(response.PublicKey) {
			http.Error(w, "local WireGuard key does not match platform membership", http.StatusForbidden)
			return
		}
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
	if s.installation != nil {
		http.Error(w, "enrolled-node mesh topology is owned by platform membership", http.StatusForbidden)
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
	if !s.requireFreeDisk(w, "/etc/wireguard") {
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

func (s *Server) handleImageInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response, err := InspectImage(r.Context(), r.URL.Query().Get("image"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}

func (s *Server) handlePlatform(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response, err := ReadPlatform(r.Context())
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
	imageTransferMu.Lock()
	defer imageTransferMu.Unlock()
	expectedImageID := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("expectedImageId")))
	exportReference := image
	if expectedImageID != "" {
		if !validSHA256Digest(expectedImageID) {
			http.Error(w, "expectedImageId must be a sha256 digest", http.StatusBadRequest)
			return
		}
		descriptor, err := InspectImage(r.Context(), image)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if !descriptor.Exists || descriptor.ImageID != expectedImageID {
			http.Error(w, "image changed since it was selected for transfer", http.StatusConflict)
			return
		}
		exportReference = expectedImageID
	}
	writeImageTarHeaders(w, image)
	if err := ExportImage(r.Context(), exportReference, w); err != nil {
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
	expectedImageID := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("expectedImageId")))
	if expectedImageID != "" && !validSHA256Digest(expectedImageID) {
		http.Error(w, "expectedImageId must be a sha256 digest", http.StatusBadRequest)
		return
	}
	if r.ContentLength > defaultImageImportMaxBytes {
		http.Error(w, fmt.Sprintf("image import exceeds maximum size %d bytes", defaultImageImportMaxBytes), http.StatusRequestEntityTooLarge)
		return
	}
	releaseDisk, ok := s.reserveFreeDisk(w, uploadReservation(r.ContentLength, defaultImageImportMaxBytes), s.dockerDataRoot)
	if !ok {
		return
	}
	defer releaseDisk()
	clearReadDeadline := setUploadReadDeadline(w, s.uploadReadTimeout)
	defer clearReadDeadline()
	defer r.Body.Close()

	response, err := ImportImage(r.Context(), image, http.MaxBytesReader(w, r.Body, defaultImageImportMaxBytes), expectedImageID)
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
	releaseDisk, ok := s.reserveFreeDisk(w, uploadReservation(r.ContentLength, defaultBuildContextMaxBytes), s.dockerDataRoot)
	if !ok {
		return
	}
	defer releaseDisk()
	releaseBuild, ok := s.acquireBuildSlot()
	if !ok {
		http.Error(w, "maximum concurrent image builds are already running", http.StatusTooManyRequests)
		return
	}
	defer releaseBuild()
	clearReadDeadline := setUploadReadDeadline(w, s.uploadReadTimeout)
	defer clearReadDeadline()
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

func setUploadReadDeadline(w http.ResponseWriter, timeout time.Duration) func() {
	if timeout <= 0 {
		return func() {}
	}
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return func() {}
	}
	return func() { _ = controller.SetReadDeadline(time.Time{}) }
}

func (s *Server) requireFreeDisk(w http.ResponseWriter, paths ...string) bool {
	return s.requireFreeDiskFor(w, 0, paths...)
}

func uploadReservation(contentLength int64, maximum int64) int64 {
	if contentLength >= 0 {
		return contentLength
	}
	return maximum
}

func (s *Server) checkFreeDisk(requiredBytes int64, paths ...string) error {
	if s.minimumFreeDiskBytes <= 0 {
		return nil
	}
	if len(paths) == 0 {
		paths = []string{s.dataDir}
	}
	s.diskReservationMu.Lock()
	defer s.diskReservationMu.Unlock()
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		filesystem, err := s.diskIdentity(path)
		if err != nil {
			return fmt.Errorf("failed to identify filesystem for %s: %w", path, err)
		}
		if _, exists := seen[filesystem]; exists {
			continue
		}
		seen[filesystem] = struct{}{}
		available, err := s.diskAvailable(path)
		if err != nil {
			return fmt.Errorf("failed to verify free disk on %s: %w", path, err)
		}
		reserved := s.diskReservations[filesystem]
		if available < s.minimumFreeDiskBytes {
			return fmt.Errorf("operation denied on %s: %d free bytes is below %d-byte platform minimum", path, available, s.minimumFreeDiskBytes)
		}
		if requiredBytes == 0 && reserved > 0 {
			return fmt.Errorf("operation denied on %s while %d bytes are reserved by active uploads on the same filesystem", path, reserved)
		}
		if requiredBytes > available-s.minimumFreeDiskBytes-reserved {
			return fmt.Errorf("operation denied on %s: %d free bytes cannot preserve %d-byte platform minimum plus %d already-reserved and %d requested bytes", path, available, s.minimumFreeDiskBytes, reserved, requiredBytes)
		}
	}
	return nil
}

func (s *Server) requireFreeDiskFor(w http.ResponseWriter, requiredBytes int64, paths ...string) bool {
	err := s.checkFreeDisk(requiredBytes, paths...)
	if err == nil {
		return true
	}
	status := http.StatusInsufficientStorage
	if strings.HasPrefix(err.Error(), "failed to verify free disk") || strings.HasPrefix(err.Error(), "failed to identify filesystem") {
		status = http.StatusServiceUnavailable
	}
	http.Error(w, err.Error(), status)
	return false
}

func (s *Server) reserveFreeDisk(w http.ResponseWriter, requiredBytes int64, paths ...string) (func(), bool) {
	if s.minimumFreeDiskBytes <= 0 || requiredBytes <= 0 {
		if !s.requireFreeDiskFor(w, requiredBytes, paths...) {
			return nil, false
		}
		return func() {}, true
	}
	if len(paths) == 0 {
		paths = []string{s.dataDir}
	}
	s.diskReservationMu.Lock()
	defer s.diskReservationMu.Unlock()
	type diskTarget struct{ path, filesystem string }
	unique := make([]diskTarget, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		filesystem, err := s.diskIdentity(path)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to identify filesystem for %s: %v", path, err), http.StatusServiceUnavailable)
			return nil, false
		}
		if _, exists := seen[filesystem]; exists {
			continue
		}
		seen[filesystem] = struct{}{}
		available, err := s.diskAvailable(path)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to verify free disk on %s: %v", path, err), http.StatusServiceUnavailable)
			return nil, false
		}
		reserved := s.diskReservations[filesystem]
		if available < s.minimumFreeDiskBytes || requiredBytes > available-s.minimumFreeDiskBytes-reserved {
			http.Error(w, fmt.Sprintf("operation denied on %s: %d free bytes cannot preserve %d-byte platform minimum plus %d already-reserved and %d requested bytes", path, available, s.minimumFreeDiskBytes, reserved, requiredBytes), http.StatusInsufficientStorage)
			return nil, false
		}
		unique = append(unique, diskTarget{path: path, filesystem: filesystem})
	}
	for _, target := range unique {
		s.diskReservations[target.filesystem] += requiredBytes
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.diskReservationMu.Lock()
			defer s.diskReservationMu.Unlock()
			for _, target := range unique {
				s.diskReservations[target.filesystem] -= requiredBytes
				if s.diskReservations[target.filesystem] == 0 {
					delete(s.diskReservations, target.filesystem)
				}
			}
		})
	}, true
}

func (s *Server) acquireBuildSlot() (func(), bool) {
	if s.buildSlots == nil {
		return func() {}, true
	}
	select {
	case s.buildSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.buildSlots }) }, true
	default:
		return nil, false
	}
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
	if request.Mode == ExecModeOneOff && !s.requireFreeDisk(w, s.dockerDataRoot) {
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
	if !s.requireFreeDisk(w, s.dataDir, serviceFilesRoot) {
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
	if !isSafeProjectName(request.Project) || !isSafeRuntimeName(request.Environment) || !isSafeServiceName(request.Job) {
		http.Error(w, "invalid job trigger identity", http.StatusBadRequest)
		return
	}
	if !s.requireFreeDisk(w, s.dataDir, s.dockerDataRoot) {
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
		Runtime:         "takod",
		Version:         s.version,
		Capabilities:    []string{CapabilityContainerArgvV1, CapabilityContainerRuntimeControlsV1, CapabilityImageBuildOptionsV1, CapabilityImageDescriptorV1, CapabilityNodePlatformV1, CapabilityExecOneOffControlsV1, CapabilityServiceFilesV1, CapabilityProxyTrustedProxiesV1, CapabilityProxyCertsV1, CapabilityAcmeDNSV1, CapabilityNodeIdentityV1},
		Hostname:        hostname,
		Socket:          s.socket,
		DataDir:         s.dataDir,
		StartedAt:       s.startedAt,
		Now:             time.Now().UTC(),
		Identity:        cloneNodeIdentity(s.installation),
		EnrollmentRoles: cloneEnrollmentRoles(s.installation),
	}
	if s.installation != nil {
		if inventory, err := nodeidentity.ReadInventory(s.inventoryFile); err == nil && inventory.ClusterID == s.installation.ClusterID {
			if node, ok := inventory.Node(s.installation.NodeID); ok {
				membership := node
				membership.Roles = append([]string(nil), node.Roles...)
				status.Membership = &membership
				status.MembershipGeneration = inventory.Generation
				if inventory.ControllerNodeID != "" && inventory.Generation > 0 {
					status.Capabilities = append(status.Capabilities, CapabilityNodeLifecycleV1)
				}
			}
		}
	}
	if s.supportsMembershipController() {
		status.Capabilities = append(status.Capabilities, CapabilityNodeMembershipV1)
	}

	status.Node = readJSONMap(filepath.Join(s.dataDir, "node.json"))
	status.Peers = readJSONMap(filepath.Join(s.dataDir, "mesh", "peers.json"))
	return status
}

func cloneNodeIdentity(installation *nodeidentity.Installation) *nodeidentity.Identity {
	if installation == nil {
		return nil
	}
	clone := installation.Identity
	return &clone
}

func cloneEnrollmentRoles(installation *nodeidentity.Installation) []string {
	if installation == nil {
		return nil
	}
	return append([]string(nil), installation.EnrollmentRoles...)
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
