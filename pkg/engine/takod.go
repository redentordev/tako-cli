package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// TakodSocketFromConfig resolves the takod unix socket path for a config.
func TakodSocketFromConfig(cfg *config.Config) string {
	if cfg.Runtime != nil && cfg.Runtime.Agent != nil && cfg.Runtime.Agent.Socket != "" {
		return cfg.Runtime.Agent.Socket
	}
	return takodclient.DefaultSocket
}

// RequireTakodRuntime rejects configs that do not use the takod runtime.
func RequireTakodRuntime(cfg *config.Config) error {
	if !cfg.IsTakodRuntime() {
		return invalidRequestf("runtime.mode=%s is not supported; Tako now uses runtime.mode=takod", cfg.GetRuntimeMode())
	}
	return nil
}

// EnsureDeployRuntimeSupported validates the runtime/state/mesh combination
// deploys require.
func EnsureDeployRuntimeSupported(cfg *config.Config) error {
	if err := RequireTakodRuntime(cfg); err != nil {
		return err
	}
	if !cfg.IsMeshEnabled() {
		return invalidRequestf("mesh.enabled=false is not supported; single-node deploys use a one-node mesh")
	}
	if cfg.GetStateBackend() != config.StateBackendReplicated {
		return invalidRequestf("state.backend=%s is not supported; takod deployments use replicated state", cfg.GetStateBackend())
	}
	if cfg.GetDeployConsistency() != config.StateDeployConsistencyLease {
		return invalidRequestf("state.deployConsistency=%s is not implemented yet; current deploys support lease", cfg.GetDeployConsistency())
	}
	return nil
}

// CleanupViaTakod runs post-deploy cleanup on one node through takod.
func CleanupViaTakod(client *ssh.Client, cfg *config.Config, request takod.CleanupRequest) (*takod.CleanupResponse, error) {
	return CleanupViaTakodContext(context.Background(), client, cfg, request)
}

// CleanupViaTakodContext runs cleanup through takod bounded by ctx.
func CleanupViaTakodContext(ctx context.Context, client *ssh.Client, cfg *config.Config, request takod.CleanupRequest) (*takod.CleanupResponse, error) {
	var response takod.CleanupResponse
	output, err := takodclient.RequestJSONWithContext(ctx, client, TakodSocketFromConfig(cfg), "POST", "/v1/cleanup", request)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse takod response: %w", err)
	}
	return &response, nil
}

// CleanupImageRepositories lists the image repositories automatic cleanup
// should prune for built services.
func CleanupImageRepositories(cfg *config.Config, environment string, services map[string]config.ServiceConfig) []string {
	seen := make(map[string]bool)
	for serviceName, service := range services {
		if service.IsRun() {
			continue
		}
		if service.Build == "" && service.Image != "" {
			continue
		}
		repository := ImageRepositoryFromRef(cfg.GetFullImageName(serviceName, environment))
		if repository != "" {
			seen[repository] = true
		}
	}
	repositories := make([]string, 0, len(seen))
	for repository := range seen {
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	return repositories
}

// ExternalVolumeNamesForEnvironment lists external volume names cleanup must
// never remove.
func ExternalVolumeNamesForEnvironment(cfg *config.Config, environment string) []string {
	if cfg == nil || cfg.Volumes == nil {
		return nil
	}
	seen := make(map[string]bool)
	names := make([]string, 0)
	for key, volume := range cfg.Volumes {
		if !volume.External {
			continue
		}
		name := cfg.GetVolumeName(key, environment)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ImageRepositoryFromRef strips tag and digest from an image reference.
func ImageRepositoryFromRef(ref string) string {
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
