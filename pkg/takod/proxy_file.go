package takod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var proxyDynamicDir = "/etc/tako/proxy/dynamic"
var proxyRoutesDir = "/etc/tako/proxy/routes"

type ProxyFileRequest struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type ProxyFileResponse struct {
	Path string `json:"path"`
}

func WriteProxyFile(ctx context.Context, req ProxyFileRequest) (*ProxyFileResponse, error) {
	name, err := validateProxyFileName(req.Name)
	if err != nil {
		return nil, err
	}
	if _, err := ParseProxyRouteManifest(req.Content); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureProxyDirectories(); err != nil {
		return nil, err
	}
	path := filepath.Join(proxyRoutesDir, name)
	previous, hadPrevious, err := readFileIfExists(path)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, []byte(req.Content), 0644); err != nil {
		return nil, fmt.Errorf("failed to publish proxy route manifest: %w", err)
	}
	if err := renderAndWriteCaddyfile(ctx); err != nil {
		_ = restoreProxyRouteManifest(path, previous, hadPrevious)
		_ = renderAndWriteCaddyfile(ctx)
		return nil, err
	}
	return &ProxyFileResponse{Path: path}, nil
}

func RemoveProxyFile(ctx context.Context, name string) (*ProxyFileResponse, error) {
	name, err := validateProxyFileName(name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(proxyRoutesDir, name)
	previous, hadPrevious, err := readFileIfExists(path)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove proxy route manifest: %w", err)
	}
	if err := renderAndWriteCaddyfile(ctx); err != nil {
		_ = restoreProxyRouteManifest(path, previous, hadPrevious)
		_ = renderAndWriteCaddyfile(ctx)
		return nil, err
	}
	return &ProxyFileResponse{Path: path}, nil
}

func readFileIfExists(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return data, true, nil
	}
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("failed to read existing route manifest: %w", err)
}

func restoreProxyRouteManifest(path string, previous []byte, hadPrevious bool) error {
	if !hadPrevious {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeFileAtomic(path, previous, 0644)
}

func validateProxyFileName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("proxy file name must not contain path separators")
	}
	if !strings.HasSuffix(name, ".json") {
		return "", fmt.Errorf("proxy file name must end with .json")
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("proxy file name contains invalid character %q", r)
	}
	return name, nil
}

func renderAndWriteCaddyfile(ctx context.Context) error {
	caddyfile, err := renderCaddyfileFromRouteManifests(proxyRoutesDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(proxyCaddyfilePath), 0755); err != nil {
		return fmt.Errorf("failed to create Caddy config directory: %w", err)
	}
	if strings.TrimSpace(caddyfile) == "" {
		caddyfile = emptyProxyCaddyfile()
	}
	candidatePath := proxyCaddyfilePath + ".next"
	if err := writeFileAtomic(candidatePath, []byte(caddyfile), 0644); err != nil {
		return fmt.Errorf("failed to stage Caddyfile: %w", err)
	}
	defer func() { _ = os.Remove(candidatePath) }()

	if err := validateStagedCaddyfile(ctx, candidatePath); err != nil {
		return err
	}
	if err := os.Rename(candidatePath, proxyCaddyfilePath); err != nil {
		return fmt.Errorf("failed to publish Caddyfile: %w", err)
	}
	return nil
}

func validateStagedCaddyfile(ctx context.Context, candidatePath string) error {
	if !currentProxyCanValidateCaddyfile(ctx) {
		return nil
	}
	containerPath := "/etc/caddy/" + filepath.Base(candidatePath)
	output, err := runDocker(ctx, "exec", "tako-proxy", "caddy", "adapt", "--adapter", "caddyfile", "--config", containerPath)
	if err != nil {
		return fmt.Errorf("generated Caddyfile failed validation: %w, output: %s", err, output)
	}
	return nil
}

func currentProxyCanValidateCaddyfile(ctx context.Context) bool {
	running, _ := runDocker(ctx, "ps", "--filter", "name=^tako-proxy$", "--format", "{{.Names}}")
	if strings.TrimSpace(running) != "tako-proxy" {
		return false
	}
	argsJSON, err := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{json .Args}}")
	if err != nil {
		return false
	}
	args, err := parseProxyArgs(argsJSON)
	if err != nil {
		return false
	}
	return proxyArgExists(args, "/etc/caddy/Caddyfile") && proxyArgExists(args, "caddyfile")
}
