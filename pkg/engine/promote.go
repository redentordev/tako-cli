package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

// KindPromoteResult identifies a serialized promotion result document.
const KindPromoteResult = "PromoteResult"

// PromoteRequest describes one manual blue-green promotion. Config must be
// loaded and validated; Environment must be resolved.
type PromoteRequest struct {
	Config      *config.Config
	Environment string
	// ServiceName is the positional SERVICE argument.
	ServiceName string
	// Revision is the --revision flag: a full warmed revision ID or a unique
	// prefix. Empty selects the single warmed revision.
	Revision string
	Verbose  bool
	// ShortRevision formats revision IDs for display in the final success
	// message. The cmd layer injects its shared shortRevision helper (also
	// used by `tako ps`, so it stays in cmd); nil leaves revisions unshortened.
	ShortRevision func(string) string
}

// PromoteResult is the serializable outcome of Promote.
type PromoteResult struct {
	APIVersion  string                   `json:"apiVersion"`
	Kind        string                   `json:"kind"`
	Project     string                   `json:"project"`
	Environment string                   `json:"environment"`
	Service     string                   `json:"service"`
	Revision    string                   `json:"revision"`
	Image       string                   `json:"image,omitempty"`
	Status      takoapi.DeploymentStatus `json:"status"`
	StartedAt   time.Time                `json:"startedAt"`
	Duration    float64                  `json:"durationSeconds"`
}

// Promote switches proxy routes to a warmed blue-green revision, prunes stale
// revisions, and persists the promoted state. Promotion has no interactive
// confirmation, so it runs as a single method rather than a plan/apply session.
func (e *Engine) Promote(ctx context.Context, req PromoteRequest) (*PromoteResult, error) {
	if req.Config == nil {
		return nil, invalidRequestf("promote request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("promote request requires an environment")
	}
	serviceName := strings.TrimSpace(req.ServiceName)
	if serviceName == "" {
		return nil, invalidRequestf("promote request requires a service name")
	}
	cfg := req.Config
	if err := RequireTakodRuntime(cfg); err != nil {
		return nil, err
	}

	envName := req.Environment
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get services: %w", err)
	}
	service, ok := services[serviceName]
	if !ok {
		return nil, invalidRequestf("service %s not found in environment %s", serviceName, envName)
	}
	if service.Deploy.Strategy != config.DeployStrategyBlueGreen {
		return nil, invalidRequestf("service %s does not use deploy.strategy=blue_green", serviceName)
	}
	if service.Deploy.Promotion != config.DeployPromotionManual {
		return nil, invalidRequestf("service %s does not use deploy.promotion=manual", serviceName)
	}

	serverNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(serverNames) == 0 {
		return nil, invalidRequestf("no servers configured for environment %s", envName)
	}

	// Register sensitive values with the redactor before emitting anything
	// that could contain them.
	for _, svc := range services {
		for _, value := range svc.Env {
			e.RegisterSecret(value)
		}
	}
	for _, server := range cfg.Servers {
		e.RegisterSecret(server.Password)
	}

	stateLock := localstate.NewStateLock(".tako")
	lockInfo, err := stateLock.Acquire("promote")
	if err != nil {
		holder := ""
		if info, infoErr := stateLock.GetLockInfo(); infoErr == nil && info != nil {
			holder = info.Who
		}
		return nil, &LockedError{Operation: "promote", Holder: holder, Err: fmt.Errorf("cannot promote: %w", err)}
	}
	defer stateLock.Release(lockInfo)

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	leaseSet, err := AcquireRemoteOperationLeasesContext(ctx, sshPool, cfg, envName, serverNames, "promote")
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
	e.debug(events.TypeLogLine, events.PhaseDeploy, fmt.Sprintf("→ Acquired remote promote leases: %s\n", leaseSet.Summary()))

	sourceServerName := serverNames[0]
	sourceServer, ok := cfg.Servers[sourceServerName]
	if !ok {
		return nil, invalidRequestf("server %s not found in configuration", sourceServerName)
	}
	sourceClient, err := sshPool.GetOrCreateWithAuth(sourceServer.Host, sourceServer.Port, sourceServer.User, sourceServer.SSHKey, sourceServer.Password)
	if err != nil {
		return nil, &ConnectivityError{Server: sourceServerName, Err: fmt.Errorf("failed to connect to node %s: %w", sourceServerName, err)}
	}

	deploy := deployer.NewDeployerWithPool(sourceClient, cfg, envName, sshPool, req.Verbose)
	deploy.SetBaseContext(ctx)
	deploy.SetCLIVersion(e.cliVersion)
	if err := deploy.SetTargetServers(serverNames); err != nil {
		return nil, err
	}
	if err := deploy.SetupTakodRuntime(); err != nil {
		return nil, fmt.Errorf("failed to setup takod runtime: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	actualState, err := reconcile.GatherActualStateFromServers(sshPool, cfg, envName, serverNames, nil)
	if err != nil {
		return nil, ActualStateError(err)
	}
	actual := actualState[serviceName]
	targetRevision, err := SelectPromotionRevision(actual, req.Revision)
	if err != nil {
		return nil, invalidRequestf("cannot promote %s: %w", serviceName, err)
	}
	targetImage, err := promotionTargetImage(cfg, envName, serviceName, service, actual, targetRevision)
	if err != nil {
		return nil, fmt.Errorf("cannot promote %s: %w", serviceName, err)
	}

	activeRevisions := deployplan.ProxyActiveRevisions(cfg, envName, services, nil, nil, actualState)
	if activeRevisions == nil {
		activeRevisions = make(map[string]string)
	}
	activeRevisions[serviceName] = targetRevision

	e.emit(events.Event{
		Type:    events.TypeDeployStarted,
		Phase:   events.PhaseDeploy,
		Level:   events.LevelInfo,
		Service: serviceName,
		Message: fmt.Sprintf("\n=== Promoting %s ===\n\n", serviceName),
	})
	e.info(events.TypeLogLine, events.PhaseDeploy, fmt.Sprintf("Target revision: %s\n", targetRevision))

	startTime := time.Now()
	result := &PromoteResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindPromoteResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Service:     serviceName,
		Revision:    targetRevision,
		Image:       targetImage,
		StartedAt:   startTime,
	}

	if err := deploy.ActivateTakodServiceRevision(serviceName, &service, targetImage); err != nil {
		return nil, fmt.Errorf("failed to activate warmed revision before proxy promotion: %w", err)
	}
	if err := ReconcileProxy(deploy, services, activeRevisions); err != nil {
		return nil, fmt.Errorf("failed to promote proxy route: %w", err)
	}
	if err := e.PruneRevisionsAfterGrace(deploy, map[string]config.ServiceConfig{serviceName: service}, map[string]string{serviceName: targetRevision}, GraceSleep); err != nil {
		return nil, fmt.Errorf("proxy promoted but failed to prune stale revisions: %w", err)
	}

	postNodeActualState, err := reconcile.GatherActualStateByServer(sshPool, cfg, envName, serverNames)
	if err != nil {
		return nil, fmt.Errorf("promotion succeeded but failed to gather post-promotion actual state: %w", err)
	}
	postActualState := reconcile.AggregateActualStateByServer(postNodeActualState)
	runtimeImageRefs := deployplan.MergeRuntimeImageRefs(cfg, envName, services, nil, postActualState)
	if err := PersistTakodRuntimeState(
		sshPool,
		cfg,
		envName,
		serverNames,
		"promote",
		services,
		runtimeImageRefs,
		postActualState,
		postNodeActualState,
		optionalPromoteGitInfo(),
		"promote.succeeded",
		fmt.Sprintf("promoted %s to revision %s", serviceName, targetRevision),
		map[string]string{
			"service":  serviceName,
			"revision": targetRevision,
		},
		req.Verbose,
	); err != nil {
		return nil, fmt.Errorf("promotion succeeded but failed to persist takod state: %w", err)
	}
	e.debug(events.TypeStatePersisted, events.PhaseState, "")

	stateManager := remotestate.NewStateManagerWithSocket(sourceClient, cfg.Project.Name, envName, sourceServer.Host, TakodSocketFromConfig(cfg))
	promoteDeployment := buildPromoteDeployment(cfg, envName, sourceServer.Host, serviceName, service, postActualState[serviceName], startTime, time.Since(startTime), e.cliVersion, e.cliCommit)
	if err := stateManager.SaveDeploymentContext(ctx, promoteDeployment); err != nil {
		return nil, fmt.Errorf("promotion succeeded but failed to save deployment history: %w", err)
	}
	if cfg.IsMultiServer() {
		replicator := remotestate.NewStateReplicator(sshPool, cfg, envName, cfg.Project.Name, req.Verbose)
		history, err := stateManager.LoadHistoryContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("promotion succeeded but failed to load remote deployment history for replication: %w", err)
		}
		if err := replicator.ReplicateDeploymentContext(ctx, promoteDeployment, history); err != nil {
			return nil, fmt.Errorf("promotion succeeded but failed to replicate remote deployment history: %w", err)
		}
		e.debug(events.TypeStateReplicated, events.PhaseState, "")
	}
	e.saveLocalPromoteState(cfg, envName, serverNames, serviceName, targetRevision, promoteDeployment)

	displayRevision := targetRevision
	if req.ShortRevision != nil {
		displayRevision = req.ShortRevision(targetRevision)
	}
	e.info(events.TypeDeploySucceeded, events.PhaseDeploy, fmt.Sprintf("\n✓ Promoted %s to revision %s\n", serviceName, displayRevision))

	result.Status = takoapi.DeploymentStatus(remotestate.StatusSuccess)
	result.Duration = time.Since(startTime).Seconds()
	return result, nil
}

func promotionTargetImage(cfg *config.Config, envName string, serviceName string, service config.ServiceConfig, actual *reconcile.ActualService, targetRevision string) (string, error) {
	if actual == nil {
		return "", fmt.Errorf("service is not deployed")
	}
	image := strings.TrimSpace(actual.RevisionImages[targetRevision])
	if image == "" {
		image = strings.TrimSpace(actual.Image)
	}
	if image == "" {
		return "", fmt.Errorf("warmed revision does not report an image")
	}
	expected := deployer.ServiceRevisionID(cfg.Project.Name, envName, serviceName, image, service)
	if expected != targetRevision {
		return "", fmt.Errorf("warmed revision %s image %s resolves to revision %s", targetRevision, image, expected)
	}
	return image, nil
}

// SelectPromotionRevision resolves which warmed revision a promotion targets:
// the requested revision (full value or unique prefix) when given, otherwise
// the single warmed candidate.
func SelectPromotionRevision(actual *reconcile.ActualService, requested string) (string, error) {
	if actual == nil {
		return "", fmt.Errorf("service is not deployed")
	}
	revisions := promotionCandidateRevisions(actual)
	requested = strings.TrimSpace(requested)
	if requested != "" {
		return matchRequestedPromotionRevision(revisions, requested)
	}
	if len(revisions) == 0 {
		return "", fmt.Errorf("no warmed revision found")
	}
	if len(revisions) > 1 {
		return "", fmt.Errorf("multiple warmed revisions found (%s); rerun with --revision", strings.Join(revisions, ", "))
	}
	return revisions[0], nil
}

func promotionCandidateRevisions(actual *reconcile.ActualService) []string {
	if actual == nil {
		return nil
	}
	seen := make(map[string]bool)
	var revisions []string
	add := func(revision string) {
		revision = strings.TrimSpace(revision)
		if revision == "" || revision == actual.CurrentRevision || seen[revision] {
			return
		}
		seen[revision] = true
		revisions = append(revisions, revision)
	}
	for _, revision := range actual.WarmingRevisions {
		add(revision)
	}
	add(actual.PreviousRevision)
	sort.Strings(revisions)
	return revisions
}

func matchRequestedPromotionRevision(revisions []string, requested string) (string, error) {
	var matches []string
	for _, revision := range revisions {
		if revision == requested || strings.HasPrefix(revision, requested) {
			matches = append(matches, revision)
		}
	}
	switch len(matches) {
	case 0:
		if len(revisions) == 0 {
			return "", fmt.Errorf("no warmed revision found")
		}
		return "", fmt.Errorf("revision %q is not warmed; available warmed revisions: %s", requested, strings.Join(revisions, ", "))
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("revision prefix %q is ambiguous: %s", requested, strings.Join(matches, ", "))
	}
}

func buildPromoteDeployment(
	cfg *config.Config,
	envName string,
	host string,
	serviceName string,
	service config.ServiceConfig,
	actual *reconcile.ActualService,
	start time.Time,
	duration time.Duration,
	cliVersion string,
	cliCommit string,
) *remotestate.DeploymentState {
	serviceState := remotestate.ServiceState{
		Name:     serviceName,
		Port:     service.Port,
		Replicas: service.Replicas,
		Env:      RedactedEnvKeys(service.Env),
	}
	if actual != nil {
		serviceState.Image = actual.Image
		serviceState.Replicas = actual.Replicas
		if len(actual.Containers) > 0 {
			serviceState.ContainerID = actual.Containers[0]
		}
	}
	return &remotestate.DeploymentState{
		Timestamp:   start,
		ProjectName: cfg.Project.Name,
		Environment: envName,
		Version:     cfg.Project.Version,
		Status:      remotestate.StatusSuccess,
		Services:    map[string]remotestate.ServiceState{serviceName: serviceState},
		User:        remotestate.GetCurrentUser(),
		Host:        host,
		Duration:    duration,
		Message:     fmt.Sprintf("promoted %s", serviceName),
		CLIVersion:  cliVersion,
		CLICommit:   cliCommit,
	}
}

func optionalPromoteGitInfo() takodstate.GitInfo {
	client := git.NewClient(".")
	if !client.IsRepository() {
		return takodstate.GitInfo{}
	}
	commit, err := client.GetCommitInfo("")
	if err != nil {
		return takodstate.GitInfo{}
	}
	return GitInfoFromCommit(commit)
}

func (e *Engine) saveLocalPromoteState(cfg *config.Config, envName string, serverNames []string, serviceName string, revision string, deployment *remotestate.DeploymentState) {
	localStateMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		e.debug(events.TypeWarning, events.PhaseState, fmt.Sprintf("Warning: failed to initialize local state for promotion: %v\n", err))
		return
	}
	localDeployment := &localstate.DeploymentState{
		DeploymentID:    fmt.Sprintf("promote-%s", time.Now().Format("20060102-150405")),
		Timestamp:       deployment.Timestamp,
		Environment:     envName,
		Mode:            cfg.GetRuntimeMode(),
		Servers:         append([]string(nil), serverNames...),
		Status:          string(remotestate.StatusSuccess),
		DurationSeconds: int(deployment.Duration.Seconds()),
		TriggeredBy:     remotestate.GetCurrentUser(),
		Notes:           fmt.Sprintf("Promoted %s to revision %s", serviceName, revision),
	}
	if err := localStateMgr.SaveDeployment(localDeployment); err != nil {
		e.debug(events.TypeWarning, events.PhaseState, fmt.Sprintf("Warning: failed to save local promote state: %v\n", err))
	}
}
