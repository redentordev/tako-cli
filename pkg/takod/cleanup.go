package takod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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
}

type CleanupResponse struct {
	Project               string   `json:"project"`
	Environment           string   `json:"environment,omitempty"`
	ContainersRemoved     int      `json:"containersRemoved,omitempty"`
	ImagesRemoved         int      `json:"imagesRemoved,omitempty"`
	NetworksRemoved       int      `json:"networksRemoved,omitempty"`
	JobsRemoved           int      `json:"jobsRemoved,omitempty"`
	ProxyFilesRemoved     int      `json:"proxyFilesRemoved,omitempty"`
	BuildCacheCleaned     bool     `json:"buildCacheCleaned,omitempty"`
	UnusedVolumesCleaned  bool     `json:"unusedVolumesCleaned,omitempty"`
	LogPermissionsSecured bool     `json:"logPermissionsSecured,omitempty"`
	InitialDiskUsage      string   `json:"initialDiskUsage,omitempty"`
	FinalDiskUsage        string   `json:"finalDiskUsage,omitempty"`
	DockerDiskUsage       string   `json:"dockerDiskUsage,omitempty"`
	Warnings              []string `json:"warnings,omitempty"`
}

const DefaultBuildCacheKeepStorage = "20GB"

const composeProjectImageLabel = "com.docker.compose.project"

func CleanupProject(ctx context.Context, req CleanupRequest) (*CleanupResponse, error) {
	if err := validateCleanupRequest(req); err != nil {
		return nil, err
	}
	response := &CleanupResponse{
		Project:     req.Project,
		Environment: req.Environment,
	}
	warn := func(format string, args ...any) {
		response.Warnings = append(response.Warnings, fmt.Sprintf(format, args...))
	}
	if req.KeepImages < 0 {
		return nil, fmt.Errorf("keepImages cannot be negative")
	}

	if req.includesMaintenanceCleanup() {
		response.InitialDiskUsage = diskUsage(ctx)
	}

	if req.RemoveContainers {
		count, err := cleanupContainers(ctx, req.Project, req.Environment)
		if err != nil {
			warn("%v", err)
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
	if req.CleanOldImages {
		count, err := cleanupOldProjectImages(ctx, req.Project, req.KeepImages, req.ImageRepositories)
		if err != nil {
			warn("%v", err)
		}
		response.ImagesRemoved += count
	}
	if req.CleanStoppedContainers {
		count, err := cleanupStoppedContainers(ctx, req.Project, req.Environment)
		if err != nil {
			warn("%v", err)
		}
		response.ContainersRemoved += count
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
	for _, name := range req.ProxyFiles {
		if _, err := RemoveProxyFile(ctx, name); err != nil {
			warn("failed to remove proxy file %s: %v", name, err)
			continue
		}
		response.ProxyFilesRemoved++
	}
	if req.Environment != "" && slices.Contains(req.ProxyFiles, runtimeid.ProxyConfigFileName(req.Project, req.Environment)) {
		if _, err := RemoveACMEDNSConfiguration(ctx, req.Project, req.Environment); err != nil {
			warn("failed to remove proxy ACME ownership: %v", err)
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
	return nil
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
	removeArgs := append([]string{"rm", "-f"}, ids...)
	if _, err := runDocker(ctx, removeArgs...); err != nil {
		return 0, fmt.Errorf("failed to remove project containers: %w", err)
	}
	return len(ids), nil
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
	output, err := runDocker(ctx, "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return 0, fmt.Errorf("failed to list docker networks: %w", err)
	}
	var names []string
	for _, line := range strings.Split(output, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if environment != "" {
			if strings.HasPrefix(name, runtimeid.NetworkEnvironmentPrefix(project, environment)) {
				names = append(names, name)
			}
			continue
		}
		if strings.HasPrefix(name, runtimeid.NetworkProjectPrefix(project)) {
			names = append(names, name)
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

func cleanupOldProjectImages(ctx context.Context, project string, keepLatest int, imageRepositories []string) (int, error) {
	if len(imageRepositories) == 0 {
		return 0, nil
	}
	output, err := runDocker(ctx, "images", "--filter", "label!="+composeProjectImageLabel, "--format", "{{.ID}}\t{{.Repository}}\t{{.Tag}}")
	if err != nil {
		return 0, fmt.Errorf("failed to list docker images: %w", err)
	}

	var ids []string
	refsByID := make(map[string][]string)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 3 {
			continue
		}
		id, repo, tag := fields[0], fields[1], fields[2]
		if id == "" || repo == "<none>" || tag == "<none>" || !imageRepositoryMatchesProject(repo, project, imageRepositories) {
			continue
		}
		if _, exists := refsByID[id]; !exists {
			ids = append(ids, id)
		}
		refsByID[id] = append(refsByID[id], repo+":"+tag)
	}
	if len(ids) <= keepLatest {
		return 0, nil
	}

	removeIDs := ids[keepLatest:]
	var removeRefs []string
	for _, id := range removeIDs {
		removeRefs = append(removeRefs, refsByID[id]...)
	}
	removeArgs := append([]string{"rmi", "-f"}, removeRefs...)
	if _, err := runDocker(ctx, removeArgs...); err != nil {
		return 0, fmt.Errorf("failed to remove old project images: %w", err)
	}
	return len(removeIDs), nil
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

func cleanupUnusedProjectVolumes(ctx context.Context, project string, environment string, protectedVolumes []string) (int, error) {
	output, err := runDocker(ctx, "volume", "ls", "--format", "{{.Name}}")
	if err != nil {
		return 0, fmt.Errorf("failed to list docker volumes: %w", err)
	}
	protected := make(map[string]bool, len(protectedVolumes))
	for _, volume := range protectedVolumes {
		protected[volume] = true
	}
	var names []string
	for _, line := range strings.Split(output, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || !volumeNameMatchesProjectEnvironment(name, project, environment) {
			continue
		}
		if protected[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	removed := 0
	for _, name := range names {
		output, err := runDocker(ctx, "volume", "rm", name)
		if err != nil {
			if dockerVolumeInUse(output) {
				continue
			}
			return removed, fmt.Errorf("failed to remove docker volume %s: %w", name, err)
		}
		removed++
	}
	return removed, nil
}

func volumeNameMatchesProjectEnvironment(name string, project string, environment string) bool {
	if environment != "" {
		return strings.HasPrefix(name, runtimeid.VolumeEnvironmentPrefix(project, environment))
	}
	return strings.HasPrefix(name, runtimeid.VolumeProjectPrefix(project))
}

func dockerVolumeInUse(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "volume is in use") ||
		strings.Contains(lower, "volume is still in use") ||
		strings.Contains(lower, "is in use")
}

func runScopedProjectPrune(ctx context.Context, project string, environment string, protectedVolumes []string, warn func(string, ...any), response *CleanupResponse) {
	if count, err := cleanupStoppedContainers(ctx, project, environment); err != nil {
		warn("failed to remove stopped project containers during scoped prune: %v", err)
	} else {
		response.ContainersRemoved += count
	}
	if environment == "" {
		if count, err := cleanupOldProjectImages(ctx, project, 0, nil); err != nil {
			warn("failed to remove project images during scoped prune: %v", err)
		} else {
			response.ImagesRemoved += count
		}
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
