package takod

import (
	"context"
	"encoding/json"
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
		ports, _ := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{json .NetworkSettings.Ports}}")
		image, _ := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{.Config.Image}}")
		mounts, _ := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{json .Mounts}}")
		if proxyContainerIsCurrent(req, args, ports, image, mounts) {
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
		"--publish", "443:443/udp",
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
		"--entryPoints.websecure.http3=true",
		"--entryPoints.websecure.http3.advertisedPort=443",
		"--certificatesResolvers.letsencrypt.acme.email=" + req.Email,
		"--certificatesResolvers.letsencrypt.acme.storage=/acme/acme.json",
		"--certificatesResolvers.letsencrypt.acme.httpChallenge.entryPoint=web",
		"--log.level=INFO",
		"--accessLog.filePath=/var/log/traefik/access.log",
		"--accessLog.format=json",
	}
}

func proxyContainerIsCurrent(req ReconcileProxyRequest, argsJSON string, portsJSON string, image string, mountsJSON string) bool {
	if strings.TrimSpace(image) != req.Image {
		return false
	}

	requiredArgs := []string{
		"--providers.file.directory=/etc/traefik/dynamic",
		"--providers.file.watch=true",
		"--entryPoints.web.address=:80",
		"--entryPoints.websecure.address=:443",
		"--entryPoints.websecure.http3=true",
		"--entryPoints.websecure.http3.advertisedPort=443",
		"--certificatesResolvers.letsencrypt.acme.email=" + req.Email,
		"--certificatesResolvers.letsencrypt.acme.storage=/acme/acme.json",
		"--certificatesResolvers.letsencrypt.acme.httpChallenge.entryPoint=web",
		"--accessLog.filePath=/var/log/traefik/access.log",
		"--accessLog.format=json",
	}
	args, err := parseProxyArgs(argsJSON)
	if err != nil {
		return false
	}
	for _, required := range requiredArgs {
		if !proxyArgExists(args, required) {
			return false
		}
	}
	if !proxyPortsAreCurrent(portsJSON) {
		return false
	}
	return proxyMountsAreCurrent(mountsJSON)
}

func parseProxyArgs(raw string) ([]string, error) {
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, err
	}
	return args, nil
}

func proxyArgExists(args []string, expected string) bool {
	for _, arg := range args {
		if arg == expected {
			return true
		}
	}
	return false
}

func proxyPortsAreCurrent(raw string) bool {
	var ports map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &ports); err != nil {
		return false
	}
	for _, port := range []string{"80/tcp", "443/tcp", "443/udp"} {
		if _, ok := ports[port]; !ok {
			return false
		}
	}
	return true
}

func proxyMountsAreCurrent(raw string) bool {
	var mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	}
	if err := json.Unmarshal([]byte(raw), &mounts); err != nil {
		return false
	}
	requiredMounts := map[string]string{
		"/acme":                "/etc/tako/proxy/acme",
		"/etc/traefik/dynamic": "/etc/tako/proxy/dynamic",
		"/var/log/traefik":     "/var/log/tako/proxy",
	}
	for destination, source := range requiredMounts {
		if !proxyMountExists(mounts, source, destination) {
			return false
		}
	}
	return true
}

func proxyMountExists(mounts []struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}, source string, destination string) bool {
	for _, mount := range mounts {
		if mount.Source == source && mount.Destination == destination {
			return true
		}
	}
	return false
}
