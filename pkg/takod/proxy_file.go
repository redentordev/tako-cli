package takod

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var proxyDynamicDir = "/etc/tako/proxy/dynamic"
var proxyRoutesDir = "/etc/tako/proxy/routes"

var proxyRenderMu sync.Mutex

type ProxyFileRequest struct {
	Name              string `json:"name"`
	Content           string `json:"content"`
	RequireActivation bool   `json:"requireActivation,omitempty"`
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
	proxyRenderMu.Lock()
	defer proxyRenderMu.Unlock()
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
	if err := renderAndWriteCaddyfileLocked(ctx, ""); err != nil {
		return nil, rollbackProxyRouteMutation(ctx, path, previous, hadPrevious, err)
	}
	if req.RequireActivation {
		running, err := currentProxyCanValidateCaddyfile(ctx)
		if err != nil {
			return nil, rollbackProxyRouteMutation(ctx, path, previous, hadPrevious, err)
		}
		if !running {
			return nil, rollbackProxyRouteMutation(ctx, path, previous, hadPrevious, fmt.Errorf("managed proxy is not running after route publication"))
		}
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
	if err := RemoveProxyFiles(ctx, []string{name}); err != nil {
		return nil, err
	}
	return &ProxyFileResponse{Path: filepath.Join(proxyRoutesDir, name)}, nil
}

type proxyRouteManifestSnapshot struct {
	path        string
	content     []byte
	hadPrevious bool
}

// RemoveProxyFiles withdraws a complete route set under one render/reload
// transaction. Any failure restores every manifest before returning.
func RemoveProxyFiles(ctx context.Context, names []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	validated := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		name, err := validateProxyFileName(name)
		if err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("duplicate proxy file %q", name)
		}
		seen[name] = true
		validated = append(validated, name)
	}
	if len(validated) == 0 {
		return nil
	}
	proxyRenderMu.Lock()
	defer proxyRenderMu.Unlock()
	snapshots := make([]proxyRouteManifestSnapshot, 0, len(validated))
	for _, name := range validated {
		path := filepath.Join(proxyRoutesDir, name)
		previous, hadPrevious, err := readFileIfExists(path)
		if err != nil {
			return err
		}
		if fence, fenced := operationFenceFromContext(ctx); fenced && hadPrevious {
			manifest, parseErr := ParseProxyRouteManifest(string(previous))
			if parseErr != nil || manifest.Project != fence.Project || manifest.Environment != fence.Environment {
				return fmt.Errorf("cleanup proxy manifest is outside the controller operation fence")
			}
		}
		snapshots = append(snapshots, proxyRouteManifestSnapshot{path: path, content: previous, hadPrevious: hadPrevious})
	}
	for _, snapshot := range snapshots {
		if err := os.Remove(snapshot.path); err != nil && !os.IsNotExist(err) {
			return rollbackProxyRouteSet(ctx, snapshots, fmt.Errorf("failed to remove proxy route manifest: %w", err))
		}
	}
	if err := renderAndWriteCaddyfileLocked(ctx, ""); err != nil {
		return rollbackProxyRouteSet(ctx, snapshots, err)
	}
	return nil
}

func rollbackProxyRouteSet(ctx context.Context, snapshots []proxyRouteManifestSnapshot, activationErr error) error {
	for _, snapshot := range snapshots {
		if err := restoreProxyRouteManifest(snapshot.path, snapshot.content, snapshot.hadPrevious); err != nil {
			return errors.Join(activationErr, fmt.Errorf("failed to restore proxy route set: %w", err))
		}
	}
	if err := renderAndWriteCaddyfileLocked(ctx, ""); err != nil {
		return errors.Join(activationErr, fmt.Errorf("failed to reactivate previous proxy route set: %w", err))
	}
	return activationErr
}

func rollbackProxyRouteMutation(ctx context.Context, path string, previous []byte, hadPrevious bool, activationErr error) error {
	if err := restoreProxyRouteManifest(path, previous, hadPrevious); err != nil {
		return errors.Join(activationErr, fmt.Errorf("failed to restore proxy route manifest: %w", err))
	}
	if err := renderAndWriteCaddyfileLocked(ctx, ""); err != nil {
		return errors.Join(activationErr, fmt.Errorf("failed to reactivate previous proxy routes: %w", err))
	}
	return activationErr
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
	proxyRenderMu.Lock()
	defer proxyRenderMu.Unlock()
	return renderAndWriteCaddyfileLocked(ctx, "")
}

func renderAndWriteCaddyfileThen(ctx context.Context, excludedCertificateDomain string, afterPublish func() error) error {
	proxyRenderMu.Lock()
	defer proxyRenderMu.Unlock()
	if err := renderAndWriteCaddyfileLocked(ctx, excludedCertificateDomain); err != nil {
		return err
	}
	if err := afterPublish(); err != nil {
		if rollbackErr := renderAndWriteCaddyfileLocked(ctx, ""); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("failed to reactivate proxy configuration: %w", rollbackErr))
		}
		return err
	}
	return nil
}

func renderAndWriteCaddyfileLocked(ctx context.Context, excludedCertificateDomain string) error {
	caddyfile, err := renderCaddyfileFromRouteManifestsExcluding(proxyRoutesDir, excludedCertificateDomain)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(proxyCaddyfilePath), 0755); err != nil {
		return fmt.Errorf("failed to create Caddy config directory: %w", err)
	}
	if strings.TrimSpace(caddyfile) == "" {
		caddyfile = emptyProxyCaddyfile()
	}
	previous, hadPrevious, err := readFileIfExists(proxyCaddyfilePath)
	if err != nil {
		return err
	}
	proxyRunning, err := currentProxyCanValidateCaddyfile(ctx)
	if err != nil {
		return err
	}
	candidatePath := proxyCaddyfilePath + ".next"
	if err := writeFileAtomic(candidatePath, []byte(caddyfile), 0644); err != nil {
		return fmt.Errorf("failed to stage Caddyfile: %w", err)
	}
	defer func() { _ = os.Remove(candidatePath) }()

	if err := validateStagedCaddyfile(ctx, candidatePath, proxyRunning); err != nil {
		return err
	}
	if err := os.Rename(candidatePath, proxyCaddyfilePath); err != nil {
		return fmt.Errorf("failed to publish Caddyfile: %w", err)
	}
	if proxyRunning {
		if err := reloadProxyCaddyfile(ctx); err != nil {
			rollbackErr := restorePublishedCaddyfile(ctx, previous, hadPrevious)
			if rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			return err
		}
	}
	return nil
}

func reloadProxyCaddyfile(ctx context.Context) error {
	output, err := runDocker(ctx, "exec", "tako-proxy", "caddy", "reload", "--adapter", "caddyfile", "--config", "/etc/caddy/Caddyfile")
	if err != nil {
		return fmt.Errorf("failed to activate generated Caddyfile: %w, output: %s", err, output)
	}
	return nil
}

func restorePublishedCaddyfile(ctx context.Context, previous []byte, hadPrevious bool) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if !hadPrevious {
		if err := os.Remove(proxyCaddyfilePath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove unactivated Caddyfile: %w", err)
		}
		if err := stopProxyAndVerifyAbsent(rollbackCtx); err != nil {
			return fmt.Errorf("failed to stop proxy after Caddyfile activation failure: %w", err)
		}
		return nil
	}
	if err := writeFileAtomic(proxyCaddyfilePath, previous, 0644); err != nil {
		if stopErr := stopProxyAndVerifyAbsent(rollbackCtx); stopErr != nil {
			return errors.Join(fmt.Errorf("failed to restore previous Caddyfile: %w", err), fmt.Errorf("failed to stop proxy: %w", stopErr))
		}
		return fmt.Errorf("failed to restore previous Caddyfile; proxy stopped fail-closed: %w", err)
	}
	if err := reloadProxyCaddyfile(rollbackCtx); err != nil {
		if stopErr := stopProxyAndVerifyAbsent(rollbackCtx); stopErr != nil {
			return errors.Join(fmt.Errorf("failed to reactivate previous Caddyfile: %w", err), fmt.Errorf("failed to stop proxy: %w", stopErr))
		}
		return fmt.Errorf("failed to reactivate previous Caddyfile; proxy stopped fail-closed: %w", err)
	}
	return nil
}

func validateStagedCaddyfile(ctx context.Context, candidatePath string, proxyRunning bool) error {
	if !proxyRunning {
		return nil
	}
	containerPath := "/etc/caddy/" + filepath.Base(candidatePath)
	output, err := runDocker(ctx, "exec", "tako-proxy", "caddy", "adapt", "--adapter", "caddyfile", "--config", containerPath)
	if err != nil {
		return fmt.Errorf("generated Caddyfile failed validation: %w, output: %s", err, output)
	}
	return nil
}

func currentProxyCanValidateCaddyfile(ctx context.Context) (bool, error) {
	running, err := runDocker(ctx, "ps", "--filter", "name=^tako-proxy$", "--format", "{{.Names}}")
	if err != nil {
		return false, fmt.Errorf("failed to inspect proxy before Caddyfile publication: %w", err)
	}
	if strings.TrimSpace(running) == "" {
		return false, nil
	}
	if strings.TrimSpace(running) != "tako-proxy" {
		return false, fmt.Errorf("unexpected proxy container selection %q", strings.TrimSpace(running))
	}
	argsJSON, err := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{json .Args}}")
	if err != nil {
		return false, fmt.Errorf("failed to inspect running proxy arguments: %w", err)
	}
	args, err := parseProxyArgs(argsJSON)
	if err != nil {
		return false, fmt.Errorf("failed to parse running proxy arguments: %w", err)
	}
	if !proxyArgExists(args, "/etc/caddy/Caddyfile") || !proxyArgExists(args, "caddyfile") {
		return false, fmt.Errorf("running tako-proxy cannot validate and reload the managed Caddyfile")
	}
	return true, nil
}
