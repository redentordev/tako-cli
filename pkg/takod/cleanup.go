package takod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type CleanupRequest struct {
	Project              string   `json:"project"`
	Environment          string   `json:"environment,omitempty"`
	RemoveContainers     bool     `json:"removeContainers,omitempty"`
	RemoveImages         bool     `json:"removeImages,omitempty"`
	RemoveNetworks       bool     `json:"removeNetworks,omitempty"`
	RemoveDeployFiles    bool     `json:"removeDeployFiles,omitempty"`
	RemoveState          bool     `json:"removeState,omitempty"`
	RemoveTakodState     bool     `json:"removeTakodState,omitempty"`
	ProxyFiles           []string `json:"proxyFiles,omitempty"`
	RemoveProxyContainer bool     `json:"removeProxyContainer,omitempty"`
	RemoveProxyRuntime   bool     `json:"removeProxyRuntime,omitempty"`
	PruneDocker          bool     `json:"pruneDocker,omitempty"`
}

type CleanupResponse struct {
	Project               string   `json:"project"`
	Environment           string   `json:"environment,omitempty"`
	ContainersRemoved     int      `json:"containersRemoved,omitempty"`
	ImagesRemoved         int      `json:"imagesRemoved,omitempty"`
	NetworksRemoved       int      `json:"networksRemoved,omitempty"`
	ProxyFilesRemoved     int      `json:"proxyFilesRemoved,omitempty"`
	ProxyContainerRemoved bool     `json:"proxyContainerRemoved,omitempty"`
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
	if req.RemoveState {
		removeFixedPath(filepath.Join("/var/lib/tako-cli", req.Project), "deployment state", warn)
	}
	if req.RemoveTakodState {
		cleanupTakodState(req.Project, req.Environment, warn)
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
