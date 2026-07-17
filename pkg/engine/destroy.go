package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// KindDestroyResult identifies a serialized destroy result document.
const KindDestroyResult = "DestroyResult"

// Destroy modes.
const (
	DestroyModeDecommission = "DECOMMISSION"
	DestroyModePurge        = "PURGE"
)

// DestroyRequest describes one destroy operation: decommission this
// app/stage's runtime on every environment node, optionally purging
// app-owned leftovers. Config must be loaded and validated; Environment must
// be resolved.
type DestroyRequest struct {
	Config      *config.Config `json:"-"`
	Environment string         `json:"environment"`
	// PurgeAll also prunes app-owned leftovers after decommission.
	PurgeAll bool `json:"purgeAll,omitempty"`
	// Force mirrors the --force flag; it suppresses the production-server
	// warning that precedes the caller's confirmation prompt.
	Force   bool `json:"force,omitempty"`
	Verbose bool `json:"verbose,omitempty"`
}

// DestroyServerOutcome reports what happened on one node during destroy.
type DestroyServerOutcome struct {
	Name      string `json:"name"`
	Host      string `json:"host,omitempty"`
	Destroyed bool   `json:"destroyed"`
	Error     string `json:"error,omitempty"`
}

// DestroyResult is the serializable outcome of DestroySession.Apply.
type DestroyResult struct {
	APIVersion  string                 `json:"apiVersion"`
	Kind        string                 `json:"kind"`
	Project     string                 `json:"project"`
	Environment string                 `json:"environment"`
	Mode        string                 `json:"mode"`
	PurgeAll    bool                   `json:"purgeAll,omitempty"`
	Servers     []DestroyServerOutcome `json:"servers"`
	StartedAt   time.Time              `json:"startedAt"`
	Duration    float64                `json:"durationSeconds"`
	Message     string                 `json:"message,omitempty"`
	Error       string                 `json:"error,omitempty"`
}

// DestroySession carries an in-flight destroy between Plan and Apply. Plan
// acquires the local state lock and announces what destruction will do; the
// caller gates the confirmation prompt and calls Apply to execute. Close
// releases the state lock.
type DestroySession struct {
	engine *Engine
	req    DestroyRequest

	cfg           *config.Config
	envName       string
	mode          string
	serverNames   []string
	servers       map[string]config.ServerConfig
	hasProduction bool

	stateLock *localstate.StateLock
	lockInfo  *localstate.LockInfo

	closed  bool
	applied bool
}

// ProjectName exposes the project name the confirmation prompt asks for.
func (s *DestroySession) ProjectName() string {
	return s.cfg.Project.Name
}

// Environment returns the resolved target environment.
func (s *DestroySession) Environment() string {
	return s.envName
}

// ServerNames returns the environment servers destruction targets.
func (s *DestroySession) ServerNames() []string {
	return append([]string(nil), s.serverNames...)
}

// HasProduction reports whether any target server name looks like a
// production node.
func (s *DestroySession) HasProduction() bool {
	return s.hasProduction
}

// Close releases the local state lock. Idempotent.
func (s *DestroySession) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	if s.stateLock != nil && s.lockInfo != nil {
		_ = s.stateLock.Release(s.lockInfo)
	}
}

// PlanDestroy validates the destroy targets, acquires the local state lock,
// and emits the warning summary of what destruction will do. The returned
// session must be Closed; call Apply to execute the plan.
func (e *Engine) PlanDestroy(ctx context.Context, req DestroyRequest) (*DestroySession, error) {
	if req.Config == nil {
		return nil, invalidRequestf("destroy request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("destroy request requires an environment")
	}
	cfg := req.Config
	if err := RequireTakodRuntime(cfg); err != nil {
		return nil, err
	}

	session := &DestroySession{
		engine:  e,
		req:     req,
		cfg:     cfg,
		envName: req.Environment,
	}
	ok := false
	defer func() {
		if !ok {
			session.Close()
		}
	}()

	// Acquire state lock to prevent concurrent operations.
	session.stateLock = localstate.NewStateLock(".tako")
	lockInfo, err := session.stateLock.Acquire("destroy")
	if err != nil {
		holder := ""
		if info, infoErr := session.stateLock.GetLockInfo(); infoErr == nil && info != nil {
			holder = info.Who
		}
		session.stateLock = nil
		return nil, &LockedError{Operation: "destroy", Holder: holder, Err: fmt.Errorf("cannot destroy: %w", err)}
	}
	session.lockInfo = lockInfo

	serversToDestroy, targetServerNames, err := DestroyEnvironmentTargets(cfg, session.envName)
	if err != nil {
		return nil, err
	}
	session.servers = serversToDestroy
	session.serverNames = targetServerNames

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for _, server := range serversToDestroy {
		e.RegisterSecret(server.Password)
	}
	if services, servicesErr := cfg.GetServices(session.envName); servicesErr == nil {
		for _, service := range services {
			for _, value := range service.Env {
				e.RegisterSecret(value)
			}
		}
	}

	mode := DestroyModeDecommission
	if req.PurgeAll {
		mode = DestroyModePurge
	}
	session.mode = mode

	var summary strings.Builder
	fmt.Fprintf(&summary, "⚠️  WARNING: You are about to %s this app/stage on the following node(s):\n\n", mode)
	for _, serverName := range targetServerNames {
		server := serversToDestroy[serverName]
		fmt.Fprintf(&summary, "   • %s (%s)\n", serverName, server.Host)
	}
	fmt.Fprintf(&summary, "\n%s MODE will:\n", mode)
	summary.WriteString("   ✓ Stop and remove this app/stage service replicas\n")
	summary.WriteString("   ✓ Remove this app/stage service images\n")
	summary.WriteString("   ✓ Remove this app/stage deployment files and directories\n")
	if req.PurgeAll {
		summary.WriteString("   ✓ Prune unused app-owned volumes\n")
		summary.WriteString("   ✓ Prune stopped app containers and old app images\n")
		summary.WriteString("\nPreserving unrelated projects and shared server setup (takod, tako-proxy, logs)\n")
	} else {
		summary.WriteString("\nPreserving unrelated projects and server setup (takod, tako-proxy, logs)\n")
		summary.WriteString("You can redeploy without running 'tako setup' again\n")
	}
	e.emit(events.Event{
		Type:    events.TypePlanComputed,
		Phase:   events.PhasePlan,
		Level:   events.LevelInfo,
		Message: summary.String(),
		Data: map[string]any{
			"project":     cfg.Project.Name,
			"environment": session.envName,
			"mode":        mode,
			"purgeAll":    req.PurgeAll,
			"servers":     len(targetServerNames),
		},
	})

	// Check if any production servers.
	hasProduction := false
	for serverName := range serversToDestroy {
		if strings.Contains(strings.ToLower(serverName), "prod") {
			hasProduction = true
			break
		}
	}
	session.hasProduction = hasProduction

	if hasProduction && !req.Force {
		e.info(events.TypePlanConfirmationRequired, events.PhasePlan, "\n🚨 PRODUCTION SERVER DETECTED!\n   This is a destructive operation.\n")
	}

	ok = true
	return session, nil
}

// Apply executes the planned destroy. The caller gates confirmation.
func (s *DestroySession) Apply(ctx context.Context) (*DestroyResult, error) {
	if s.closed {
		return nil, fmt.Errorf("destroy session is closed")
	}
	if s.applied {
		return nil, fmt.Errorf("destroy session was already applied")
	}
	s.applied = true

	e := s.engine
	cfg := s.cfg
	envName := s.envName
	startTime := time.Now()

	result := &DestroyResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindDestroyResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Mode:        s.mode,
		PurgeAll:    s.req.PurgeAll,
		StartedAt:   startTime,
	}

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	runtimeFactory, err := nodeclient.NewFactory(cfg, sshPool, TakodSocketFromConfig(cfg))
	if err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(startTime).Seconds()
		return result, err
	}
	defer runtimeFactory.CloseIdleConnections()
	leaseSet, err := AcquireRemoteOperationLeasesContext(ctx, sshPool, cfg, envName, s.serverNames, "destroy")
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
	e.debug(events.TypeLogLine, events.PhaseCleanup, fmt.Sprintf("→ Acquired remote destroy leases: %s\n", leaseSet.Summary()))

	e.info(events.TypePhaseStarted, events.PhaseCleanup, fmt.Sprintf("\n🗑️  Removing app runtime on %d node(s)...\n\n", len(s.servers)))

	totalErrors := 0
	for _, serverName := range s.serverNames {
		if err := ctx.Err(); err != nil {
			result.Error = err.Error()
			result.Duration = time.Since(startTime).Seconds()
			return result, err
		}
		serverCfg := s.servers[serverName]
		e.info(events.TypeLogLine, events.PhaseCleanup, fmt.Sprintf("=== Removing app runtime on node: %s (%s) ===\n", serverName, serverCfg.Host))
		outcome := DestroyServerOutcome{Name: serverName, Host: serverCfg.Host}
		client, _, connectErr := runtimeFactory.Client(ctx, serverName)
		var destroyErr error
		if connectErr != nil {
			destroyErr = &ConnectivityError{Server: serverName, Err: fmt.Errorf("failed to connect to %s: %w", serverName, connectErr)}
		} else if destroyErr = s.decommissionApp(ctx, client, cfg, envName, s.req.Verbose); destroyErr == nil && s.req.PurgeAll {
			destroyErr = s.purgeProjectRuntime(ctx, client, cfg, envName, s.req.Verbose)
		}
		if destroyErr != nil {
			e.warn(events.PhaseCleanup, fmt.Sprintf("⚠️  Errors removing app runtime on %s: %v\n", serverName, destroyErr))
			outcome.Error = destroyErr.Error()
			totalErrors++
		} else {
			e.info(events.TypeCleanupCompleted, events.PhaseCleanup, fmt.Sprintf("✓ App runtime removed on %s\n\n", serverName))
			outcome.Destroyed = true
		}
		result.Servers = append(result.Servers, outcome)
	}

	// Summary
	if totalErrors > 0 {
		e.warn(events.PhaseCleanup, fmt.Sprintf("⚠️  Destroy completed with %d errors\n   Run with -v (verbose) flag for more details\n", totalErrors))
		err := fmt.Errorf("destroy incomplete: %d server(s) failed", totalErrors)
		result.Error = err.Error()
		result.Duration = time.Since(startTime).Seconds()
		return result, err
	}

	e.info(events.TypePhaseCompleted, events.PhaseCleanup, "✨ All servers destroyed successfully!\n")
	if s.req.PurgeAll {
		e.info(events.TypeLogLine, events.PhaseCleanup, "\n💡 App-owned leftovers pruned. Shared server setup was preserved.\n")
		result.Message = "app-owned leftovers pruned; shared server setup preserved"
	} else {
		e.info(events.TypeLogLine, events.PhaseCleanup, "\n💡 Server setup preserved. You can redeploy without running 'tako setup'.\n")
		result.Message = "server setup preserved; redeploy without running 'tako setup'"
	}

	result.Duration = time.Since(startTime).Seconds()
	return result, nil
}

// DestroyEnvironmentTargets resolves the environment's servers into the
// destroy target set, preserving environment order.
func DestroyEnvironmentTargets(cfg *config.Config, envName string) (map[string]config.ServerConfig, []string, error) {
	envServerNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, nil, invalidRequestf("failed to get environment servers: %w", err)
	}
	if len(envServerNames) == 0 {
		return nil, nil, invalidRequestf("no servers configured for environment %s", envName)
	}

	servers := make(map[string]config.ServerConfig, len(envServerNames))
	for _, serverName := range envServerNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, nil, invalidRequestf("server %s not found in config", serverName)
		}
		servers[serverName] = server
	}
	return servers, append([]string(nil), envServerNames...), nil
}

// SSHClientProvider supplies pooled SSH clients; *ssh.Pool implements it.
type SSHClientProvider interface {
	GetOrCreateWithAuth(host string, port int, user string, keyPath string, password string) (*ssh.Client, error)
}

// DestroySingleServerWithHooks connects to one node, decommissions the
// app/stage runtime, and purges app-owned leftovers when requested. Purge
// never runs after a failed decommission.
func DestroySingleServerWithHooks(pool SSHClientProvider, serverName string, serverCfg config.ServerConfig, cfg *config.Config, envName string, verbose bool, purgeAll bool, decommission func(*ssh.Client, *config.Config, string, bool) error, purge func(*ssh.Client, *config.Config, string, bool) error) error {
	return DestroySingleServerWithHooksContext(
		context.Background(), pool, serverName, serverCfg, cfg, envName, verbose, purgeAll,
		func(_ context.Context, client *ssh.Client, cfg *config.Config, envName string, verbose bool) error {
			return decommission(client, cfg, envName, verbose)
		},
		func(_ context.Context, client *ssh.Client, cfg *config.Config, envName string, verbose bool) error {
			return purge(client, cfg, envName, verbose)
		},
	)
}

// DestroySingleServerWithHooksContext is the context-aware destroy primitive.
// It stops before each destructive phase when lease loss or caller
// cancellation cancels ctx.
func DestroySingleServerWithHooksContext(ctx context.Context, pool SSHClientProvider, serverName string, serverCfg config.ServerConfig, cfg *config.Config, envName string, verbose bool, purgeAll bool, decommission func(context.Context, *ssh.Client, *config.Config, string, bool) error, purge func(context.Context, *ssh.Client, *config.Config, string, bool) error) error {
	if pool == nil {
		return fmt.Errorf("ssh pool is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	client, err := pool.GetOrCreateWithAuth(serverCfg.Host, serverCfg.Port, serverCfg.User, serverCfg.SSHKey, serverCfg.Password)
	if err != nil {
		return &ConnectivityError{Server: serverName, Err: fmt.Errorf("failed to connect to %s: %w", serverName, err)}
	}

	// Decommission application
	if err := decommission(ctx, client, cfg, envName, verbose); err != nil {
		return fmt.Errorf("decommission failed: %w", err)
	}

	// Prune app-owned leftovers if requested.
	if purgeAll {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := purge(ctx, client, cfg, envName, verbose); err != nil {
			return fmt.Errorf("purge failed: %w", err)
		}
	}

	return nil
}

// decommissionApp stops and removes the deployed application. Progress that
// the CLI printed only with --verbose is emitted as debug events; renderers
// filter by level.
func (s *DestroySession) decommissionApp(ctx context.Context, client any, cfg *config.Config, envName string, _ bool) error {
	e := s.engine
	e.debug(events.TypeLogLine, events.PhaseCleanup, "  → Removing takod-managed services...\n")
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services for environment %s: %w", envName, err)
	}
	response, err := CleanupViaTakodContext(ctx, client, cfg, RemoveCleanupRequest(cfg, envName, services))
	if err != nil {
		return err
	}
	emitCleanupWarnings(e, response)

	e.debug(events.TypeLogLine, events.PhaseCleanup, "  ✓ Application decommissioned\n")

	return nil
}

// purgeProjectRuntime removes app-owned leftovers without touching shared
// takod or tako-proxy runtime used by other projects on the same node.
func (s *DestroySession) purgeProjectRuntime(ctx context.Context, client any, cfg *config.Config, envName string, _ bool) error {
	e := s.engine
	e.debug(events.TypeLogLine, events.PhaseCleanup, "  → Pruning app-owned leftovers...\n")
	response, err := CleanupViaTakodContext(ctx, client, cfg, takod.CleanupRequest{
		Project:         cfg.Project.Name,
		Environment:     envName,
		PruneDocker:     true,
		ExternalVolumes: ExternalVolumeNamesForEnvironment(cfg, envName),
	})
	if err != nil {
		return err
	}
	emitCleanupWarnings(e, response)

	e.debug(events.TypeLogLine, events.PhaseCleanup, "  ✓ App-owned leftovers pruned\n")

	return nil
}
