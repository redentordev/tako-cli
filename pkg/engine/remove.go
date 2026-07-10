package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// KindRemoveResult identifies a serialized remove result document.
const KindRemoveResult = "RemoveResult"

// RemoveRequest describes one remove operation: tear down this project's
// services on the environment mesh while preserving server setup. Config
// must be loaded and validated; Environment must be resolved.
type RemoveRequest struct {
	Config      *config.Config `json:"-"`
	Environment string         `json:"environment"`
	// Servers optionally scopes removal to the named environment servers
	// (the --server flag). Empty targets every environment server.
	Servers []string `json:"servers,omitempty"`
	Verbose bool     `json:"verbose,omitempty"`
}

// RemoveServerOutcome reports what happened on one node during remove.
type RemoveServerOutcome struct {
	Name    string `json:"name"`
	Host    string `json:"host,omitempty"`
	Removed bool   `json:"removed"`
	Error   string `json:"error,omitempty"`
}

// RemoveResult is the serializable outcome of RemoveSession.Apply.
type RemoveResult struct {
	APIVersion  string                `json:"apiVersion"`
	Kind        string                `json:"kind"`
	Project     string                `json:"project"`
	Environment string                `json:"environment"`
	Scoped      bool                  `json:"scoped,omitempty"`
	Servers     []RemoveServerOutcome `json:"servers"`
	StartedAt   time.Time             `json:"startedAt"`
	Duration    float64               `json:"durationSeconds"`
	Message     string                `json:"message,omitempty"`
	Error       string                `json:"error,omitempty"`
}

// RemoveSession carries an in-flight remove between Plan and Apply. Plan
// validates targets and announces what removal will do; the caller gates the
// confirmation prompt and calls Apply to execute.
type RemoveSession struct {
	engine *Engine
	req    RemoveRequest

	cfg         *config.Config
	envName     string
	serverNames []string
	servers     map[string]config.ServerConfig
	services    map[string]config.ServiceConfig
	scoped      bool

	closed  bool
	applied bool
}

// ProjectName exposes the project name the confirmation prompt asks for.
func (s *RemoveSession) ProjectName() string {
	return s.cfg.Project.Name
}

// Environment returns the resolved target environment.
func (s *RemoveSession) Environment() string {
	return s.envName
}

// ServerNames returns the environment servers removal targets.
func (s *RemoveSession) ServerNames() []string {
	return append([]string(nil), s.serverNames...)
}

// Close releases the session. Remove acquires its remote resources inside
// Apply, so Close only invalidates the session. Idempotent.
func (s *RemoveSession) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
}

// PlanRemove validates the remove targets and emits the warning summary of
// what removal will and will not do. The returned session holds no remote
// resources; Apply acquires leases and executes the cleanup.
func (e *Engine) PlanRemove(ctx context.Context, req RemoveRequest) (*RemoveSession, error) {
	if req.Config == nil {
		return nil, invalidRequestf("remove request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("remove request requires an environment")
	}
	cfg := req.Config
	if err := RequireTakodRuntime(cfg); err != nil {
		return nil, err
	}

	envName := req.Environment
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, invalidRequestf("failed to get services for environment %s: %w", envName, err)
	}
	serverNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, invalidRequestf("failed to get environment servers: %w", err)
	}
	serverNames, err = ResolveRemoveTargetServers(envName, serverNames, req.Servers)
	if err != nil {
		return nil, err
	}
	if len(serverNames) == 0 {
		return nil, invalidRequestf("no servers configured for environment %s", envName)
	}
	servers := make(map[string]config.ServerConfig, len(serverNames))
	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, invalidRequestf("server %s not found in configuration", serverName)
		}
		servers[serverName] = server
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for _, server := range servers {
		e.RegisterSecret(server.Password)
	}
	for _, service := range services {
		for _, value := range service.Env {
			e.RegisterSecret(value)
		}
	}

	session := &RemoveSession{
		engine:      e,
		req:         req,
		cfg:         cfg,
		envName:     envName,
		serverNames: serverNames,
		servers:     servers,
		services:    services,
		scoped:      len(req.Servers) > 0,
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "\n⚠️  WARNING: You are about to REMOVE all services from:\n\n")
	fmt.Fprintf(&summary, "   Project:     %s\n", cfg.Project.Name)
	fmt.Fprintf(&summary, "   Environment: %s\n", envName)
	fmt.Fprintf(&summary, "   Servers:     %d\n\n", len(serverNames))
	for _, serverName := range serverNames {
		server := servers[serverName]
		fmt.Fprintf(&summary, "   • %s (%s)\n", serverName, server.Host)
	}
	summary.WriteString("\n")
	summary.WriteString("This will:\n")
	summary.WriteString("   • Stop and remove all service replicas for this project\n")
	summary.WriteString("   • Remove service images\n")
	summary.WriteString("   • Remove proxy configurations\n")
	summary.WriteString("   • Remove deployment state\n\n")
	summary.WriteString("This will NOT:\n")
	summary.WriteString("   • Decommission the server\n")
	summary.WriteString("   • Remove takod or tako-proxy\n")
	summary.WriteString("   • Remove persistent volume data\n\n")
	if session.scoped {
		summary.WriteString("Scoped cleanup: only the selected server(s) above are targeted.\n")
		summary.WriteString("Update tako.yaml after cleanup if you are retiring a node from the environment.\n\n")
	}
	e.emit(events.Event{
		Type:    events.TypePlanComputed,
		Phase:   events.PhasePlan,
		Level:   events.LevelInfo,
		Message: summary.String(),
		Data: map[string]any{
			"project":     cfg.Project.Name,
			"environment": envName,
			"servers":     len(serverNames),
			"scoped":      session.scoped,
		},
	})

	return session, nil
}

// Apply executes the planned remove. The caller gates confirmation.
func (s *RemoveSession) Apply(ctx context.Context) (*RemoveResult, error) {
	if s.closed {
		return nil, fmt.Errorf("remove session is closed")
	}
	if s.applied {
		return nil, fmt.Errorf("remove session was already applied")
	}
	s.applied = true

	e := s.engine
	cfg := s.cfg
	envName := s.envName
	startTime := time.Now()

	result := &RemoveResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindRemoveResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Scoped:      s.scoped,
		StartedAt:   startTime,
	}

	e.info(events.TypePhaseStarted, events.PhaseCleanup, fmt.Sprintf("\n🗑️  Removing all services for %s...\n\n", cfg.Project.Name))

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	leaseSet, err := AcquireRemoteOperationLeasesContext(ctx, sshPool, cfg, envName, s.serverNames, "remove")
	if err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(startTime).Seconds()
		return result, err
	}
	leaseSet.SetWarnFunc(func(message string) {
		e.debug(events.TypeWarning, events.PhaseDeploy, message)
	})
	defer leaseSet.Release()
	leaseCtx, cancelLeaseContext := leaseSet.BindContext(ctx)
	defer cancelLeaseContext()
	ctx = leaseCtx
	e.debug(events.TypeLogLine, events.PhaseCleanup, fmt.Sprintf("→ Acquired remote remove leases: %s\n", leaseSet.Summary()))

	e.info(events.TypeLogLine, events.PhaseCleanup, fmt.Sprintf("→ Reconciling cleanup through takod on %d node(s)...\n", len(s.serverNames)))
	request := RemoveCleanupRequest(cfg, envName, s.services)
	outcomes := collectCleanupNodeOutcomes(s.servers, func(_ string, serverCfg config.ServerConfig) (*takod.CleanupResponse, error) {
		return cleanupNodeViaTakod(ctx, cfg, sshPool, serverCfg, request)
	})

	var failures []string
	for _, outcome := range outcomes {
		serverOutcome := RemoveServerOutcome{Name: outcome.serverName, Host: outcome.host}
		if outcome.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", outcome.serverName, outcome.err))
			serverOutcome.Error = outcome.err.Error()
			result.Servers = append(result.Servers, serverOutcome)
			e.warn(events.PhaseCleanup, fmt.Sprintf("  ✗ %s failed: %v\n", outcome.serverName, outcome.err))
			continue
		}
		emitCleanupWarnings(e, outcome.response)
		serverOutcome.Removed = true
		result.Servers = append(result.Servers, serverOutcome)
		e.info(events.TypeCleanupCompleted, events.PhaseCleanup, fmt.Sprintf("  ✓ %s removed\n", outcome.serverName))
	}
	if len(failures) > 0 {
		err := fmt.Errorf("remove incomplete: %s", strings.Join(failures, "; "))
		result.Error = err.Error()
		result.Duration = time.Since(startTime).Seconds()
		return result, err
	}

	if s.scoped {
		e.info(events.TypePhaseCompleted, events.PhaseCleanup, fmt.Sprintf("\n✓ Services removed from selected server(s) in environment %s\n", envName))
		result.Message = fmt.Sprintf("services removed from selected server(s) in environment %s", envName)
	} else {
		e.info(events.TypePhaseCompleted, events.PhaseCleanup, fmt.Sprintf("\n✓ All services removed from environment %s\n", envName))
		result.Message = fmt.Sprintf("all services removed from environment %s", envName)
	}
	e.info(events.TypeLogLine, events.PhaseCleanup, "\nThe servers are still provisioned and ready for new deployments.\n")
	e.info(events.TypeLogLine, events.PhaseCleanup, "To deploy again: tako deploy\n")

	result.Duration = time.Since(startTime).Seconds()
	return result, nil
}

// ResolveRemoveTargetServers filters the --server selections against the
// environment's servers, preserving environment order and de-duplicating.
func ResolveRemoveTargetServers(envName string, environmentServers []string, selected []string) ([]string, error) {
	if len(selected) == 0 {
		return append([]string(nil), environmentServers...), nil
	}
	allowed := make(map[string]struct{}, len(environmentServers))
	for _, serverName := range environmentServers {
		allowed[serverName] = struct{}{}
	}
	selectedSet := make(map[string]struct{}, len(selected))
	for _, serverName := range selected {
		serverName = strings.TrimSpace(serverName)
		if serverName == "" {
			continue
		}
		if _, ok := allowed[serverName]; !ok {
			return nil, invalidRequestf("server %s is not listed in environment %s", serverName, envName)
		}
		selectedSet[serverName] = struct{}{}
	}
	targets := make([]string, 0, len(selectedSet))
	for _, serverName := range environmentServers {
		if _, ok := selectedSet[serverName]; ok {
			targets = append(targets, serverName)
		}
	}
	return targets, nil
}

// RemoveCleanupRequest builds the full app/stage takod cleanup request that
// remove (and destroy's decommission step) issues on each node.
func RemoveCleanupRequest(cfg *config.Config, envName string, services map[string]config.ServiceConfig) takod.CleanupRequest {
	return takod.CleanupRequest{
		Project:           cfg.Project.Name,
		Environment:       envName,
		RemoveContainers:  true,
		RemoveImages:      true,
		RemoveNetworks:    true,
		RemoveDeployFiles: true,
		RemoveTakodState:  true,
		ProxyFiles:        CleanupProxyFiles(cfg.Project.Name, envName, services),
		ImageRepositories: CleanupImageRepositories(cfg, envName, services),
		ExternalVolumes:   ExternalVolumeNamesForEnvironment(cfg, envName),
	}
}

// CleanupProxyFiles lists the proxy configuration files cleanup must delete
// for a project/environment, including maintenance-page configs for proxied
// services.
func CleanupProxyFiles(project string, environment string, services map[string]config.ServiceConfig) []string {
	seen := make(map[string]bool)
	add := func(name string) {
		if name != "" {
			seen[name] = true
		}
	}
	add(runtimeid.ProxyConfigFileName(project, environment))
	for serviceName, service := range services {
		if service.IsProxied() {
			add(runtimeid.MaintenanceProxyConfigFileName(project, environment, serviceName))
		}
	}
	files := make([]string, 0, len(seen))
	for name := range seen {
		files = append(files, name)
	}
	sort.Strings(files)
	return files
}

// emitCleanupWarnings surfaces takod cleanup warnings as debug events; the
// CLI historically printed them only with --verbose.
func emitCleanupWarnings(e *Engine, response *takod.CleanupResponse) {
	if response == nil {
		return
	}
	for _, warning := range response.Warnings {
		e.debug(events.TypeWarning, events.PhaseCleanup, fmt.Sprintf("  Warning: %s\n", warning))
	}
}

// cleanupNodeOutcome is one node's cleanup fan-out result.
type cleanupNodeOutcome struct {
	index      int
	serverName string
	host       string
	response   *takod.CleanupResponse
	err        error
}

// collectCleanupNodeOutcomes runs one cleanup action per node in parallel and
// returns results ordered by sorted server name.
func collectCleanupNodeOutcomes(servers map[string]config.ServerConfig, action func(serverName string, serverCfg config.ServerConfig) (*takod.CleanupResponse, error)) []cleanupNodeOutcome {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	resultCh := make(chan cleanupNodeOutcome, len(names))
	var wg sync.WaitGroup
	for index, serverName := range names {
		serverCfg := servers[serverName]
		wg.Add(1)
		go func(index int, serverName string, serverCfg config.ServerConfig) {
			defer wg.Done()
			response, err := action(serverName, serverCfg)
			resultCh <- cleanupNodeOutcome{
				index:      index,
				serverName: serverName,
				host:       serverCfg.Host,
				response:   response,
				err:        err,
			}
		}(index, serverName, serverCfg)
	}
	wg.Wait()
	close(resultCh)

	results := make([]cleanupNodeOutcome, len(names))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

// cleanupNodeViaTakod connects to one node and runs the cleanup request
// through takod.
func cleanupNodeViaTakod(ctx context.Context, cfg *config.Config, pool *ssh.Pool, serverCfg config.ServerConfig, request takod.CleanupRequest) (*takod.CleanupResponse, error) {
	client, err := pool.GetOrCreateWithAuth(serverCfg.Host, serverCfg.Port, serverCfg.User, serverCfg.SSHKey, serverCfg.Password)
	if err != nil {
		return nil, &ConnectivityError{Server: serverCfg.Host, Err: fmt.Errorf("failed to connect: %w", err)}
	}
	return CleanupViaTakodContext(ctx, client, cfg, request)
}
