package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/notification"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

// KindRollbackResult identifies a serialized rollback result document.
const KindRollbackResult = "RollbackResult"

// RollbackHistorySourceFunc selects the freshest reachable deployment history
// from the mesh, returning the source node name and its history document. The
// cmd layer supplies it because the shared mesh-history collection helpers
// (collectStateDeploymentHistories, bestDeploymentHistory) live in cmd.
type RollbackHistorySourceFunc func() (source string, history *remotestate.DeploymentHistory, err error)

// ListDeploymentsFunc filters and orders deployments from a history document.
// The cmd layer supplies it because the shared listing helper is also used by
// `tako history` and stays in cmd.
type ListDeploymentsFunc func(history *remotestate.DeploymentHistory, opts *remotestate.HistoryOptions) []*remotestate.DeploymentState

// RollbackRequest describes one rollback operation. Config must be loaded and
// validated; Environment must be resolved.
type RollbackRequest struct {
	Config      *config.Config
	Environment string
	// Service is the --service flag (required).
	Service string
	// DeploymentID is the optional positional deployment-id argument; empty
	// rolls back to the previous stable deployment for the service.
	DeploymentID string
	Verbose      bool

	// HistorySource and ListDeployments are seams for history helpers shared
	// with other cmd files; see the type docs.
	HistorySource   RollbackHistorySourceFunc
	ListDeployments ListDeploymentsFunc
}

// RollbackResult is the serializable outcome of Rollback.
type RollbackResult struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	// DeploymentID is the target deployment the service was rolled back to.
	DeploymentID string                   `json:"deploymentId"`
	Version      string                   `json:"version,omitempty"`
	Status       takoapi.DeploymentStatus `json:"status"`
	StartedAt    time.Time                `json:"startedAt"`
	Duration     float64                  `json:"durationSeconds"`
	Message      string                   `json:"message,omitempty"`
}

// Rollback re-deploys a service from a recorded deployment state, reconciles
// the proxy, and persists the rolled-back state across the mesh. Rollback has
// no interactive confirmation, so it runs as a single method rather than a
// plan/apply session.
func (e *Engine) Rollback(ctx context.Context, req RollbackRequest) (*RollbackResult, error) {
	if req.Config == nil {
		return nil, invalidRequestf("rollback request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("rollback request requires an environment")
	}
	if strings.TrimSpace(req.Service) == "" {
		return nil, invalidRequestf("rollback request requires a service")
	}
	if req.HistorySource == nil {
		return nil, invalidRequestf("rollback request requires a history source")
	}
	if req.ListDeployments == nil {
		return nil, invalidRequestf("rollback request requires a deployment lister")
	}
	cfg := req.Config
	envName := req.Environment
	if err := RequireTakodRuntime(cfg); err != nil {
		return nil, err
	}

	// Acquire state lock to prevent concurrent operations.
	stateLock := localstate.NewStateLock(".tako")
	lockInfo, err := stateLock.Acquire("rollback")
	if err != nil {
		holder := ""
		if info, infoErr := stateLock.GetLockInfo(); infoErr == nil && info != nil {
			holder = info.Who
		}
		return nil, &LockedError{Operation: "rollback", Holder: holder, Err: fmt.Errorf("cannot rollback: %w", err)}
	}
	defer stateLock.Release(lockInfo)

	// Get services for the environment.
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get services: %w", err)
	}

	// Check service exists in environment.
	if _, exists := services[req.Service]; !exists {
		return nil, invalidRequestf("service %s not found in environment %s", req.Service, envName)
	}
	rollbackService := services[req.Service]
	if rollbackService.IsRun() || rollbackService.IsJob() {
		return nil, invalidRequestf("service %s is kind: %s and cannot be rolled back as a long-running service", req.Service, rollbackService.Kind)
	}

	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(envServers) == 0 {
		return nil, invalidRequestf("no servers configured for environment %s", envName)
	}

	// Register sensitive values with the redactor before emitting anything
	// that could contain them.
	for _, service := range services {
		for _, value := range service.Env {
			e.RegisterSecret(value)
		}
	}
	for _, server := range cfg.Servers {
		e.RegisterSecret(server.Password)
	}

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	leaseSet, err := AcquireRemoteOperationLeasesContext(ctx, sshPool, cfg, envName, envServers, "rollback")
	if err != nil {
		return nil, err
	}
	defer leaseSet.Release()
	leaseCtx, cancelLeaseContext := leaseSet.BindContext(ctx)
	defer cancelLeaseContext()
	ctx = leaseCtx
	leaseSet.SetWarnFunc(func(message string) {
		e.debug(events.TypeWarning, events.PhaseDeploy, message)
	})
	e.debug(events.TypeLogLine, events.PhaseDeploy, fmt.Sprintf("→ Acquired remote rollback leases: %s\n", leaseSet.Summary()))

	sourceName, history, err := req.HistorySource()
	if err != nil {
		return nil, err
	}
	server, exists := cfg.Servers[sourceName]
	if !exists {
		return nil, invalidRequestf("server %s not found in configuration", sourceName)
	}
	e.debug(events.TypeLogLine, events.PhaseDeploy, fmt.Sprintf("Reading rollback state from node: %s (%s)\n", sourceName, server.Host))

	client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	if err != nil {
		return nil, &ConnectivityError{Server: sourceName, Err: fmt.Errorf("failed to connect to node %s: %w", sourceName, err)}
	}

	// Create state manager.
	stateManager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, TakodSocketFromConfig(cfg))

	// Determine which deployment to rollback to.
	if req.DeploymentID != "" {
		e.emit(events.Event{
			Type:    events.TypeDeployStarted,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: req.Service,
			Message: fmt.Sprintf("\n=== Rolling back to deployment: %s ===\n\n", req.DeploymentID),
		})
	} else {
		e.emit(events.Event{
			Type:    events.TypeDeployStarted,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: req.Service,
			Message: "\n=== Rolling back to previous successful deployment ===\n\n",
		})
	}
	targetDeployment, err := SelectRollbackTargetFromHistory(history, req.DeploymentID, req.Service, req.ListDeployments)
	if err != nil {
		return nil, &InvalidRequestError{Err: err}
	}

	// Display deployment info.
	e.info(events.TypeLogLine, events.PhaseDeploy, fmt.Sprintf(
		"Target deployment:\n  ID:        %s\n  Timestamp: %s\n  Version:   %s\n  User:      %s\n\n",
		targetDeployment.ID,
		targetDeployment.Timestamp.Format("2006-01-02 15:04:05"),
		targetDeployment.Version,
		targetDeployment.User))

	// Setup notifications if configured.
	var notifier *notification.Notifier
	if cfg.Notifications != nil && (cfg.Notifications.Slack != "" || cfg.Notifications.Discord != "" || cfg.Notifications.Webhook != "") {
		notifier = notification.NewNotifier(notification.NotifierConfig{
			SlackWebhook:   cfg.Notifications.Slack,
			DiscordWebhook: cfg.Notifications.Discord,
			Webhook:        cfg.Notifications.Webhook,
		}, req.Verbose)

		// Send rollback started notification.
		notifier.Notify(notification.Event{
			Type:        notification.EventRollbackStarted,
			Project:     cfg.Project.Name,
			Environment: envName,
			Service:     req.Service,
			Message:     fmt.Sprintf("Rolling back `%s` to deployment `%s` (version %s)", req.Service, targetDeployment.ID, targetDeployment.Version),
			Details: map[string]string{
				"deployment_id": targetDeployment.ID,
				"version":       targetDeployment.Version,
				"user":          remotestate.GetCurrentUser(),
			},
		})
	}

	startTime := time.Now()

	result := &RollbackResult{
		APIVersion:   takoapi.APIVersionCurrent,
		Kind:         KindRollbackResult,
		Project:      cfg.Project.Name,
		Environment:  envName,
		Service:      req.Service,
		DeploymentID: targetDeployment.ID,
		Version:      targetDeployment.Version,
		StartedAt:    startTime,
	}

	deploy := deployer.NewDeployerWithPool(client, cfg, envName, sshPool, req.Verbose)
	deploy.SetBaseContext(ctx)
	deploy.SetCLIVersion(e.cliVersion)
	if err := deploy.SetTargetServers(envServers); err != nil {
		return nil, err
	}
	if err := deploy.SetupTakodRuntime(); err != nil {
		return nil, fmt.Errorf("failed to setup takod runtime: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Perform rollback using state.
	serviceState := targetDeployment.Services[req.Service]
	if err := e.rollbackToTargetState(deploy, req.Service, services[req.Service], targetDeployment, serviceState); err != nil {
		// Send failure notification.
		if notifier != nil {
			notifier.Notify(notification.Event{
				Type:        notification.EventDeployFailed,
				Project:     cfg.Project.Name,
				Environment: envName,
				Service:     req.Service,
				Message:     fmt.Sprintf("Rollback of `%s` failed", req.Service),
				Error:       err.Error(),
			})
		}
		return nil, fmt.Errorf("rollback failed: %w", err)
	}

	rollbackDuration := time.Since(startTime)

	rollbackDeployment := BuildRollbackDeployment(cfg, envName, server.Host, startTime, rollbackDuration, targetDeployment, req.Service, serviceState, e.cliVersion, e.cliCommit)
	if err := stateManager.SaveDeploymentContext(ctx, rollbackDeployment); err != nil {
		return nil, RollbackRemoteHistoryError(err)
	}

	actualStateForProxy, err := reconcile.GatherActualStateFromServers(sshPool, cfg, envName, envServers, nil)
	if err != nil {
		return nil, fmt.Errorf("rollback succeeded but failed to gather actual state before proxy reconciliation: %w", err)
	}
	desiredServices, rollbackImageRefs, activeRevisions := RollbackProxyInputs(cfg, envName, services, req.Service, serviceState, actualStateForProxy)

	if err := ReconcileProxy(deploy, desiredServices, activeRevisions); err != nil {
		return nil, fmt.Errorf("rollback succeeded but failed to reconcile proxy: %w", err)
	}
	if err := deploy.PruneTakodServiceRevisions(desiredServices, deployplan.DeployedProxyActiveRevisions(map[string]config.ServiceConfig{req.Service: desiredServices[req.Service]}, activeRevisions)); err != nil {
		return nil, fmt.Errorf("rollback succeeded but failed to prune stale service revisions: %w", err)
	}

	postRollbackNodeActualState, err := reconcile.GatherActualStateByServer(sshPool, cfg, envName, envServers)
	if err != nil {
		return nil, fmt.Errorf("rollback succeeded but failed to gather post-rollback actual state: %w", err)
	}
	postRollbackActualState := reconcile.AggregateActualStateByServer(postRollbackNodeActualState)
	runtimeImageRefs := deployplan.MergeRuntimeImageRefs(cfg, envName, desiredServices, rollbackImageRefs, postRollbackActualState)
	if err := PersistTakodRuntimeState(
		sshPool,
		cfg,
		envName,
		envServers,
		"rollback",
		desiredServices,
		runtimeImageRefs,
		postRollbackActualState,
		postRollbackNodeActualState,
		takodstate.GitInfo{
			Commit:      targetDeployment.GitCommit,
			CommitShort: targetDeployment.GitCommitShort,
			Branch:      targetDeployment.GitBranch,
			Message:     targetDeployment.GitCommitMsg,
			Author:      targetDeployment.GitAuthor,
		},
		"rollback.succeeded",
		fmt.Sprintf("rolled back %s to deployment %s", req.Service, targetDeployment.ID),
		map[string]string{
			"service":      req.Service,
			"deploymentId": targetDeployment.ID,
		},
		req.Verbose,
	); err != nil {
		return nil, fmt.Errorf("rollback succeeded but failed to persist takod state: %w", err)
	}
	e.debug(events.TypeStatePersisted, events.PhaseState, "")

	// Replicate updated state to mesh nodes.
	if cfg.IsMultiServer() {
		replicator := remotestate.NewStateReplicator(sshPool, cfg, envName, cfg.Project.Name, req.Verbose)
		history, err := stateManager.LoadHistoryContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("rollback succeeded but failed to load remote deployment history for replication: %w", err)
		}
		if err := replicator.ReplicateDeploymentContext(ctx, rollbackDeployment, history); err != nil {
			return nil, fmt.Errorf("rollback succeeded but failed to replicate remote deployment history: %w", err)
		}
		e.debug(events.TypeStateReplicated, events.PhaseState, "")
	}

	e.saveLocalRollbackState(cfg, envName, envServers, rollbackDeployment, req.Service, targetDeployment.ID)

	// Send success notification.
	if notifier != nil {
		notifier.Notify(notification.Event{
			Type:        notification.EventRollbackDone,
			Project:     cfg.Project.Name,
			Environment: envName,
			Service:     req.Service,
			Message:     fmt.Sprintf("Successfully rolled back `%s` to version %s in %s", req.Service, targetDeployment.Version, rollbackDuration.Round(time.Second)),
			Duration:    rollbackDuration,
			Details: map[string]string{
				"deployment_id": targetDeployment.ID,
				"version":       targetDeployment.Version,
			},
		})
	}

	e.info(events.TypeDeploySucceeded, events.PhaseDeploy, fmt.Sprintf("\n✓ Successfully rolled back to deployment %s!\n", targetDeployment.ID))

	result.Status = takoapi.DeploymentStatus(remotestate.StatusRolledBack)
	result.Message = rollbackDeployment.Message
	result.Duration = time.Since(startTime).Seconds()
	return result, nil
}

func (e *Engine) rollbackToTargetState(
	deploy *deployer.Deployer,
	serviceName string,
	service config.ServiceConfig,
	targetDeployment *remotestate.DeploymentState,
	serviceState remotestate.ServiceState,
) error {
	if !RollbackNeedsTargetWorktree(service, targetDeployment) {
		return deploy.RollbackToState(serviceName, &serviceState)
	}

	commit := RollbackTargetCommit(targetDeployment)
	e.debug(events.TypeLogLine, events.PhaseDeploy, fmt.Sprintf("  Rebuilding rollback image from commit %s...\n", commit))
	return withRollbackGitWorktree(commit, func(worktreeDir string) error {
		return WithWorkingDirectory(worktreeDir, func() error {
			return deploy.RollbackToState(serviceName, &serviceState)
		})
	})
}

// RollbackNeedsTargetWorktree reports whether a rollback must rebuild the
// service image from the target deployment's git commit.
func RollbackNeedsTargetWorktree(service config.ServiceConfig, targetDeployment *remotestate.DeploymentState) bool {
	return strings.TrimSpace(service.Build) != "" && RollbackTargetCommit(targetDeployment) != ""
}

// RollbackTargetCommit resolves the git commit recorded with the rollback
// target, preferring the full hash over the short form.
func RollbackTargetCommit(targetDeployment *remotestate.DeploymentState) string {
	if targetDeployment == nil {
		return ""
	}
	if commit := strings.TrimSpace(targetDeployment.GitCommit); commit != "" {
		return commit
	}
	return strings.TrimSpace(targetDeployment.GitCommitShort)
}

func withRollbackGitWorktree(commit string, fn func(worktreeDir string) error) error {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return fmt.Errorf("rollback target deployment does not include a git commit for rebuild")
	}

	tempRoot, err := os.MkdirTemp("", "tako-rollback-worktree-*")
	if err != nil {
		return fmt.Errorf("failed to create rollback worktree directory: %w", err)
	}
	defer os.RemoveAll(tempRoot)

	worktreeDir := filepath.Join(tempRoot, "source")
	if err := runRollbackGit("worktree", "add", "--detach", "--quiet", worktreeDir, commit); err != nil {
		return fmt.Errorf("failed to create rollback worktree for commit %s: %w", commit, err)
	}
	defer func() {
		_ = runRollbackGit("worktree", "remove", "--force", worktreeDir)
	}()

	return fn(worktreeDir)
}

// WithWorkingDirectory runs fn from dir, restoring the original working
// directory afterwards.
func WithWorkingDirectory(dir string, fn func() error) error {
	currentDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to read current working directory: %w", err)
	}
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("failed to enter rollback worktree: %w", err)
	}
	defer os.Chdir(currentDir)
	return fn()
}

func runRollbackGit(args ...string) error {
	cmd := exec.Command("git", args...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		return fmt.Errorf("git %s failed: %w", strings.Join(args, " "), err)
	}
	return fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, message)
}

// RollbackProxyInputs derives the desired services, image refs, and active
// revisions used to reconcile the proxy after a rollback.
func RollbackProxyInputs(
	cfg *config.Config,
	envName string,
	services map[string]config.ServiceConfig,
	rollbackService string,
	serviceState remotestate.ServiceState,
	actualState map[string]*reconcile.ActualService,
) (map[string]config.ServiceConfig, map[string]string, map[string]string) {
	desiredServices := CloneServiceMap(services)
	rollbackConfig := desiredServices[rollbackService]
	if serviceState.SharedBuildHash != "" {
		rollbackConfig.ClearBuild()
		rollbackConfig.Image = ""
		rollbackConfig.ImageFrom = serviceState.SharedBuild
		rollbackConfig.SharedBuildHash = serviceState.SharedBuildHash
	} else {
		rollbackConfig.Image = serviceState.Image
		rollbackConfig.ImageFrom = ""
		rollbackConfig.SharedBuildHash = ""
	}
	rollbackConfig.Replicas = serviceState.Replicas
	if serviceState.Port > 0 {
		rollbackConfig.Port = serviceState.Port
	}
	desiredServices[rollbackService] = rollbackConfig

	rollbackServices := map[string]config.ServiceConfig{rollbackService: rollbackConfig}
	rollbackImageRefs := map[string]string{rollbackService: serviceState.Image}
	activeRevisions := deployplan.ProxyActiveRevisions(cfg, envName, desiredServices, rollbackServices, rollbackImageRefs, actualState)
	return desiredServices, rollbackImageRefs, activeRevisions
}

// DeploymentFromHistory finds a deployment by ID in a history document.
func DeploymentFromHistory(history *remotestate.DeploymentHistory, deploymentID string) (*remotestate.DeploymentState, error) {
	if history == nil {
		return nil, fmt.Errorf("deployment %s not found", deploymentID)
	}
	for _, deployment := range history.Deployments {
		if deployment != nil && deployment.ID == deploymentID {
			return deployment, nil
		}
	}
	return nil, fmt.Errorf("deployment %s not found", deploymentID)
}

// SelectRollbackTargetFromHistory resolves the rollback target: the requested
// deployment when an ID is given, otherwise the previous stable deployment for
// the service.
func SelectRollbackTargetFromHistory(history *remotestate.DeploymentHistory, deploymentID string, serviceName string, listDeployments ListDeploymentsFunc) (*remotestate.DeploymentState, error) {
	if deploymentID != "" {
		deployment, err := DeploymentFromHistory(history, deploymentID)
		if err != nil {
			return nil, fmt.Errorf("failed to find deployment %s: %w", deploymentID, err)
		}
		if err := validateRollbackTarget(deployment, serviceName); err != nil {
			return nil, err
		}
		return deployment, nil
	}
	deployment, err := PreviousStableServiceDeploymentFromHistory(history, serviceName, listDeployments)
	if err != nil {
		return nil, fmt.Errorf("failed to find previous deployment: %w", err)
	}
	return deployment, nil
}

func validateRollbackTarget(deployment *remotestate.DeploymentState, serviceName string) error {
	if deployment == nil {
		return fmt.Errorf("deployment is nil")
	}
	if !isRollbackStableStatus(deployment.Status) {
		return fmt.Errorf("deployment %s has status %s and is not a stable rollback target", deployment.ID, deployment.Status)
	}
	if _, exists := deployment.Services[serviceName]; !exists {
		return fmt.Errorf("service %s not found in deployment %s", serviceName, deployment.ID)
	}
	return nil
}

// PreviousStableServiceDeploymentFromHistory finds the most recent stable
// deployment for a service that precedes its current deployment.
func PreviousStableServiceDeploymentFromHistory(history *remotestate.DeploymentHistory, serviceName string, listDeployments ListDeploymentsFunc) (*remotestate.DeploymentState, error) {
	if listDeployments == nil {
		return nil, fmt.Errorf("deployment lister is required")
	}
	deployments := listDeployments(history, &remotestate.HistoryOptions{
		Limit:         0,
		IncludeFailed: true,
	})
	seenCurrentServiceDeployment := false
	for _, deployment := range deployments {
		if deployment == nil {
			continue
		}
		if _, exists := deployment.Services[serviceName]; !exists {
			continue
		}
		if !seenCurrentServiceDeployment {
			seenCurrentServiceDeployment = true
			continue
		}
		if !isRollbackStableStatus(deployment.Status) {
			continue
		}
		return deployment, nil
	}
	return nil, fmt.Errorf("no previous stable deployment found for service %s", serviceName)
}

func isRollbackStableStatus(status remotestate.DeploymentStatus) bool {
	return status == remotestate.StatusSuccess || status == remotestate.StatusRolledBack
}

// RollbackRemoteHistoryError wraps failures to persist history after the
// runtime mutation already succeeded.
func RollbackRemoteHistoryError(err error) error {
	return fmt.Errorf("rollback succeeded but failed to update remote deployment history: %w", err)
}

// BuildRollbackDeployment builds the deployment record persisted after a
// successful rollback.
func BuildRollbackDeployment(
	cfg *config.Config,
	envName string,
	host string,
	startTime time.Time,
	duration time.Duration,
	targetDeployment *remotestate.DeploymentState,
	serviceName string,
	serviceState remotestate.ServiceState,
	cliVersion string,
	cliCommit string,
) *remotestate.DeploymentState {
	return &remotestate.DeploymentState{
		Timestamp:      startTime,
		ProjectName:    cfg.Project.Name,
		Environment:    envName,
		Version:        targetDeployment.Version,
		Status:         remotestate.StatusRolledBack,
		Services:       map[string]remotestate.ServiceState{serviceName: serviceState},
		User:           remotestate.GetCurrentUser(),
		Host:           host,
		Duration:       duration,
		Message:        fmt.Sprintf("rolled back %s to deployment %s", serviceName, targetDeployment.ID),
		GitCommit:      targetDeployment.GitCommit,
		GitCommitShort: targetDeployment.GitCommitShort,
		GitBranch:      targetDeployment.GitBranch,
		GitCommitMsg:   targetDeployment.GitCommitMsg,
		GitAuthor:      targetDeployment.GitAuthor,
		CLIVersion:     cliVersion,
		CLICommit:      cliCommit,
	}
}

func (e *Engine) saveLocalRollbackState(cfg *config.Config, envName string, serverNames []string, deployment *remotestate.DeploymentState, serviceName string, targetDeploymentID string) {
	localStateMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		e.debug(events.TypeWarning, events.PhaseState, fmt.Sprintf("Warning: failed to initialize local state for rollback: %v\n", err))
		return
	}
	localDeployment := &localstate.DeploymentState{
		DeploymentID:    fmt.Sprintf("rollback-%s", deployment.Timestamp.Format("20060102-150405")),
		Timestamp:       deployment.Timestamp,
		Environment:     envName,
		Mode:            cfg.GetRuntimeMode(),
		Servers:         append([]string(nil), serverNames...),
		Status:          string(remotestate.StatusRolledBack),
		DurationSeconds: int(deployment.Duration.Seconds()),
		GitCommit:       deployment.GitCommit,
		TriggeredBy:     remotestate.GetCurrentUser(),
		Notes:           fmt.Sprintf("Rolled back %s to deployment %s", serviceName, targetDeploymentID),
	}
	if err := localStateMgr.SaveDeployment(localDeployment); err != nil {
		e.debug(events.TypeWarning, events.PhaseState, fmt.Sprintf("Warning: failed to save local rollback state: %v\n", err))
	}
}
