package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/scheduler"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

// TakodSocketFromConfig resolves the takod unix socket path for a config.
func TakodSocketFromConfig(cfg *config.Config) string {
	if cfg.Runtime != nil && cfg.Runtime.Agent != nil && cfg.Runtime.Agent.Socket != "" {
		return cfg.Runtime.Agent.Socket
	}
	return takodclient.DefaultSocket
}

// PreferredRuntimeServer keeps controller-side operations on the protected
// local ingress whenever the local enrolled node is mutation-eligible.
func PreferredRuntimeServer(cfg *config.Config, serverNames []string) (string, error) {
	for _, name := range serverNames {
		if server, ok := cfg.Servers[name]; ok && server.Transport == "local" {
			return name, nil
		}
	}
	if len(serverNames) == 0 {
		return "", fmt.Errorf("no mutation-eligible runtime server")
	}
	return serverNames[0], nil
}

// AuthoritativeStateServer resolves controller-only state in enrolled mode and
// retains the legacy local/first preference for unenrolled configurations.
func AuthoritativeStateServer(cfg *config.Config, fallback []string) (string, error) {
	controller, enrolled, err := controllerAuthorityServer(cfg, fallback)
	if err != nil {
		return "", err
	}
	if enrolled {
		return controller, nil
	}
	return PreferredRuntimeServer(cfg, fallback)
}

// DeployLeaseTargets includes every schedulable environment worker plus any
// schedulable builder that an auto/remote build may mutate. Setup remains
// scoped to environment workers; the builder lease protects the later exact
// image build/transfer operation.
func DeployLeaseTargets(cfg *config.Config, environmentServers []string, services map[string]config.ServiceConfig) []string {
	seen := make(map[string]struct{}, len(environmentServers))
	for _, name := range environmentServers {
		seen[name] = struct{}{}
	}
	needsRemoteBuilder := cfg.GetBuildStrategy() != config.BuildStrategyLocal
	if needsRemoteBuilder {
		needsRemoteBuilder = false
		for _, service := range services {
			if service.Build != "" || service.SharedBuildHash != "" {
				needsRemoteBuilder = true
				break
			}
		}
	}
	if needsRemoteBuilder {
		for name, server := range cfg.Servers {
			if server.Schedulable() && server.HasPlatformRole(nodeidentity.RoleBuilder) {
				seen[name] = struct{}{}
			}
		}
	}
	targets := make([]string, 0, len(seen))
	for name := range seen {
		targets = append(targets, name)
	}
	sort.Strings(targets)
	return targets
}

// DeployMutationTargets returns only nodes the resolved deploy can mutate:
// assigned workers, edge nodes, a required builder, and the authoritative
// state/controller node. An unrelated unavailable worker therefore cannot
// block an otherwise local operation.
func DeployMutationTargets(cfg *config.Config, services map[string]config.ServiceConfig, assignments map[string][]scheduler.Assignment) []string {
	seen := make(map[string]struct{})
	for serviceName := range services {
		for _, assignment := range assignments[serviceName] {
			if _, ok := cfg.Servers[assignment.Node]; ok {
				seen[assignment.Node] = struct{}{}
			}
		}
	}
	hasProxy := false
	for _, service := range services {
		hasProxy = hasProxy || service.IsProxied()
	}
	if hasProxy {
		for name, server := range cfg.Servers {
			if server.Schedulable() && server.HasPlatformRole(nodeidentity.RoleEdge) {
				seen[name] = struct{}{}
			}
		}
	}
	for _, name := range DeployLeaseTargets(cfg, nil, services) {
		seen[name] = struct{}{}
	}
	if controller, enrolled, err := controllerAuthorityServer(cfg, nil); err == nil && enrolled {
		seen[controller] = struct{}{}
	}
	targets := make([]string, 0, len(seen))
	for name := range seen {
		targets = append(targets, name)
	}
	sort.Strings(targets)
	return targets
}

// AddPriorDesiredMutationTargets keeps workers that still own authoritative
// desired assignments in the fenced observation/cleanup set. This is what
// lets a full deploy remove the last service from a worker instead of
// forgetting the worker before its old containers are observed and stopped.
func AddPriorDesiredMutationTargets(cfg *config.Config, current []string, prior *takodstate.DesiredRevision) []string {
	seen := make(map[string]struct{}, len(current))
	for _, name := range current {
		seen[name] = struct{}{}
	}
	if prior != nil {
		for _, service := range prior.Services {
			for _, assignment := range service.Assignments {
				if _, configured := cfg.Servers[assignment.Node]; configured {
					seen[assignment.Node] = struct{}{}
				}
			}
		}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// ShouldReplicateDeploymentHistory preserves legacy mesh replication while
// keeping enrolled platform state single-writer on the controller.
func ShouldReplicateDeploymentHistory(cfg *config.Config) bool {
	_, enrolled, err := controllerAuthorityServer(cfg, nil)
	return err != nil || !enrolled
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

type takodRuntimeSetup interface {
	PreflightTakodProxyCapabilities(map[string]config.ServiceConfig) error
	SetupTakodRuntime() error
}

// preflightAndSetupTakodRuntime preserves the no-mutation boundary for
// unsupported proxy topology. Setup applies mesh and node runtime state, so it
// must never run before route capability admission succeeds.
func preflightAndSetupTakodRuntime(runtime takodRuntimeSetup, services map[string]config.ServiceConfig) error {
	if err := runtime.PreflightTakodProxyCapabilities(services); err != nil {
		return err
	}
	if err := runtime.SetupTakodRuntime(); err != nil {
		return fmt.Errorf("failed to setup takod runtime: %w", err)
	}
	return nil
}

// CleanupViaTakod runs post-deploy cleanup on one node through takod.
func CleanupViaTakod(client any, cfg *config.Config, request takod.CleanupRequest) (*takod.CleanupResponse, error) {
	return CleanupViaTakodContext(context.Background(), client, cfg, request)
}

// CleanupViaTakodContext runs cleanup through takod bounded by ctx.
func CleanupViaTakodContext(ctx context.Context, client any, cfg *config.Config, request takod.CleanupRequest) (*takod.CleanupResponse, error) {
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
		if service.SharedBuildHash != "" {
			repository := ImageRepositoryFromRef(deployplan.SharedBuildImageRef(cfg, environment, service.ImageFrom, ""))
			if repository != "" {
				seen[repository] = true
			}
			continue
		}
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
