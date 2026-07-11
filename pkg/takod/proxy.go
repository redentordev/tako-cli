package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	defaultProxyImage = "caddy:2.9-alpine"
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
	networks, err := proxyNetworksForCurrentRoutes(req.Network)
	if err != nil {
		return nil, err
	}

	running, _ := runDocker(ctx, "ps", "--filter", "name=^tako-proxy$", "--format", "{{.Names}}")
	if strings.TrimSpace(running) == "tako-proxy" {
		args, _ := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{json .Args}}")
		ports, _ := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{json .NetworkSettings.Ports}}")
		hostPorts, _ := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{json .HostConfig.PortBindings}}")
		image, _ := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{.Config.Image}}")
		mounts, _ := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{json .Mounts}}")
		env, _ := runDocker(ctx, "inspect", "tako-proxy", "--format", "{{json .Config.Env}}")
		if proxyContainerIsCurrent(req, args, ports, hostPorts, image, mounts, env) {
			if err := ensureProxyNetworkAttachments(ctx, networks); err == nil {
				return &ReconcileProxyResponse{Container: "tako-proxy", Image: req.Image}, nil
			}
		}
	}

	if err := renderAndWriteCaddyfile(ctx); err != nil {
		return nil, err
	}
	_, _ = runDocker(ctx, "rm", "-f", "tako-proxy")
	if output, err := runDocker(ctx, buildProxyContainerArgs(req)...); err != nil {
		return nil, fmt.Errorf("failed to start tako-proxy: %w, output: %s", err, output)
	}
	if err := ensureProxyNetworkAttachments(ctx, networks); err != nil {
		return nil, err
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
		proxyRoutesDir,
		filepath.Dir(proxyCaddyfilePath),
		proxyCaddyDataDir,
		proxyCaddyConfigDir,
		proxyLogDir,
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
	}
	if err := ensureSecureProxyCertificateDirectory(proxyCertStoreDir); err != nil {
		return err
	}
	return nil
}

func proxyNetworksForCurrentRoutes(fallback string) ([]string, error) {
	networkSet := make(map[string]bool)
	if fallback != "" {
		networkSet[fallback] = true
	}
	manifests, err := readProxyRouteManifests(proxyRoutesDir)
	if err != nil {
		return nil, err
	}
	for _, manifest := range manifests {
		if manifest.Network != "" {
			networkSet[manifest.Network] = true
		}
	}
	networks := make([]string, 0, len(networkSet))
	for network := range networkSet {
		if !isSafeRuntimeName(network) {
			return nil, fmt.Errorf("invalid network name")
		}
		networks = append(networks, network)
	}
	sort.Strings(networks)
	return networks, nil
}

func ensureProxyNetworkAttachments(ctx context.Context, networks []string) error {
	for _, network := range networks {
		if !dockerNetworkExists(ctx, network) {
			continue
		}
		output, err := runDocker(ctx, "network", "connect", network, "tako-proxy")
		if err != nil && !isAlreadyConnectedNetworkOutput(output) {
			return fmt.Errorf("failed to connect tako-proxy to network %s: %w, output: %s", network, err, output)
		}
	}
	return nil
}

func dockerNetworkExists(ctx context.Context, network string) bool {
	_, err := runDocker(ctx, "network", "inspect", network)
	return err == nil
}

func isAlreadyConnectedNetworkOutput(output string) bool {
	output = strings.ToLower(output)
	return strings.Contains(output, "already exists") || strings.Contains(output, "already connected")
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
		"--env", "TAKO_PROXY_EMAIL=" + req.Email,
		"--volume", filepath.Dir(proxyCaddyfilePath) + ":/etc/caddy:ro",
		"--volume", proxyCaddyDataDir + ":/data",
		"--volume", proxyCaddyConfigDir + ":/config",
		"--volume", proxyLogDir + ":/var/log/caddy",
		"--volume", proxyCertStoreDir + ":" + proxyCertContainerDir + ":ro",
		"--label", "tako.runtime=takod",
		"--label", "tako.component=proxy",
		req.Image,
		"caddy",
		"run",
		"--config", "/etc/caddy/Caddyfile",
		"--adapter", "caddyfile",
		"--watch",
	}
}

func proxyContainerIsCurrent(req ReconcileProxyRequest, argsJSON string, portsJSON string, hostPortsJSON string, image string, mountsJSON string, envJSON string) bool {
	if strings.TrimSpace(image) != req.Image {
		return false
	}

	requiredArgs := []string{
		"run",
		"--config",
		"/etc/caddy/Caddyfile",
		"--adapter",
		"caddyfile",
		"--watch",
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
	if !proxyPortsAreCurrent(portsJSON, hostPortsJSON) {
		return false
	}
	return proxyMountsAreCurrent(mountsJSON) && proxyEnvIsCurrent(envJSON, req.Email)
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

func proxyPortsAreCurrent(networkRaw string, hostRaw string) bool {
	networkPorts, networkOK := parseProxyPortMap(networkRaw)
	hostPorts, hostOK := parseProxyPortMap(hostRaw)
	if !networkOK && !hostOK {
		return false
	}
	for _, port := range []string{"80/tcp", "443/tcp", "443/udp"} {
		if !proxyPortExists(networkPorts, port) && !proxyPortExists(hostPorts, port) {
			return false
		}
	}
	return true
}

func parseProxyPortMap(raw string) (map[string]json.RawMessage, bool) {
	var ports map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &ports); err != nil {
		return nil, false
	}
	return ports, true
}

func proxyPortExists(ports map[string]json.RawMessage, port string) bool {
	if ports == nil {
		return false
	}
	_, ok := ports[port]
	return ok
}

type proxyMount struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

func proxyMountsAreCurrent(raw string) bool {
	var mounts []proxyMount
	if err := json.Unmarshal([]byte(raw), &mounts); err != nil {
		return false
	}
	requiredMounts := map[string]string{
		"/etc/caddy":          filepath.Dir(proxyCaddyfilePath),
		"/data":               proxyCaddyDataDir,
		"/config":             proxyCaddyConfigDir,
		"/var/log/caddy":      proxyLogDir,
		proxyCertContainerDir: proxyCertStoreDir,
	}
	for destination, source := range requiredMounts {
		if !proxyMountExists(mounts, source, destination) {
			return false
		}
	}
	for _, mount := range mounts {
		if mount.Destination == proxyCertContainerDir && mount.RW {
			return false
		}
	}
	return true
}

func proxyEnvIsCurrent(raw string, email string) bool {
	var env []string
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return false
	}
	expected := "TAKO_PROXY_EMAIL=" + email
	for _, value := range env {
		if value == expected {
			return true
		}
	}
	return false
}

func proxyMountExists(mounts []proxyMount, source string, destination string) bool {
	for _, mount := range mounts {
		if mount.Source == source && mount.Destination == destination {
			return true
		}
	}
	return false
}
