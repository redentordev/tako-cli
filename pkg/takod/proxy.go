package takod

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	defaultProxyImage = "traefik:v3.6.1"
	defaultProxyEmail = "tako@redentor.dev"
)

var (
	proxyAcmeDir = "/etc/tako/proxy/acme"
	proxyLogDir  = "/var/log/tako/proxy"
)

type ReconcileProxyRequest struct {
	Network string `json:"network"`
	Email   string `json:"email,omitempty"`
	Image   string `json:"image,omitempty"`
}

type ReconcileProxyResponse struct {
	Container string `json:"container"`
	Image     string `json:"image"`
}

type DisableProxyResponse struct {
	Container  string   `json:"container"`
	Removed    bool     `json:"removed"`
	RouteFiles []string `json:"routeFiles,omitempty"`
}

func ReconcileProxy(ctx context.Context, req ReconcileProxyRequest) (*ReconcileProxyResponse, error) {
	normalizeReconcileProxyRequest(&req)
	if err := validateReconcileProxyRequest(req); err != nil {
		return nil, err
	}
	if err := ensureDockerNetwork(ctx, req.Network, dockerNetworkOwner{}); err != nil {
		return nil, err
	}
	if err := ensureProxyDirectories(); err != nil {
		return nil, err
	}

	running, _ := runDocker(ctx, "ps", "--filter", "name=^tako-proxy$", "--format", "{{.Names}}")
	if strings.TrimSpace(running) == "tako-proxy" {
		args, _ := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{json .Args}}")
		if strings.Contains(args, "--providers.file.directory=/etc/traefik/dynamic") && proxyHasPublicRuntimePorts(ctx) {
			_, _ = runDocker(ctx, "network", "connect", req.Network, "tako-proxy")
			return &ReconcileProxyResponse{Container: "tako-proxy", Image: req.Image}, nil
		}
	}

	_, _ = runDocker(ctx, "rm", "-f", "tako-proxy")
	if output, err := runDocker(ctx, buildProxyContainerArgs(req)...); err != nil {
		return nil, fmt.Errorf("failed to start tako-proxy: %w, output: %s", err, output)
	}
	return &ReconcileProxyResponse{Container: "tako-proxy", Image: req.Image}, nil
}

func proxyHasPublicRuntimePorts(ctx context.Context) bool {
	output, err := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{json .NetworkSettings.Ports}}")
	if err != nil {
		return false
	}
	ports := parseDockerHostPorts(output)
	return ports[80] && ports[443]
}

func DisableProxy(ctx context.Context) (*DisableProxyResponse, error) {
	routeFiles, err := listProxyRouteFiles()
	if err != nil {
		return nil, err
	}
	if len(routeFiles) > 0 {
		return nil, fmt.Errorf("cannot disable tako-proxy while proxy route files exist: %s", strings.Join(routeFiles, ", "))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	running, _ := runDocker(ctx, "ps", "-a", "--filter", "name=^tako-proxy$", "--format", "{{.Names}}")
	removed := strings.TrimSpace(running) == "tako-proxy"
	if removed {
		if output, err := runDocker(ctx, "rm", "-f", "tako-proxy"); err != nil {
			return nil, fmt.Errorf("failed to remove tako-proxy: %w, output: %s", err, output)
		}
	}
	return &DisableProxyResponse{
		Container:  "tako-proxy",
		Removed:    removed,
		RouteFiles: routeFiles,
	}, nil
}

func listProxyRouteFiles() ([]string, error) {
	entries, err := os.ReadDir(proxyDynamicDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list proxy route files: %w", err)
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") {
			files = append(files, name)
		}
	}
	sort.Strings(files)
	return files, nil
}

func normalizeReconcileProxyRequest(req *ReconcileProxyRequest) {
	if req.Email == "" {
		req.Email = defaultProxyEmail
	}
	if req.Image == "" {
		req.Image = defaultProxyImage
	}
}

func validateReconcileProxyRequest(req ReconcileProxyRequest) error {
	if strings.TrimSpace(req.Network) == "" {
		return fmt.Errorf("network is required")
	}
	if !isSafeRuntimeName(req.Network) {
		return fmt.Errorf("invalid network name")
	}
	if err := validateImageName(req.Image); err != nil {
		return err
	}
	if !isSafeProxyEmail(req.Email) {
		return fmt.Errorf("invalid proxy email")
	}
	return nil
}

func isSafeProxyEmail(value string) bool {
	if len(value) == 0 || len(value) > 254 || strings.TrimSpace(value) != value {
		return false
	}
	if strings.Count(value, "@") != 1 {
		return false
	}
	return !hasControlChars(value) && !strings.ContainsAny(value, " \t")
}

func ensureProxyDirectories() error {
	for _, dir := range []string{
		proxyAcmeDir,
		proxyDynamicDir,
		proxyLogDir,
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
	}
	acmePath := proxyAcmeDir + "/acme.json"
	file, err := os.OpenFile(acmePath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", acmePath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", acmePath, err)
	}
	if err := os.Chmod(acmePath, 0600); err != nil {
		return fmt.Errorf("failed to chmod %s: %w", acmePath, err)
	}
	return nil
}

func buildProxyContainerArgs(req ReconcileProxyRequest) []string {
	return []string{
		"run", "-d",
		"--name", "tako-proxy",
		"--restart", "unless-stopped",
		"--network", req.Network,
		"--publish", "80:80",
		"--publish", "443:443",
		"--volume", proxyAcmeDir + ":/acme",
		"--volume", proxyDynamicDir + ":/etc/traefik/dynamic:ro",
		"--volume", proxyLogDir + ":/var/log/traefik",
		"--label", "tako.runtime=takod",
		"--label", "tako.component=proxy",
		req.Image,
		"--api.dashboard=false",
		"--providers.file.directory=/etc/traefik/dynamic",
		"--providers.file.watch=true",
		"--entryPoints.web.address=:80",
		"--entryPoints.websecure.address=:443",
		"--certificatesResolvers.letsencrypt.acme.email=" + req.Email,
		"--certificatesResolvers.letsencrypt.acme.storage=/acme/acme.json",
		"--certificatesResolvers.letsencrypt.acme.httpChallenge.entryPoint=web",
		"--log.level=INFO",
		"--accessLog.filePath=/var/log/traefik/access.log",
		"--accessLog.format=json",
	}
}
