package takod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	RemoveProxyContainer   bool     `json:"removeProxyContainer,omitempty"`
	RemoveProxyRuntime     bool     `json:"removeProxyRuntime,omitempty"`
	PruneDocker            bool     `json:"pruneDocker,omitempty"`
	KeepImages             int      `json:"keepImages,omitempty"`
	CleanOldImages         bool     `json:"cleanOldImages,omitempty"`
	CleanStoppedContainers bool     `json:"cleanStoppedContainers,omitempty"`
	CleanDanglingImages    bool     `json:"cleanDanglingImages,omitempty"`
	CleanBuildCache        bool     `json:"cleanBuildCache,omitempty"`
	CleanUnusedVolumes     bool     `json:"cleanUnusedVolumes,omitempty"`
	SecureLogPermissions   bool     `json:"secureLogPermissions,omitempty"`
}

type CleanupResponse struct {
	Project               string   `json:"project"`
	Environment           string   `json:"environment,omitempty"`
	ContainersRemoved     int      `json:"containersRemoved,omitempty"`
	ImagesRemoved         int      `json:"imagesRemoved,omitempty"`
	NetworksRemoved       int      `json:"networksRemoved,omitempty"`
	ProxyFilesRemoved     int      `json:"proxyFilesRemoved,omitempty"`
	ProxyContainerRemoved bool     `json:"proxyContainerRemoved,omitempty"`
	BuildCacheCleaned     bool     `json:"buildCacheCleaned,omitempty"`
	UnusedVolumesCleaned  bool     `json:"unusedVolumesCleaned,omitempty"`
	LogPermissionsSecured bool     `json:"logPermissionsSecured,omitempty"`
	InitialDiskUsage      string   `json:"initialDiskUsage,omitempty"`
	FinalDiskUsage        string   `json:"finalDiskUsage,omitempty"`
	DockerDiskUsage       string   `json:"dockerDiskUsage,omitempty"`
	Warnings              []string `json:"warnings,omitempty"`
}

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
	if req.RemoveProxyContainer {
		if _, err := runDocker(ctx, "rm", "-f", "tako-proxy"); err != nil {
			warn("failed to remove tako-proxy: %v", err)
		} else {
			response.ProxyContainerRemoved = true
		}
	}
	if req.RemoveNetworks {
		count, err := cleanupNetworks(ctx, req.Project, req.Environment)
		if err != nil {
			warn("%v", err)
		}
		response.NetworksRemoved = count
	}
	if req.RemoveImages {
		count, err := cleanupImages(ctx, req.Project)
		if err != nil {
			warn("%v", err)
		}
		response.ImagesRemoved = count
	}
	if req.CleanOldImages {
		count, err := cleanupOldProjectImages(ctx, req.Project, req.KeepImages)
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
		if _, err := runDocker(ctx, "builder", "prune", "-f"); err != nil {
			warn("failed to clean Docker build cache: %v", err)
		} else {
			response.BuildCacheCleaned = true
		}
	}
	if req.CleanUnusedVolumes {
		if _, err := runDocker(ctx, "volume", "prune", "-f"); err != nil {
			warn("failed to clean unused Docker volumes: %v", err)
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
	if req.RemoveDeployFiles {
		removeFixedPath(filepath.Join("/opt", req.Project), "deployment files", warn)
	}
	if req.RemoveTakodState {
		cleanupTakodState(req.Project, req.Environment, warn)
	}
	if req.SecureLogPermissions {
		if err := secureProxyLogPermissions("/var/log/tako/proxy"); err != nil {
			warn("failed to secure proxy log permissions: %v", err)
		} else {
			response.LogPermissionsSecured = true
		}
	}
	if req.PruneDocker {
		if _, err := runDocker(ctx, "system", "prune", "-af", "--volumes"); err != nil {
			warn("failed to prune docker system: %v", err)
		}
	}
	if req.RemoveProxyRuntime {
		removeFixedPath("/var/log/tako/proxy", "proxy logs", warn)
		removeFixedPath("/etc/tako", "tako config", warn)
		removeFixedPath("/var/lib/tako", "takod state", warn)
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
			if name == fmt.Sprintf("tako_%s_%s", project, environment) {
				names = append(names, name)
			}
			continue
		}
		if strings.HasPrefix(name, "tako_"+project+"_") {
			names = append(names, name)
		}
	}
	removed := 0
	for _, name := range names {
		if _, err := runDocker(ctx, "network", "rm", name); err != nil {
			return removed, fmt.Errorf("failed to remove docker network %s: %w", name, err)
		}
		removed++
	}
	return removed, nil
}

func cleanupImages(ctx context.Context, project string) (int, error) {
	output, err := runDocker(ctx, "images", "--format", "{{.Repository}}\t{{.Tag}}")
	if err != nil {
		return 0, fmt.Errorf("failed to list docker images: %w", err)
	}
	var refs []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 2 {
			continue
		}
		repo, tag := fields[0], fields[1]
		if repo == "<none>" || tag == "<none>" || !imageRepositoryMatchesProject(repo, project) {
			continue
		}
		refs = append(refs, repo+":"+tag)
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

func cleanupOldProjectImages(ctx context.Context, project string, keepLatest int) (int, error) {
	output, err := runDocker(ctx, "images", "--format", "{{.ID}}\t{{.Repository}}\t{{.Tag}}")
	if err != nil {
		return 0, fmt.Errorf("failed to list docker images: %w", err)
	}

	var ids []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 3 {
			continue
		}
		id, repo, tag := fields[0], fields[1], fields[2]
		if id == "" || repo == "<none>" || tag == "<none>" || !imageRepositoryMatchesProject(repo, project) || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) <= keepLatest {
		return 0, nil
	}

	removeIDs := ids[keepLatest:]
	removeArgs := append([]string{"rmi", "-f"}, removeIDs...)
	if _, err := runDocker(ctx, removeArgs...); err != nil {
		return 0, fmt.Errorf("failed to remove old project images: %w", err)
	}
	return len(removeIDs), nil
}

func cleanupDanglingImages(ctx context.Context) (int, error) {
	output, err := runDocker(ctx, "images", "-f", "dangling=true", "-q")
	if err != nil {
		return 0, fmt.Errorf("failed to list dangling docker images: %w", err)
	}
	ids := uniqueFields(output)
	if len(ids) == 0 {
		return 0, nil
	}
	removeArgs := append([]string{"rmi"}, ids...)
	if _, err := runDocker(ctx, removeArgs...); err != nil {
		return 0, fmt.Errorf("failed to remove dangling docker images: %w", err)
	}
	return len(ids), nil
}

func imageRepositoryMatchesProject(repository string, project string) bool {
	if repository == project || strings.HasPrefix(repository, project+"/") {
		return true
	}
	for _, segment := range strings.Split(repository, "/") {
		if segment == project {
			return true
		}
	}
	return false
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
