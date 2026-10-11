package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployplan"
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

// servicesBuildImages reports whether any selected service builds an image
// (directly or through a shared build) rather than using a prebuilt image.
func servicesBuildImages(services map[string]config.ServiceConfig) bool {
	for _, service := range services {
		if service.Build != "" || service.SharedBuildHash != "" {
			return true
		}
	}
	return false
}

// buildStrategyHeaderLine returns the deploy header's build strategy line, or
// an empty string when no selected service builds anything.
func buildStrategyHeaderLine(cfg *config.Config, services map[string]config.ServiceConfig) string {
	if !servicesBuildImages(services) {
		return ""
	}
	return fmt.Sprintf("Build strategy: %s\n", cfg.GetBuildStrategy())
}

// DeployLeaseTargets includes every schedulable environment worker plus any
// builder candidate that an auto/remote build may mutate. Builder candidates
// come from config.EnvironmentBuilderCandidates, the same helper the deployer
// uses to pick build and image-export sources, so a build can never reach an
// unleased node. Without enrolled servers the candidates are environment
// members only.
func DeployLeaseTargets(cfg *config.Config, environment string, environmentServers []string, services map[string]config.ServiceConfig) []string {
	seen := make(map[string]struct{}, len(environmentServers))
	for _, name := range environmentServers {
		seen[name] = struct{}{}
	}
	needsRemoteBuilder := cfg.GetBuildStrategy() != config.BuildStrategyLocal && servicesBuildImages(services)
	if needsRemoteBuilder {
		// A resolution error leaves no builder to lease; the deployer reports
		// the same error before any build runs.
		builders, _ := config.EnvironmentBuilderCandidates(cfg, environment)
		for _, name := range builders {
			seen[name] = struct{}{}
		}
	}
	return sortedServerSet(seen)
}

// DeployMutationTargets returns only nodes the resolved deploy can mutate:
// assigned workers, the environment's edge nodes, a required builder, and the
// authoritative state/controller node. An unrelated unavailable worker, or a
// node of another environment, therefore cannot block an otherwise local
// operation.
func DeployMutationTargets(cfg *config.Config, environment string, services map[string]config.ServiceConfig, assignments map[string][]scheduler.Assignment) []string {
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
		edges, err := config.EnvironmentEdgeCandidates(cfg, environment)
		if err != nil {
			// Proxy targets are always a subset of the environment; fall back
			// to every schedulable member so an unresolvable placement still
			// fences every node the proxy reconcile could touch.
			edges = environmentSchedulableMembers(cfg, environment)
		}
		for _, name := range edges {
			seen[name] = struct{}{}
		}
	}
	for _, name := range DeployLeaseTargets(cfg, environment, nil, services) {
		seen[name] = struct{}{}
	}
	if controller, enrolled, err := controllerAuthorityServer(cfg, nil); err == nil && enrolled {
		seen[controller] = struct{}{}
	}
	return sortedServerSet(seen)
}

func environmentSchedulableMembers(cfg *config.Config, environment string) []string {
	servers, err := cfg.GetEnvironmentServers(environment)
	if err != nil {
		return nil
	}
	members, err := config.ResolveSchedulableEnvironmentTargets(cfg.Servers, servers, environment)
	if err != nil {
		return nil
	}
	return members
}

func sortedServerSet(seen map[string]struct{}) []string {
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
//
// A prior owner that environment scoping removed from the loaded config
// because it references unset variables fails closed with an invalid-request
// error (exit 2) naming the server and the variables; skipping it would leave
// its containers both unleased and uncleaned. A prior owner that is absent
// from servers entirely keeps the historical behavior: it is skipped, and the
// deploy warns about retired nodes from local state.
func AddPriorDesiredMutationTargets(cfg *config.Config, environment string, current []string, prior *takodstate.DesiredRevision) ([]string, error) {
	seen := make(map[string]struct{}, len(current))
	for _, name := range current {
		seen[name] = struct{}{}
	}
	if prior != nil {
		scopedOut := cfg.ScopedOutServers()
		var pruned []string
		for _, service := range prior.Services {
			for _, assignment := range service.Assignments {
				if _, configured := cfg.Servers[assignment.Node]; configured {
					seen[assignment.Node] = struct{}{}
				} else if _, wasPruned := scopedOut[assignment.Node]; wasPruned && !containsString(pruned, assignment.Node) {
					pruned = append(pruned, assignment.Node)
				}
			}
		}
		if len(pruned) > 0 {
			sort.Strings(pruned)
			messages := make([]string, 0, len(pruned))
			for _, name := range pruned {
				vars := scopedOut[name]
				refs := make([]string, 0, len(vars))
				for _, variable := range vars {
					refs = append(refs, "${"+variable+"}")
				}
				verb := "is"
				if len(refs) > 1 {
					verb = "are"
				}
				messages = append(messages, fmt.Sprintf("server %s holds prior desired assignments for %s but was not loaded because %s %s unset", name, environment, strings.Join(refs, ", "), verb))
			}
			return nil, invalidRequestf("%s; export the variable(s) and retry", strings.Join(messages, "; "))
		}
	}
	return sortedServerSet(seen), nil
}

// PriorOwnerScopeReasons explains why nodes outside the environment are in a
// lease set: they still hold prior desired assignments for the environment.
// The reasons are attached to lease and connectivity errors so an operator
// can see that the node must be reachable (or cleaned with
// 'tako remove --server') before the environment can be deployed.
func PriorOwnerScopeReasons(environment string, environmentServers []string, prior *takodstate.DesiredRevision) map[string]string {
	if prior == nil {
		return nil
	}
	members := make(map[string]struct{}, len(environmentServers))
	for _, name := range environmentServers {
		members[name] = struct{}{}
	}
	reasons := map[string]string{}
	for _, service := range prior.Services {
		for _, assignment := range service.Assignments {
			if _, member := members[assignment.Node]; member || assignment.Node == "" {
				continue
			}
			reasons[assignment.Node] = fmt.Sprintf("node %s holds prior desired assignments for environment %s", assignment.Node, environment)
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	return reasons
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
	PreflightGracefulStopCapabilities() error
	PreflightServiceMutationCapabilities(map[string]config.ServiceConfig) error
	SetupTakodRuntime() error
}

// preflightAndSetupTakodRuntime preserves the no-mutation boundary for
// unsupported proxy topology. Setup applies mesh and node runtime state, so it
// must never run before route capability admission succeeds.
func preflightAndSetupTakodRuntime(runtime takodRuntimeSetup, routeServices, mutationServices map[string]config.ServiceConfig) error {
	if err := runtime.PreflightTakodProxyCapabilities(routeServices); err != nil {
		return err
	}
	if err := runtime.PreflightGracefulStopCapabilities(); err != nil {
		return err
	}
	if err := runtime.PreflightServiceMutationCapabilities(mutationServices); err != nil {
		return err
	}
	if err := runtime.SetupTakodRuntime(); err != nil {
		return fmt.Errorf("failed to setup takod runtime: %w", err)
	}
	return nil
}

type takodRuntimeVerifier interface {
	PreflightTakodProxyCapabilities(map[string]config.ServiceConfig) error
	PreflightGracefulStopCapabilities() error
	PreflightServiceMutationCapabilities(map[string]config.ServiceConfig) error
	VerifyTakodRuntime() error
}

// preflightAndVerifyTakodRuntime is the narrow-operation variant used by
// `tako run`: it runs the same capability admission but only verifies the
// target agent. It never writes node metadata or mesh config, because a run
// contacts too few nodes to build a correct environment-wide peer list.
func preflightAndVerifyTakodRuntime(runtime takodRuntimeVerifier, routeServices, mutationServices map[string]config.ServiceConfig) error {
	if err := runtime.PreflightTakodProxyCapabilities(routeServices); err != nil {
		return err
	}
	if err := runtime.PreflightGracefulStopCapabilities(); err != nil {
		return err
	}
	if err := runtime.PreflightServiceMutationCapabilities(mutationServices); err != nil {
		return err
	}
	if err := runtime.VerifyTakodRuntime(); err != nil {
		return fmt.Errorf("failed to verify takod runtime: %w", err)
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
	timeout := takodclient.JSONRequestTimeout
	if request.RemoveContainers {
		// Scheduled work can consume its 24-hour maximum before service
		// containers begin their own maximum 24-hour graceful drain.
		timeout = 48*time.Hour + 10*time.Minute
	} else if request.RemoveTakodState {
		timeout = 24*time.Hour + 5*time.Minute
	}
	output, err := takodclient.RequestJSONWithTimeoutContext(ctx, client, TakodSocketFromConfig(cfg), "POST", "/v1/cleanup", request, timeout)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse takod response: %w", err)
	}
	return &response, nil
}

func RequireTakodCapabilityContext(ctx context.Context, client any, cfg *config.Config, capability string, feature string) error {
	output, err := takodclient.RequestJSONWithContext(ctx, client, TakodSocketFromConfig(cfg), "GET", "/v1/status", nil)
	if err != nil {
		return fmt.Errorf("read takod capabilities: %w", err)
	}
	var status takod.Status
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return fmt.Errorf("parse takod capabilities: %w", err)
	}
	for _, advertised := range status.Capabilities {
		if advertised == capability {
			return nil
		}
	}
	return fmt.Errorf("takod does not support %s (missing capability %s); upgrade the node agent", feature, capability)
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

// ExternalVolumeNamesForEnvironment lists volume names cleanup must never
// remove automatically. Besides external volumes, this includes every volume
// whose creation semantics cannot be proven by takod.
func ExternalVolumeNamesForEnvironment(cfg *config.Config, environment string) []string {
	if cfg == nil || cfg.Volumes == nil {
		return nil
	}
	seen := make(map[string]bool)
	names := make([]string, 0)
	for key, volume := range cfg.Volumes {
		if !volume.External && volume.Owner == "" && (volume.Driver == "" || volume.Driver == "local") && len(volume.DriverOpts) == 0 {
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
