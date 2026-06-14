package takod

import (
	"context"
	"fmt"
	"os"
	"strings"
)

const (
	defaultProxyImage = "traefik:v3.6.1"
	defaultProxyEmail = "tako@redentor.dev"
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

func ReconcileProxy(ctx context.Context, req ReconcileProxyRequest) (*ReconcileProxyResponse, error) {
	normalizeReconcileProxyRequest(&req)
	if err := validateReconcileProxyRequest(req); err != nil {
		return nil, err
	}
	if err := ensureDockerNetwork(ctx, req.Network); err != nil {
		return nil, err
	}
	if err := ensureProxyDirectories(); err != nil {
		return nil, err
	}

	running, _ := runDocker(ctx, "ps", "--filter", "name=^tako-proxy$", "--format", "{{.Names}}")
	if strings.TrimSpace(running) == "tako-proxy" {
		args, _ := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{json .Args}}")
		if strings.Contains(args, "--providers.file.directory=/etc/traefik/dynamic") {
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
		"/etc/tako/proxy/acme",
		proxyDynamicDir,
		"/var/log/tako/proxy",
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
	}
	acmePath := "/etc/tako/proxy/acme/acme.json"
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
		"--volume", "/etc/tako/proxy/acme:/acme",
		"--volume", "/etc/tako/proxy/dynamic:/etc/traefik/dynamic:ro",
		"--volume", "/var/log/tako/proxy:/var/log/traefik",
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
