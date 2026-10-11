package takod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

type CleanupRequest struct {
	Project                string   `json:"project"`
	Environment            string   `json:"environment,omitempty"`
	RemoveContainers       bool     `json:"removeContainers,omitempty"`
	RemoveImages           bool     `json:"removeImages,omitempty"`
	RemoveNetworks         bool     `json:"removeNetworks,omitempty"`
	RemoveDeployFiles      bool     `json:"removeDeployFiles,omitempty"`
	RemoveTakodState       bool     `json:"removeTakodState,omitempty"`
	ProxyFiles             []string `json:"proxyFiles,omitempty"`
	ExternalVolumes        []string `json:"externalVolumes,omitempty"`
	PruneDocker            bool     `json:"pruneDocker,omitempty"`
	ImageRepositories      []string `json:"imageRepositories,omitempty"`
	KeepImages             int      `json:"keepImages,omitempty"`
	CleanOldImages         bool     `json:"cleanOldImages,omitempty"`
	CleanStoppedContainers bool     `json:"cleanStoppedContainers,omitempty"`
	CleanDanglingImages    bool     `json:"cleanDanglingImages,omitempty"`
	CleanBuildCache        bool     `json:"cleanBuildCache,omitempty"`
	BuildCacheKeepStorage  string   `json:"buildCacheKeepStorage,omitempty"`
	CleanUnusedVolumes     bool     `json:"cleanUnusedVolumes,omitempty"`
	SecureLogPermissions   bool     `json:"secureLogPermissions,omitempty"`
	// DryRun (node.disk-hygiene-v1) reports what old-image cleanup would
	// remove and why the rest is kept, and changes nothing. It accepts only
	// old-image cleanup.
	DryRun bool `json:"dryRun,omitempty"`
	// Trigger names the caller of an old-image cleanup in the hygiene
	// journal: "deploy" or "cleanup" (the default).
	Trigger string `json:"trigger,omitempty"`
	// RemovePreviewData tears down one preview environment in a fixed order
	// behind the preview guard; see cleanupPreviewEnvironment. It requires
	// RemoveContainers, RemoveNetworks and RemoveTakodState and rejects image,
	// deploy-file and maintenance cleanup.
	RemovePreviewData bool `json:"removePreviewData,omitempty"`
	// PreviewUpdatedAt, with RemovePreviewData, is the updatedAt of the
	// preview record the caller read under the preview's lease. The guard
	// refuses the teardown when the node's record differs.
	PreviewUpdatedAt *time.Time `json:"previewUpdatedAt,omitempty"`
	// DataDir is the takod data directory. The server sets it; it is never
	// read from the request body.
	DataDir string `json:"-"`
	// BeforeRecordRemoval, with RemovePreviewData, runs after the preview's
	// runtime, data and state are gone and before its record is deleted.
	// The server sets it to remove scheduler, alert and port state; it is
	// never read from the request body.
	BeforeRecordRemoval func(ctx context.Context, response *CleanupResponse) error `json:"-"`
}

type CleanupResponse struct {
	Project               string `json:"project"`
	Environment           string `json:"environment,omitempty"`
	ContainersRemoved     int    `json:"containersRemoved,omitempty"`
	ImagesRemoved         int    `json:"imagesRemoved,omitempty"`
	NetworksRemoved       int    `json:"networksRemoved,omitempty"`
	JobsRemoved           int    `json:"jobsRemoved,omitempty"`
	ProxyFilesRemoved     int    `json:"proxyFilesRemoved,omitempty"`
	BuildCacheCleaned     bool   `json:"buildCacheCleaned,omitempty"`
	UnusedVolumesCleaned  bool   `json:"unusedVolumesCleaned,omitempty"`
	LogPermissionsSecured bool   `json:"logPermissionsSecured,omitempty"`
	InitialDiskUsage      string `json:"initialDiskUsage,omitempty"`
	FinalDiskUsage        string `json:"finalDiskUsage,omitempty"`
	DockerDiskUsage       string `json:"dockerDiskUsage,omitempty"`
	VolumesRemoved        int    `json:"volumesRemoved,omitempty"`
	PreviewRecordRemoved  bool   `json:"previewRecordRemoved,omitempty"`
	// PreviewNotFound reports a preview teardown that found neither a
	// preview record nor labeled volumes and changed nothing.
	PreviewNotFound bool     `json:"previewNotFound,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
	// Hygiene is the old-image report (node.disk-hygiene-v1): removed or,
	// with DryRun, removable images, kept counts by reason and bytes freed.
	Hygiene *CleanupHygiene `json:"hygiene,omitempty"`
}

const DefaultBuildCacheKeepStorage = "20GB"

const composeProjectImageLabel = "com.docker.compose.project"

// CleanupProject runs cleanup without a takod data directory. Old-image
// cleanup needs the node's deployment state and is skipped with a warning;
// takod's API handler uses CleanupProjectWithDataDir.
func CleanupProject(ctx context.Context, req CleanupRequest) (*CleanupResponse, error) {
	return CleanupProjectWithDataDir(ctx, "", req)
}

// CleanupProjectWithDataDir runs cleanup using dataDir as the source of
// replicated deployment state for image ownership and protection.
// The server-set req.DataDir is used when dataDir is empty; otherwise
// dataDir wins, so a preview teardown and image cleanup read the same
// directory.
func CleanupProjectWithDataDir(ctx context.Context, dataDir string, req CleanupRequest) (*CleanupResponse, error) {
	if dataDir == "" {
		dataDir = req.DataDir
	} else {
		req.DataDir = dataDir
	}
	if err := validateCleanupRequest(req); err != nil {
		return nil, err
	}
	if req.RemovePreviewData {
		return cleanupPreviewEnvironment(ctx, req)
	}
	response := &CleanupResponse{
		Project:     req.Project,
		Environment: req.Environment,
	}
	warn := func(format string, args ...any) {
		response.Warnings = append(response.Warnings, fmt.Sprintf(format, args...))
	}
	if req.DryRun {
		report, err := cleanupOldProjectImagesWithOptions(ctx, dataDir, req.Project, req.Environment, req.KeepImages, req.ImageRepositories, imageCleanupOptions{DryRun: true})
		if err != nil {
			warn("%v", err)
		}
		response.Hygiene = &report
		return response, nil
	}
	// The repositories of every accepted scoped old-image cleanup are what
	// the autonomous image GC may later select from (hygiene_scopes.go).
	if req.CleanOldImages && req.Environment != "" && dataDir != "" {
		if err := recordHygieneScope(dataDir, req.Project, req.Environment, req.ImageRepositories, time.Now().UTC()); err != nil {
			warn("failed to record image hygiene scope: %v", err)
		}
	}
	if req.includesMaintenanceCleanup() {
		response.InitialDiskUsage = diskUsage(ctx)
	}

	// Public routes must be withdrawn and activated before service processes
	// begin draining. A failed withdrawal aborts cleanup with runtime state
	// intact so the operation can be retried safely.
	if err := RemoveProxyFiles(ctx, req.ProxyFiles); err != nil {
		return response, fmt.Errorf("failed to withdraw proxy files: %w", err)
	}
	response.ProxyFilesRemoved = len(req.ProxyFiles)
	if req.Environment != "" && slices.Contains(req.ProxyFiles, runtimeid.ProxyConfigFileName(req.Project, req.Environment)) {
		if _, err := RemoveACMEDNSConfiguration(ctx, req.Project, req.Environment); err != nil {
			warn("failed to remove proxy ACME ownership: %v", err)
		}
	}

	if req.RemoveContainers {
		// Self-healing must never restart a container that is being torn
		// down: drop the converged records (waiting for an in-flight heal
		// action) before any container is removed.
		if err := healClearScope(dataDir, req.Project, req.Environment); err != nil {
			return response, fmt.Errorf("failed to clear self-healing state: %w", err)
		}
		count, err := cleanupContainers(ctx, req.Project, req.Environment)
		if err != nil {
			return response, err
		}
		response.ContainersRemoved = count
	}
	if req.RemoveNetworks {
		count, err := cleanupNetworks(ctx, req.Project, req.Environment)
		if err != nil {
			warn("%v", err)
		}
		response.NetworksRemoved = count
	}
	if req.RemoveImages {
		count, err := cleanupImages(ctx, req.Project, req.ImageRepositories)
		if err != nil {
			warn("%v", err)
		}
		response.ImagesRemoved = count
	}
	// Stopped containers go first so their images are no longer in use when
	// old-image cleanup checks container usage.
	if req.CleanStoppedContainers {
		count, err := cleanupStoppedContainers(ctx, req.Project, req.Environment)
		if err != nil {
			warn("%v", err)
		}
		response.ContainersRemoved += count
	}
	if req.CleanOldImages {
		report, err := cleanupOldProjectImagesWithOptions(ctx, dataDir, req.Project, req.Environment, req.KeepImages, req.ImageRepositories, imageCleanupOptions{})
		if err != nil {
			warn("%v", err)
		}
		response.ImagesRemoved += len(report.Removed)
		response.Hygiene = &report
	}
	if req.CleanDanglingImages {
		count, err := cleanupDanglingImages(ctx)
		if err != nil {
			warn("%v", err)
		}
		response.ImagesRemoved += count
	}
	if req.CleanBuildCache {
		if _, err := cleanupBuildCache(ctx, req.BuildCacheKeepStorage); err != nil {
			warn("failed to clean Docker build cache: %v", err)
		} else {
			response.BuildCacheCleaned = true
		}
	}
	if req.CleanUnusedVolumes {
		if _, err := cleanupUnusedProjectVolumes(ctx, req.Project, req.Environment, req.ExternalVolumes); err != nil {
			warn("failed to clean unused project volumes: %v", err)
		} else {
			response.UnusedVolumesCleaned = true
		}
	}
	if req.RemoveDeployFiles {
		removeFixedPath(filepath.Join("/opt", req.Project), "deployment files", warn)
		filesPath := filepath.Join(serviceFilesRoot, req.Project)
		if req.Environment != "" {
			filesPath = filepath.Join(filesPath, req.Environment)
		}
		removeFixedPath(filesPath, "operator files", warn)
	}
	if req.RemoveTakodState {
		cleanupTakodState(req.Project, req.Environment, warn)
		if dataDir != "" {
			if err := removeHygieneScope(dataDir, req.Project, req.Environment); err != nil {
				warn("failed to remove image hygiene scope: %v", err)
			}
		}
	}
	if req.SecureLogPermissions {
		if err := secureProxyLogPermissions(proxyLogDir); err != nil {
			warn("failed to secure proxy log permissions: %v", err)
		} else {
			response.LogPermissionsSecured = true
		}
	}
	if req.PruneDocker {
		runScopedProjectPrune(ctx, req.Project, req.Environment, req.ExternalVolumes, warn, response)
	}
	if req.includesMaintenanceCleanup() {
		response.FinalDiskUsage = diskUsage(ctx)
		response.DockerDiskUsage = dockerDiskUsage(ctx)
	}

	return response, nil
}

func validateCleanupRequest(req CleanupRequest) error {
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if req.Environment != "" && !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	for _, name := range req.ProxyFiles {
		if _, err := validateProxyFileName(name); err != nil {
			return fmt.Errorf("invalid proxy file %q: %w", name, err)
		}
	}
	for _, repository := range req.ImageRepositories {
		if !isSafeImageRepository(repository) {
			return fmt.Errorf("invalid image repository %q", repository)
		}
	}
	for _, volume := range req.ExternalVolumes {
		if !isSafeDockerVolumeName(volume) {
			return fmt.Errorf("invalid external volume name")
		}
	}
	if req.BuildCacheKeepStorage != "" && !isSafeBuildCacheKeepStorage(req.BuildCacheKeepStorage) {
		return fmt.Errorf("invalid build cache keep storage")
	}
	if req.KeepImages < 0 {
		return fmt.Errorf("keepImages cannot be negative")
	}
	switch req.Trigger {
	case "", HygieneTriggerDeploy, HygieneTriggerCleanup:
	default:
		return fmt.Errorf("invalid cleanup trigger")
	}
	if req.DryRun {
		if req.Environment == "" {
			return fmt.Errorf("dryRun requires an environment")
		}
		if req.RemoveContainers || req.RemoveImages || req.RemoveNetworks || req.RemoveDeployFiles || req.RemoveTakodState ||
			len(req.ProxyFiles) > 0 || req.PruneDocker || req.CleanStoppedContainers || req.CleanDanglingImages ||
			req.CleanBuildCache || req.CleanUnusedVolumes || req.SecureLogPermissions || req.RemovePreviewData {
			return fmt.Errorf("dryRun supports only old-image cleanup")
		}
	}
	return validatePreviewTeardownRequest(req)
}

func (req CleanupRequest) includesMaintenanceCleanup() bool {
	return req.CleanOldImages ||
		req.CleanStoppedContainers ||
		req.CleanDanglingImages ||
		req.CleanBuildCache ||
		req.CleanUnusedVolumes ||
		req.SecureLogPermissions ||
		req.PruneDocker
}

func cleanupContainers(ctx context.Context, project string, environment string) (int, error) {
	args := []string{
		"ps", "-aq",
		"--filter", "label=tako.project=" + project,
	}
	if environment != "" {
		args = append(args, "--filter", "label=tako.environment="+environment)
	}
	output, err := runDocker(ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to list project containers: %w", err)
	}
	ids := strings.Fields(strings.TrimSpace(output))
	if len(ids) == 0 {
		return 0, nil
	}
	return removeContainerIDs(ctx, ids)
}

func cleanupStoppedContainers(ctx context.Context, project string, environment string) (int, error) {
	args := []string{
		"ps", "-aq",
		"--filter", "label=tako.project=" + project,
		"--filter", "status=exited",
	}
	if environment != "" {
		args = append(args, "--filter", "label=tako.environment="+environment)
	}
	output, err := runDocker(ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to list stopped project containers: %w", err)
	}
	ids := strings.Fields(strings.TrimSpace(output))
	if len(ids) == 0 {
		return 0, nil
	}
	removeArgs := append([]string{"rm"}, ids...)
	if _, err := runDocker(ctx, removeArgs...); err != nil {
		return 0, fmt.Errorf("failed to remove stopped project containers: %w", err)
	}
	return len(ids), nil
}

func cleanupNetworks(ctx context.Context, project string, environment string) (int, error) {
	var names []string
	if environment != "" {
		scoped, err := environmentNetworkNames(ctx, project, environment)
		if err != nil {
			return 0, err
		}
		names = scoped
	} else {
		output, err := runDocker(ctx, "network", "ls", "--format", "{{.Name}}")
		if err != nil {
			return 0, fmt.Errorf("failed to list docker networks: %w", err)
		}
		for _, line := range strings.Split(output, "\n") {
			name := strings.TrimSpace(line)
			if name != "" && strings.HasPrefix(name, runtimeid.NetworkProjectPrefix(project)) {
				names = append(names, name)
			}
		}
	}
	removed := 0
	for _, name := range names {
		disconnectProxyFromNetwork(ctx, name)
		if _, err := runDocker(ctx, "network", "rm", name); err != nil {
			return removed, fmt.Errorf("failed to remove docker network %s: %w", name, err)
		}
		removed++
	}
	return removed, nil
}

// environmentNetworkNames returns the networks owned by exactly one
// environment. A name prefix is not enough: the prefix of environment
// "preview" is also a prefix of "preview-pr-42". The main network matches
// its exact name; an export network must carry the environment's project
// and environment labels and have exactly the name derived from its
// service label. Unlabeled legacy export networks are left alone.
func environmentNetworkNames(ctx context.Context, project string, environment string) ([]string, error) {
	output, err := runDocker(ctx, "network", "ls", "--format",
		"{{.Name}}\t{{.Label \"tako.project\"}}\t{{.Label \"tako.environment\"}}\t{{.Label \"tako.service\"}}")
	if err != nil {
		return nil, fmt.Errorf("failed to list docker networks: %w", err)
	}
	main := runtimeid.NetworkName(project, environment)
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		name := strings.TrimSpace(fields[0])
		if name == "" || seen[name] {
			continue
		}
		label := func(index int) string {
			if index >= len(fields) {
				return ""
			}
			value := strings.TrimSpace(fields[index])
			if value == "<no value>" {
				return ""
			}
			return value
		}
		matched := name == main
		if !matched && label(1) == project && label(2) == environment && label(3) != "" {
			matched = name == runtimeid.ExportNetworkName(project, environment, label(3))
		}
		if matched {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}

func disconnectProxyFromNetwork(ctx context.Context, network string) {
	_, _ = runDocker(ctx, "network", "disconnect", "-f", network, "tako-proxy")
}

func cleanupImages(ctx context.Context, project string, imageRepositories []string) (int, error) {
	if len(imageRepositories) == 0 {
		return 0, nil
	}
	output, err := runDocker(ctx, "images", "--filter", "label!="+composeProjectImageLabel, "--format", "{{.ID}}\t{{.Repository}}\t{{.Tag}}")
	if err != nil {
		return 0, fmt.Errorf("failed to list docker images: %w", err)
	}
	var refs []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			continue
		}
		id, repo, tag := fields[0], fields[1], fields[2]
		ref := repo + ":" + tag
		if id == "" || seen[ref] || repo == "<none>" || tag == "<none>" || !imageRepositoryMatchesProject(repo, project, imageRepositories) {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return 0, nil
	}
	args := append([]string{"rmi", "-f"}, refs...)
	if _, err := runDocker(ctx, args...); err != nil {
		return 0, fmt.Errorf("failed to remove project images: %w", err)
	}
	return len(refs), nil
}

func cleanupDanglingImages(ctx context.Context) (int, error) {
	// Docker's negative label filter keeps Compose-built images out of both
	// the preflight list and the global dangling-image prune. Compose images
	// can be dangling during an upstream upgrade and are not Tako-owned.
	labelExclusion := "label!=" + composeProjectImageLabel
	output, err := runDocker(ctx, "images", "-f", "dangling=true", "--filter", labelExclusion, "-q")
	if err != nil {
		return 0, fmt.Errorf("failed to list dangling docker images: %w", err)
	}
	ids := uniqueFields(output)
	if len(ids) == 0 {
		return 0, nil
	}
	pruneOutput, err := runDocker(ctx, "image", "prune", "-f", "--filter", labelExclusion)
	if err != nil {
		return 0, fmt.Errorf("failed to prune dangling docker images: %w, output: %s", err, strings.TrimSpace(pruneOutput))
	}
	return countDockerImagePruneEntries(pruneOutput), nil
}

func cleanupBuildCache(ctx context.Context, keepStorage string) (string, error) {
	keepStorage = strings.TrimSpace(keepStorage)
	if keepStorage == "" {
		keepStorage = DefaultBuildCacheKeepStorage
	}
	if !isSafeBuildCacheKeepStorage(keepStorage) {
		return "", fmt.Errorf("invalid build cache keep storage")
	}
	return runDocker(ctx, "builder", "prune", "-f", "--keep-storage", keepStorage)
}

func countDockerImagePruneEntries(output string) int {
	count := 0
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "deleted:") || strings.HasPrefix(line, "untagged:") {
			count++
		}
	}
	return count
}

func isSafeBuildCacheKeepStorage(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 32 || strings.HasPrefix(value, "-") {
		return false
	}
	hasDigit := false
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '.':
		default:
			return false
		}
	}
	return hasDigit
}

func cleanupUnusedProjectVolumes(ctx context.Context, _ string, _ string, _ []string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("automatic named-volume deletion is disabled because Docker provides no atomic identity-checked delete; remove verified volumes explicitly")
}

func runScopedProjectPrune(ctx context.Context, project string, environment string, protectedVolumes []string, warn func(string, ...any), response *CleanupResponse) {
	if count, err := cleanupStoppedContainers(ctx, project, environment); err != nil {
		warn("failed to remove stopped project containers during scoped prune: %v", err)
	} else {
		response.ContainersRemoved += count
	}
	if _, err := cleanupUnusedProjectVolumes(ctx, project, environment, protectedVolumes); err != nil {
		warn("failed to remove unused project volumes during scoped prune: %v", err)
	} else {
		response.UnusedVolumesCleaned = true
	}
}

func imageRepositoryMatchesProject(repository string, _ string, allowedRepositories []string) bool {
	for _, allowed := range allowedRepositories {
		if repository == allowed {
			return true
		}
	}
	return false
}

func isSafeImageRepository(repository string) bool {
	if strings.TrimSpace(repository) == "" || len(repository) > maxImageRefLength {
		return false
	}
	if strings.HasPrefix(repository, "-") || strings.Contains(repository, "@") {
		return false
	}
	if imageRepositoryFromRef(repository) != repository {
		return false
	}
	for _, r := range repository {
		if r < 0x20 || r == 0x7f || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func imageRepositoryFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if digest := strings.Index(ref, "@"); digest >= 0 {
		ref = ref[:digest]
	}
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		ref = ref[:lastColon]
	}
	return ref
}

func uniqueFields(output string) []string {
	seen := make(map[string]bool)
	fields := strings.Fields(strings.TrimSpace(output))
	values := make([]string, 0, len(fields))
	for _, field := range fields {
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		values = append(values, field)
	}
	return values
}

func cleanupTakodState(project string, environment string, warn func(string, ...any)) {
	if environment != "" {
		for _, path := range []string{
			filepath.Join("/var/lib/tako/desired", project, environment),
			filepath.Join("/var/lib/tako/actual", project, environment),
			filepath.Join("/var/lib/tako/events", project, environment+".jsonl"),
		} {
			removeFixedPath(path, "takod state", warn)
		}
		return
	}

	for _, path := range []string{
		filepath.Join("/var/lib/tako/desired", project),
		filepath.Join("/var/lib/tako/actual", project),
		filepath.Join("/var/lib/tako/events", project),
	} {
		removeFixedPath(path, "takod state", warn)
	}
}

func removeFixedPath(path string, label string, warn func(string, ...any)) {
	if err := os.RemoveAll(path); err != nil {
		warn("failed to remove %s at %s: %v", label, path, err)
	}
}

func secureProxyLogPermissions(root string) error {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := os.Chown(path, 0, 0); err != nil && !os.IsPermission(err) {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0750)
		}
		return os.Chmod(path, 0640)
	})
}

func diskUsage(ctx context.Context) string {
	output, err := runHostCommand(ctx, "df", "-h", "/")
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

func dockerDiskUsage(ctx context.Context) string {
	output, err := runDocker(ctx, "system", "df")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

func runHostCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := dockerCommandContext(ctx, name, args...)
	output := newCappedOutputBuffer(defaultCommandOutputMaxBytes)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	return output.String(), err
}

func isSafeProjectName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	last := name[len(name)-1]
	if !((last >= 'a' && last <= 'z') || (last >= '0' && last <= '9')) {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

func isSafeRuntimeName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}
