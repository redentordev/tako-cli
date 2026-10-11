package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/dependency"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/notification"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

// KindRollbackPlan identifies a serialized project rollback plan document.
const KindRollbackPlan = "RollbackPlan"

// Release handling recorded in RollbackPlan.ReleaseCommands.
const (
	RollbackReleaseSkip = "skip"
	RollbackReleaseRun  = "run"
)

// RollbackServiceOutcome is one service's planned or applied project
// rollback outcome.
type RollbackServiceOutcome struct {
	Name string `json:"name"`
	// Action is rolled_back, unchanged, skipped, failed or not_attempted.
	Action    string `json:"action"`
	FromImage string `json:"fromImage,omitempty"`
	ToImage   string `json:"toImage,omitempty"`
	Replicas  int    `json:"replicas,omitempty"`
	// SourceDeploymentID is the history record the target state came from.
	SourceDeploymentID string `json:"sourceDeploymentId,omitempty"`
	// ImageSource is retained, rebuild or pull.
	ImageSource string `json:"imageSource,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Error       string `json:"error,omitempty"`
	// Recovery reports how a failed rollout was recovered.
	Recovery *RecoveryOutcome `json:"recovery,omitempty"`
}

// RollbackPlanTarget identifies the deployment a project rollback restores.
type RollbackPlanTarget struct {
	ID        string    `json:"id"`
	DisplayID string    `json:"displayId"`
	Timestamp time.Time `json:"timestamp"`
	Commit    string    `json:"commit,omitempty"`
	Version   string    `json:"version,omitempty"`
	Message   string    `json:"message,omitempty"`
}

// RollbackPlan is the serializable plan of a project rollback: the target,
// every service's planned action and image source, and warnings. It feeds
// the confirmation screen, --plan-only output and --plan verification.
type RollbackPlan struct {
	APIVersion  string                   `json:"apiVersion"`
	Kind        string                   `json:"kind"`
	Project     string                   `json:"project"`
	Environment string                   `json:"environment"`
	Servers     []string                 `json:"servers,omitempty"`
	Target      RollbackPlanTarget       `json:"target"`
	Services    []RollbackServiceOutcome `json:"services"`
	Warnings    []string                 `json:"warnings,omitempty"`
	// ReleaseCommands is "skip" (default) or "run" (--run-release).
	ReleaseCommands   string `json:"releaseCommands"`
	IncludePersistent bool   `json:"includePersistent,omitempty"`
	// Hash fingerprints the mutation-determining fields; see PlanHash.
	Hash string `json:"hash"`
}

// PlanHash fingerprints the fields that determine the mutation: environment,
// target, each service's action, image, replicas, image source and source
// record, the release handling and includePersistent. Display-only fields
// (timestamps, fromImage, reasons, warnings) are excluded so re-planning
// unchanged state yields the same hash.
func (p *RollbackPlan) PlanHash() string {
	if p == nil {
		return ""
	}
	type hashedService struct {
		Name               string `json:"name"`
		Action             string `json:"action"`
		ToImage            string `json:"toImage"`
		Replicas           int    `json:"replicas"`
		ImageSource        string `json:"imageSource"`
		SourceDeploymentID string `json:"sourceDeploymentId"`
	}
	services := make([]hashedService, 0, len(p.Services))
	for _, service := range p.Services {
		services = append(services, hashedService{
			Name:               service.Name,
			Action:             service.Action,
			ToImage:            service.ToImage,
			Replicas:           service.Replicas,
			ImageSource:        service.ImageSource,
			SourceDeploymentID: service.SourceDeploymentID,
		})
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	payload, err := json.Marshal(struct {
		Kind              string          `json:"kind"`
		Environment       string          `json:"environment"`
		Target            string          `json:"target"`
		Services          []hashedService `json:"services"`
		ReleaseCommands   string          `json:"releaseCommands"`
		IncludePersistent bool            `json:"includePersistent"`
	}{KindRollbackPlan, p.Environment, p.Target.ID, services, p.ReleaseCommands, p.IncludePersistent})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// RolledBackServices lists the services the plan rolls back, sorted.
func (p *RollbackPlan) RolledBackServices() []string {
	var names []string
	for _, service := range p.Services {
		if service.Action == RollbackActionRolledBack {
			names = append(names, service.Name)
		}
	}
	return names
}

// RollbackUnavailableError reports a project rollback refused by the image
// preflight; Result is the failed result document (every planned service
// not_attempted). It classifies as a failed operation (exit 1).
type RollbackUnavailableError struct {
	Result *RollbackResult
	Err    error
}

func (e *RollbackUnavailableError) Error() string { return e.Err.Error() }
func (e *RollbackUnavailableError) Unwrap() error { return e.Err }

// RollbackSession holds the lock, leases and plan of a project rollback
// between PlanRollback and Apply, so the reviewed plan cannot go stale
// before confirmation. Close it exactly once.
type RollbackSession struct {
	engine *Engine
	req    RollbackRequest

	cfg             *config.Config
	envName         string
	services        map[string]config.ServiceConfig
	envServers      []string
	mutationServers []string
	stateHost       string
	target          *remotestate.DeploymentState
	history         *remotestate.DeploymentHistory
	records         map[string]*remotestate.DeploymentState
	planDoc         RollbackPlan
	targets         map[string]remotestate.ServiceState
	priorDesired    *takodstate.DesiredRevision

	stateLock    *localstate.StateLock
	lockInfo     *localstate.LockInfo
	sshPool      *ssh.Pool
	runtime      *nodeclient.Factory
	leases       *RemoteLeaseSet
	deployer     *deployer.Deployer
	stateManager *remotestate.StateManager

	restoreEvents []func()
	closed        bool
	applied       bool
}

// Plan returns the reviewed plan document.
func (s *RollbackSession) Plan() RollbackPlan {
	return s.planDoc
}

// ProjectName, Environment and ServerNames feed confirmation documents.
func (s *RollbackSession) ProjectName() string { return s.cfg.Project.Name }
func (s *RollbackSession) Environment() string { return s.envName }
func (s *RollbackSession) ServerNames() []string {
	return append([]string(nil), s.mutationServers...)
}

// Close releases the leases, connections and local lock. Idempotent.
func (s *RollbackSession) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	s.release()
}

func (s *RollbackSession) release() {
	for index := len(s.restoreEvents) - 1; index >= 0; index-- {
		s.restoreEvents[index]()
	}
	s.restoreEvents = nil
	if s.leases != nil {
		s.leases.Release()
	}
	if s.runtime != nil {
		s.runtime.CloseIdleConnections()
	}
	if s.sshPool != nil {
		s.sshPool.CloseAll()
	}
	if s.stateLock != nil && s.lockInfo != nil {
		_ = s.stateLock.Release(s.lockInfo)
	}
}

// PlanRollback plans a project-wide rollback (RollbackRequest.Service
// empty). It takes the local lock and remote leases, loads the freshest mesh
// history, resolves the target, reconstructs each service's target state,
// detects unchanged services, and proves every needed image is available
// before returning. Nothing is mutated.
func (e *Engine) PlanRollback(ctx context.Context, req RollbackRequest) (_ *RollbackSession, err error) {
	if req.Config == nil {
		return nil, invalidRequestf("rollback request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("rollback request requires an environment")
	}
	if req.Scope() != RollbackScopeProject {
		return nil, invalidRequestf("project rollback plans take no service; use Rollback for --service")
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

	session := &RollbackSession{engine: e, req: req, cfg: cfg, envName: envName}
	defer func() {
		if err != nil {
			session.Close()
		}
	}()

	session.stateLock = localstate.NewStateLock(".tako")
	lockInfo, err := session.stateLock.Acquire("rollback")
	if err != nil {
		holder := ""
		if info, infoErr := session.stateLock.GetLockInfo(); infoErr == nil && info != nil {
			holder = info.Who
		}
		return nil, &LockedError{Operation: "rollback", Holder: holder, Err: fmt.Errorf("cannot rollback: %w", err)}
	}
	session.lockInfo = lockInfo

	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get services: %w", err)
	}
	session.services = services
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(envServers) == 0 {
		return nil, invalidRequestf("no servers configured for environment %s", envName)
	}
	session.envServers = envServers
	mutationServers, err := config.ResolveSchedulableEnvironmentTargets(cfg.Servers, envServers, envName)
	if err != nil {
		return nil, err
	}
	session.mutationServers = mutationServers

	// Register sensitive values with the redactor before emitting anything
	// that could contain them.
	e.RegisterPublicConfigNames(cfg, services)
	e.RegisterServiceEnvValues(services)
	for _, server := range cfg.Servers {
		e.RegisterSecret(server.Password)
	}
	e.RegisterACMEDNSSecrets(cfg)
	e.RegisterRegistrySecrets(cfg)
	e.registerServiceSecretValuesOrWarn(cfg, envName, services, events.PhaseDeploy)
	session.restoreEvents = append(session.restoreEvents, e.stream.SetContextData(map[string]any{"operation": "rollback"}))

	session.sshPool = ssh.NewPool()
	leaseSet, err := AcquireRemoteOperationLeasesContext(ctx, session.sshPool, cfg, envName, mutationServers, "rollback")
	if err != nil {
		return nil, err
	}
	session.leases = leaseSet
	leaseCtx, cancelLeaseContext := leaseSet.BindContext(ctx)
	defer cancelLeaseContext()
	ctx = leaseCtx
	leaseSet.SetWarnFunc(func(message string) {
		e.debug(events.TypeWarning, events.PhaseDeploy, message)
	})
	e.debug(events.TypeLogLine, events.PhasePlan, fmt.Sprintf("→ Acquired remote rollback leases: %s\n", leaseSet.Summary()))

	sourceName, history, err := req.HistorySource()
	if err != nil {
		return nil, err
	}
	if _, exists := cfg.Servers[sourceName]; !exists {
		return nil, invalidRequestf("history source server %s not found in configuration", sourceName)
	}
	session.history = history
	stateSourceName, err := AuthoritativeStateServer(cfg, mutationServers)
	if err != nil {
		return nil, err
	}
	server, exists := cfg.Servers[stateSourceName]
	if !exists {
		return nil, invalidRequestf("server %s not found in configuration", stateSourceName)
	}
	session.stateHost = server.Host

	target, err := SelectProjectRollbackTarget(history, req.DeploymentID, req.ListDeployments)
	if err != nil {
		return nil, err
	}
	session.target = target
	session.restoreEvents = append(session.restoreEvents, e.stream.SetContextData(map[string]any{"targetDeploymentId": target.ID}))
	session.records = map[string]*remotestate.DeploymentState{}
	if history != nil {
		for _, record := range history.Deployments {
			if record != nil {
				session.records[record.ID] = record
			}
		}
	}

	plans := ReconstructServiceTargets(history, target, services, req.IncludePersistent)
	if err := unresolvableRollbackError(plans); err != nil {
		return nil, err
	}

	runtimeFactory, err := nodeclient.NewFactory(cfg, session.sshPool, TakodSocketFromConfig(cfg))
	if err != nil {
		return nil, err
	}
	session.runtime = runtimeFactory
	client, _, err := runtimeFactory.Client(ctx, stateSourceName)
	if err != nil {
		return nil, &ConnectivityError{Server: stateSourceName, Err: fmt.Errorf("failed to connect to node %s: %w", stateSourceName, err)}
	}
	session.stateManager = remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, TakodSocketFromConfig(cfg))

	actualState, err := reconcile.GatherActualStateFromServers(session.sshPool, cfg, envName, envServers, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to gather actual state for rollback planning: %w", err)
	}
	outcomes := planRollbackOutcomes(plans, actualState, history)

	deploy := deployer.NewDeployerWithPool(client, cfg, envName, session.sshPool, req.Verbose)
	priorDesired, priorAssignments, err := LoadPriorPlacementState(client, cfg, envName)
	if err != nil {
		return nil, err
	}
	if err := ValidatePriorDesiredServices(priorDesired, services); err != nil {
		return nil, err
	}
	session.priorDesired = priorDesired
	deploy.SetPriorAssignments(priorAssignments)
	session.targets = rollbackTargetStates(plans, outcomes)
	placementServices := rollbackPlacementServices(services, session.targets)
	if err := deploy.ResolveAllAssignments(placementServices); err != nil {
		return nil, err
	}
	deploy.SetRuntimeFactory(runtimeFactory)
	deploy.SetBaseContext(ctx)
	deploy.SetEventSink(e.stream)
	deploy.SetCLIVersion(e.cliVersion)
	if output := e.buildOutputWriter(); output != nil {
		deploy.SetOutput(output)
	}
	if err := deploy.SetTargetServers(mutationServers); err != nil {
		return nil, err
	}
	session.deployer = deploy

	rolledConfigs := make(map[string]config.ServiceConfig, len(session.targets))
	for name := range session.targets {
		rolledConfigs[name] = placementServices[name]
	}
	if len(rolledConfigs) > 0 {
		// Capability admission only: runtime setup applies node state and
		// waits for Apply.
		if err := preflightRollbackTakodRuntime(deploy, services, rolledConfigs); err != nil {
			return nil, err
		}
		if err := deploy.PreflightAssignmentMutations(rolledConfigs); err != nil {
			return nil, err
		}
	}

	// Image preflight: every rolled-back service needs a source before any
	// mutation.
	unavailable, err := preflightRollbackImages(outcomes, services, session.targets, session.records, rollbackImageProbe{
		LocateRetained: func(name string, state remotestate.ServiceState) (bool, error) {
			service := placementServices[name]
			return deploy.LocateRetainedServiceImage(name, &service, state.Image)
		},
		CommitAvailable: rollbackCommitAvailable,
	})
	if err != nil {
		return nil, err
	}

	session.planDoc = newRollbackPlanDocument(cfg, envName, mutationServers, target, outcomes, services, req)
	if len(unavailable) > 0 {
		preflightErr := fmt.Errorf("rollback refused before any change: no image source for %s", strings.Join(unavailable, "; "))
		return nil, &RollbackUnavailableError{Result: session.unavailableResult(preflightErr), Err: preflightErr}
	}
	for _, service := range session.planDoc.Services {
		if service.Action == RollbackActionRolledBack && services[service.Name].Persistent {
			e.warn(events.PhaseDeploy, fmt.Sprintf("Warning: persistent service %s is rolled back to %s; volume data is not rolled back and may be incompatible with the older image\n", service.Name, service.ToImage))
		}
	}
	e.emit(events.Event{
		Type:    events.TypePlanComputed,
		Phase:   events.PhasePlan,
		Level:   events.LevelDebug,
		Message: fmt.Sprintf("Planned rollback to deployment %s (%d service(s) to roll back)\n", target.ID, len(session.planDoc.RolledBackServices())),
		Data:    map[string]any{"hash": session.planDoc.Hash},
	})
	return session, nil
}

// preflightRollbackTakodRuntime runs the capability checks of
// preflightAndSetupTakodRuntime without its runtime setup mutation.
func preflightRollbackTakodRuntime(runtime takodRuntimeSetup, routeServices, mutationServices map[string]config.ServiceConfig) error {
	if err := runtime.PreflightTakodProxyCapabilities(routeServices); err != nil {
		return err
	}
	if err := runtime.PreflightGracefulStopCapabilities(); err != nil {
		return err
	}
	return runtime.PreflightServiceMutationCapabilities(mutationServices)
}

// planRollbackOutcomes turns reconstructed targets into plan outcomes,
// marking services that already run their target state unchanged.
func planRollbackOutcomes(plans []RollbackServicePlan, actualState map[string]*reconcile.ActualService, history *remotestate.DeploymentHistory) []RollbackServiceOutcome {
	outcomes := make([]RollbackServiceOutcome, 0, len(plans))
	for _, plan := range plans {
		outcome := RollbackServiceOutcome{Name: plan.Name, Action: plan.Action, Reason: plan.Reason, SourceDeploymentID: plan.SourceDeploymentID}
		if actual := actualState[plan.Name]; actual != nil {
			outcome.FromImage = actual.Image
		}
		if plan.State != nil {
			outcome.ToImage = plan.State.Image
			outcome.Replicas = plan.State.Replicas
			if RollbackServiceUnchanged(*plan.State, actualState[plan.Name], CurrentServiceRecord(history, plan.Name)) {
				outcome.Action = RollbackActionUnchanged
				outcome.Reason = "already running the target state"
			}
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

func rollbackTargetStates(plans []RollbackServicePlan, outcomes []RollbackServiceOutcome) map[string]remotestate.ServiceState {
	targets := map[string]remotestate.ServiceState{}
	for index, plan := range plans {
		if outcomes[index].Action == RollbackActionRolledBack && plan.State != nil {
			targets[plan.Name] = *plan.State
		}
	}
	return targets
}

// rollbackPlacementServices sets each rolled-back service's replicas to its
// target state for placement.
func rollbackPlacementServices(services map[string]config.ServiceConfig, targets map[string]remotestate.ServiceState) map[string]config.ServiceConfig {
	placement := CloneServiceMap(services)
	for name, state := range targets {
		service := placement[name]
		service.Replicas = state.Replicas
		placement[name] = service
	}
	return placement
}

// rollbackImageProbe abstracts node and git access for the image preflight.
type rollbackImageProbe struct {
	LocateRetained  func(name string, state remotestate.ServiceState) (bool, error)
	CommitAvailable func(commit string) bool
}

// preflightRollbackImages chooses each rolled-back service's image source:
// retained (exact image on a node), rebuild (build-backed, commit in the
// local repo) or pull (registry image). Shared-build targets accept only a
// retained image. It returns the services with no source; capability and
// node errors abort planning.
func preflightRollbackImages(outcomes []RollbackServiceOutcome, services map[string]config.ServiceConfig, targets map[string]remotestate.ServiceState, records map[string]*remotestate.DeploymentState, probe rollbackImageProbe) ([]string, error) {
	var unavailable []string
	for index := range outcomes {
		outcome := &outcomes[index]
		if outcome.Action != RollbackActionRolledBack {
			continue
		}
		state := targets[outcome.Name]
		service := services[outcome.Name]
		retained, err := probe.LocateRetained(outcome.Name, state)
		if err != nil {
			var capability *takodclient.CapabilityRequiredError
			if errors.As(err, &capability) {
				return nil, err
			}
			return nil, fmt.Errorf("image preflight for %s (%s) failed: %w", outcome.Name, state.Image, err)
		}
		switch {
		case retained:
			outcome.ImageSource = RollbackImageRetained
		case state.SharedBuildHash != "":
			unavailable = append(unavailable, fmt.Sprintf("%s (%s: shared-build image is not retained on any node)", outcome.Name, state.Image))
		case strings.TrimSpace(service.Build) != "":
			commit := RollbackTargetCommit(records[outcome.SourceDeploymentID])
			if commit != "" && probe.CommitAvailable(commit) {
				outcome.ImageSource = RollbackImageRebuild
				continue
			}
			reason := "image is not retained and the deployment recorded no git commit"
			if commit != "" {
				reason = fmt.Sprintf("image is not retained and commit %s is not in the local repository", commit)
			}
			unavailable = append(unavailable, fmt.Sprintf("%s (%s: %s)", outcome.Name, state.Image, reason))
		case strings.TrimSpace(service.Image) != "":
			outcome.ImageSource = RollbackImagePull
		default:
			unavailable = append(unavailable, fmt.Sprintf("%s (%s: image is not retained and cannot be rebuilt or pulled)", outcome.Name, state.Image))
		}
	}
	return unavailable, nil
}

// rollbackJobWarning names the kind: job services a project rollback leaves
// on the newer image and schedule.
func rollbackJobWarning(services map[string]config.ServiceConfig) string {
	var jobs []string
	for _, name := range sortedServiceNames(services) {
		service := services[name]
		if service.IsJob() {
			jobs = append(jobs, name)
		}
	}
	switch len(jobs) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("job %s is not rolled back and keeps running the newer image and schedule", jobs[0])
	default:
		return fmt.Sprintf("jobs %s are not rolled back and keep running the newer image and schedule", strings.Join(jobs, ", "))
	}
}

func newRollbackPlanDocument(cfg *config.Config, envName string, servers []string, target *remotestate.DeploymentState, outcomes []RollbackServiceOutcome, services map[string]config.ServiceConfig, req RollbackRequest) RollbackPlan {
	plan := RollbackPlan{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindRollbackPlan,
		Project:     cfg.Project.Name,
		Environment: envName,
		Servers:     append([]string(nil), servers...),
		Target: RollbackPlanTarget{
			ID:        target.ID,
			DisplayID: remotestate.FormatDeploymentID(target.ID),
			Timestamp: target.Timestamp,
			Commit:    firstNonEmpty(target.GitCommitShort, target.GitCommit),
			Version:   target.Version,
			Message:   firstNonEmpty(target.GitCommitMsg, target.Message),
		},
		Services:          append([]RollbackServiceOutcome(nil), outcomes...),
		ReleaseCommands:   RollbackReleaseSkip,
		IncludePersistent: req.IncludePersistent,
	}
	if req.RunRelease {
		plan.ReleaseCommands = RollbackReleaseRun
	}
	if warning := rollbackJobWarning(services); warning != "" {
		plan.Warnings = append(plan.Warnings, warning)
	}
	for _, outcome := range outcomes {
		if outcome.Action == RollbackActionRolledBack && services[outcome.Name].Persistent {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("persistent service %s is rolled back; volume data is not rolled back and may be incompatible with the older image", outcome.Name))
		}
	}
	plan.Hash = plan.PlanHash()
	return plan
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// unavailableResult is the failed result of a rollback refused by the image
// preflight: every planned service is not_attempted.
func (s *RollbackSession) unavailableResult(err error) *RollbackResult {
	result := s.newResult(time.Now())
	result.Status = takoapi.StatusFailed
	result.Error = s.engine.RedactSensitive(err.Error())
	for index := range result.Services {
		if result.Services[index].Action == RollbackActionRolledBack {
			result.Services[index].Action = RollbackActionNotAttempted
		}
	}
	s.attachSummary(context.Background(), result, "", nil)
	return result
}

func (s *RollbackSession) newResult(startedAt time.Time) *RollbackResult {
	return &RollbackResult{
		APIVersion:   takoapi.APIVersionCurrent,
		Kind:         KindRollbackResult,
		Project:      s.cfg.Project.Name,
		Environment:  s.envName,
		Scope:        RollbackScopeProject,
		DeploymentID: s.target.ID,
		Version:      s.target.Version,
		Services:     append([]RollbackServiceOutcome(nil), s.planDoc.Services...),
		StartedAt:    startedAt,
	}
}

// rollbackStep rolls one service back; it returns the recovery outcome of a
// failed rollout when one is known.
type rollbackStep func(ctx context.Context, outcome RollbackServiceOutcome) (*RecoveryOutcome, error)

// runRollbackSequence rolls back the planned services in order, one at a
// time, stopping at the first failure and marking the rest not_attempted.
// It returns the services rolled back and the failure.
func (e *Engine) runRollbackSequence(ctx context.Context, order []string, outcomes []RollbackServiceOutcome, step rollbackStep) ([]string, error) {
	index := make(map[string]int, len(outcomes))
	for i, outcome := range outcomes {
		index[outcome.Name] = i
	}
	var rolled []string
	var failure error
	for _, name := range order {
		i, ok := index[name]
		if !ok || outcomes[i].Action != RollbackActionRolledBack {
			continue
		}
		if failure != nil {
			outcomes[i].Action = RollbackActionNotAttempted
			continue
		}
		if err := ctx.Err(); err != nil {
			failure = err
			outcomes[i].Action = RollbackActionNotAttempted
			continue
		}
		e.emit(events.Event{
			Type:    events.TypeDeployServiceStarted,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: name,
			Message: fmt.Sprintf("→ Rolling back service: %s to %s\n", name, outcomes[i].ToImage),
			Data:    map[string]any{"image": outcomes[i].ToImage, "imageSource": outcomes[i].ImageSource},
		})
		recovery, err := step(ctx, outcomes[i])
		if err != nil {
			e.emit(events.Event{
				Type:    events.TypeDeployServiceFailed,
				Phase:   events.PhaseDeploy,
				Level:   events.LevelError,
				Service: name,
				Message: fmt.Sprintf("  ✗ Rollback of %s failed: %v\n", name, err),
				Data:    map[string]any{"image": outcomes[i].ToImage},
			})
			e.emitRecoveryEvents(name, recovery)
			outcomes[i].Action = RollbackActionFailed
			outcomes[i].Error = e.RedactSensitive(err.Error())
			outcomes[i].Recovery = recovery
			failure = fmt.Errorf("rollback of %s failed: %w", name, err)
			continue
		}
		e.emit(events.Event{
			Type:    events.TypeDeployServiceReconciled,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: name,
			Message: fmt.Sprintf("  ✓ Service %s rolled back by takod\n", name),
			Data:    map[string]any{"image": outcomes[i].ToImage},
		})
		rolled = append(rolled, name)
	}
	// Services missing from the dependency order never ran.
	if failure != nil {
		for i := range outcomes {
			if outcomes[i].Action == RollbackActionRolledBack && !containsString(rolled, outcomes[i].Name) {
				outcomes[i].Action = RollbackActionNotAttempted
			}
		}
	}
	return rolled, failure
}

// emitRollbackSkips reports every skipped or unchanged service.
func (e *Engine) emitRollbackSkips(outcomes []RollbackServiceOutcome) {
	for _, outcome := range outcomes {
		if outcome.Action != RollbackActionSkipped && outcome.Action != RollbackActionUnchanged {
			continue
		}
		e.emit(events.Event{
			Type:    events.TypeDeployServiceSkipped,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: outcome.Name,
			Message: fmt.Sprintf("  - %s %s (%s)\n", outcome.Name, outcome.Action, outcome.Reason),
			Data:    map[string]any{"action": outcome.Action, "reason": outcome.Reason},
		})
	}
}

// rollbackOrder is the dependency order deploy uses.
func rollbackOrder(services map[string]config.ServiceConfig, verbose bool) ([]string, error) {
	resolver := dependency.NewResolver(services, verbose)
	resolver.MergeDependencies(resolver.InferDependencies())
	order, err := resolver.ResolveOrder()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve service dependencies: %w", err)
	}
	return order, nil
}

// Apply runs the reviewed plan: services roll back sequentially in
// dependency order, stopping at the first failure; proxy, takod state and
// history then converge for the services that changed. A plan with nothing
// to roll back returns status up_to_date without mutating anything.
func (s *RollbackSession) Apply(ctx context.Context) (*RollbackResult, error) {
	if s == nil || s.closed {
		return nil, fmt.Errorf("rollback session is closed")
	}
	if s.applied {
		return nil, fmt.Errorf("rollback session was already applied")
	}
	s.applied = true
	e := s.engine
	cfg := s.cfg
	envName := s.envName
	target := s.target
	leaseCtx, cancelLeaseContext := s.leases.BindContext(ctx)
	defer cancelLeaseContext()
	ctx = leaseCtx
	if s.deployer != nil {
		s.deployer.SetBaseContext(ctx)
	}

	startTime := time.Now()
	result := s.newResult(startTime)
	rolledNames := s.planDoc.RolledBackServices()
	gitInfo := takodstate.GitInfo{
		Commit: target.GitCommit, CommitShort: target.GitCommitShort, Branch: target.GitBranch, Message: target.GitCommitMsg, Author: target.GitAuthor,
	}

	if err := callBeforeMutation(ctx, s.req.BeforeMutation, MutationInfo{
		Operation:          "rollback",
		Services:           append([]string(nil), rolledNames...),
		Servers:            append([]string(nil), s.mutationServers...),
		TargetDeploymentID: target.ID,
		Revision:           RollbackTargetCommit(target),
	}); err != nil {
		return nil, err
	}

	e.emit(events.Event{
		Type:    events.TypeDeployStarted,
		Phase:   events.PhaseDeploy,
		Level:   events.LevelInfo,
		Message: fmt.Sprintf("\n=== Rolling back %s to deployment %s ===\n\n", envName, target.ID),
		Data:    map[string]any{"scope": RollbackScopeProject},
	})
	e.emitRollbackSkips(result.Services)

	if len(rolledNames) == 0 {
		result.Status = RollbackStatusUpToDate
		result.Message = fmt.Sprintf("every service already matches deployment %s", target.ID)
		result.Duration = time.Since(startTime).Seconds()
		e.info(events.TypeDeploySucceeded, events.PhaseDeploy, fmt.Sprintf("\n✓ Nothing to roll back: every service already matches deployment %s\n", target.ID))
		s.attachSummary(ctx, result, "", nil)
		return result, nil
	}

	notifier := rollbackNotifier(cfg, s.req.Verbose)
	if notifier != nil {
		notifier.Notify(notification.Event{
			Type:        notification.EventRollbackStarted,
			Project:     cfg.Project.Name,
			Environment: envName,
			Message:     fmt.Sprintf("Rolling back %s to deployment `%s` (%d services: %s)", envName, target.ID, len(rolledNames), strings.Join(rolledNames, ", ")),
			Details: map[string]string{
				"deployment_id": target.ID,
				"version":       target.Version,
				"services":      strings.Join(rolledNames, ","),
				"user":          remotestate.GetCurrentUser(),
			},
		})
	}

	// Runtime setup and the placement intent are recorded once for every
	// planned service before the first service changes.
	actualBefore, order, err := s.prepareApply(gitInfo)
	if err != nil {
		result.Services = notAttemptedOutcomes(result.Services)
		result.Status = takoapi.StatusFailed
		result.Error = e.RedactSensitive(err.Error())
		result.Duration = time.Since(startTime).Seconds()
		if notifier != nil {
			notifier.Notify(notification.Event{
				Type:        notification.EventDeployFailed,
				Project:     cfg.Project.Name,
				Environment: envName,
				Message:     fmt.Sprintf("Rollback of %s to deployment `%s` failed before any service changed", envName, target.ID),
				Error:       result.Error,
			})
		}
		s.attachSummary(ctx, result, "", nil)
		return result, err
	}
	worktrees := &rollbackWorktrees{}
	defer worktrees.close()
	rolled, applyErr := e.runRollbackSequence(ctx, order, result.Services, func(ctx context.Context, outcome RollbackServiceOutcome) (*RecoveryOutcome, error) {
		state := s.targets[outcome.Name]
		opts := deployer.RollbackOptions{SkipRelease: !s.req.RunRelease, ImageSource: outcome.ImageSource}
		var stepErr error
		if outcome.ImageSource == RollbackImageRebuild {
			commit := RollbackTargetCommit(s.records[outcome.SourceDeploymentID])
			dir, err := worktrees.dir(commit)
			if err != nil {
				return nil, err
			}
			e.debug(events.TypeLogLine, events.PhaseDeploy, fmt.Sprintf("  Rebuilding rollback image for %s from commit %s...\n", outcome.Name, commit))
			stepErr = WithWorkingDirectory(dir, func() error {
				return s.deployer.RollbackToState(outcome.Name, &state, opts)
			})
		} else {
			stepErr = s.deployer.RollbackToState(outcome.Name, &state, opts)
		}
		if stepErr == nil {
			return nil, nil
		}
		previousRevision := ""
		if actual := actualBefore[outcome.Name]; actual != nil {
			previousRevision = actual.CurrentRevision
		}
		return recoveryOutcomeFor(s.deployer, outcome.Name, previousRevision), stepErr
	})

	rolledStates := make(map[string]remotestate.ServiceState, len(rolled))
	for _, name := range rolled {
		rolledStates[name] = s.targets[name]
	}
	duration := time.Since(startTime)
	recordID, bookkeepingErr := s.converge(ctx, startTime, duration, rolledStates, applyErr, gitInfo)

	result.Duration = time.Since(startTime).Seconds()
	if applyErr != nil {
		result.Status = takoapi.StatusFailed
		result.Error = e.RedactSensitive(applyErr.Error())
		var notes []string
		if bookkeepingErr != nil {
			e.warn(events.PhaseState, fmt.Sprintf("Warning: %v\n", bookkeepingErr))
			notes = append(notes, bookkeepingErr.Error())
		}
		if notifier != nil {
			failedName, failedRecovery := failedRollbackService(result.Services)
			message, details := failureNotificationContent(fmt.Sprintf("Rollback of %s to deployment `%s` failed", envName, target.ID), map[string]string{"deployment_id": target.ID}, failedName, failedRecovery)
			notifier.Notify(notification.Event{
				Type:        notification.EventDeployFailed,
				Project:     cfg.Project.Name,
				Environment: envName,
				Message:     message,
				Error:       result.Error,
				Details:     details,
			})
		}
		e.emit(events.Event{
			Type:    events.TypeDeployFailed,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelError,
			Message: fmt.Sprintf("\n✗ Rollback to deployment %s failed: %d of %d service(s) rolled back\n", target.ID, len(rolled), len(rolledNames)),
		})
		s.attachSummary(ctx, result, recordID, notes)
		return result, applyErr
	}
	if bookkeepingErr != nil {
		result.Status = takoapi.StatusFailed
		result.Error = e.RedactSensitive(bookkeepingErr.Error())
		s.attachSummary(ctx, result, recordID, []string{"rollback succeeded; state bookkeeping failed: " + result.Error})
		return result, bookkeepingErr
	}

	if notifier != nil {
		notifier.Notify(notification.Event{
			Type:        notification.EventRollbackDone,
			Project:     cfg.Project.Name,
			Environment: envName,
			Message:     fmt.Sprintf("Successfully rolled back %s to deployment `%s` (%d services) in %s", envName, target.ID, len(rolled), duration.Round(time.Second)),
			Duration:    duration,
			Details: map[string]string{
				"deployment_id": target.ID,
				"version":       target.Version,
				"services":      strings.Join(rolled, ","),
			},
		})
	}
	e.info(events.TypeDeploySucceeded, events.PhaseDeploy, fmt.Sprintf("\n✓ Successfully rolled back %d service(s) to deployment %s!\n", len(rolled), target.ID))
	result.Status = takoapi.DeploymentStatus(remotestate.StatusRolledBack)
	result.Message = projectRollbackMessage(target.ID, len(rolled))
	s.attachSummary(ctx, result, recordID, nil)
	return result, nil
}

// attachSummary builds the project rollback summary onto result. recordID
// is the history record the rollback wrote, when written.
func (s *RollbackSession) attachSummary(ctx context.Context, result *RollbackResult, recordID string, notes []string) {
	if result == nil {
		return
	}
	e := s.engine
	in := rollbackSummaryInput{
		Environment:  s.envName,
		StartedAt:    result.StartedAt,
		Duration:     result.Duration,
		DeploymentID: recordID,
		Services:     s.services,
		Outcomes:     result.Services,
		Failed:       result.Status == takoapi.StatusFailed,
		Error:        result.Error,
		Notes:        notes,
		Target:       s.target,
		RollForward:  newestStableRecord(s.history, "", time.Time{}, ""),
	}
	if s.cfg != nil {
		in.Project = s.cfg.Project.Name
	}
	if s.target != nil {
		in.Commit = firstNonEmpty(s.target.GitCommitShort, s.target.GitCommit)
	}
	if s.runtime != nil {
		in.Health = e.healthProbe(ctx, s.runtime, s.cfg, s.envName, s.mutationServers, rollbackPlacementServices(s.services, s.targets))
	}
	result.Summary = buildRollbackSummary(e.RedactSensitive, in)
}

// prepareApply sets up the takod runtime, persists the placement intent for
// every planned service, and resolves the dependency order. It returns the
// actual state observed before the first service changes.
func (s *RollbackSession) prepareApply(gitInfo takodstate.GitInfo) (map[string]*reconcile.ActualService, []string, error) {
	cfg, envName := s.cfg, s.envName
	order, err := rollbackOrder(s.services, s.req.Verbose)
	if err != nil {
		return nil, nil, err
	}
	if err := s.deployer.SetupTakodRuntime(); err != nil {
		return nil, nil, fmt.Errorf("failed to setup takod runtime: %w", err)
	}
	placementServices := rollbackPlacementServices(s.services, s.targets)
	actualState, err := reconcile.GatherActualStateFromServers(s.sshPool, cfg, envName, s.mutationServers, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to gather actual state before rollback placement intent: %w", err)
	}
	intentImageRefs := deployplan.MergeRuntimeImageRefs(cfg, envName, placementServices, nil, actualState)
	for name, state := range s.targets {
		intentImageRefs[name] = state.Image
	}
	if err := PersistTakodDesiredIntentWithPlacementBaseline(s.sshPool, cfg, envName, s.mutationServers, "rollback", placementServices, intentImageRefs, s.deployer.ResolvedAssignments(), nil, s.priorDesired, gitInfo, "recorded stable placement before rollback mutation", s.req.Verbose); err != nil {
		return nil, nil, err
	}
	return actualState, order, nil
}

// notAttemptedOutcomes marks every planned service not_attempted.
func notAttemptedOutcomes(outcomes []RollbackServiceOutcome) []RollbackServiceOutcome {
	for index := range outcomes {
		if outcomes[index].Action == RollbackActionRolledBack {
			outcomes[index].Action = RollbackActionNotAttempted
		}
	}
	return outcomes
}

// failedRollbackService returns the failed service name and recovery for
// the failure notification.
func failedRollbackService(outcomes []RollbackServiceOutcome) (string, *RecoveryOutcome) {
	for _, outcome := range outcomes {
		if outcome.Action == RollbackActionFailed {
			return outcome.Name, outcome.Recovery
		}
	}
	return "", nil
}

func projectRollbackMessage(targetID string, services int) string {
	return fmt.Sprintf("rolled back project to deployment %s (%d services)", targetID, services)
}

func rollbackNotifier(cfg *config.Config, verbose bool) *notification.Notifier {
	if cfg.Notifications == nil || (cfg.Notifications.Slack == "" && cfg.Notifications.Discord == "" && cfg.Notifications.Webhook == "") {
		return nil
	}
	return notification.NewNotifier(notification.NotifierConfig{
		SlackWebhook:   cfg.Notifications.Slack,
		DiscordWebhook: cfg.Notifications.Discord,
		Webhook:        cfg.Notifications.Webhook,
	}, verbose)
}

// converge records the outcome after the apply loop, whether or not it
// succeeded: one history record (rolled_back, or failed with Error), then
// proxy, revision pruning and takod runtime state for the services that
// changed, replication and the local state entry.
func (s *RollbackSession) converge(ctx context.Context, startTime time.Time, duration time.Duration, rolled map[string]remotestate.ServiceState, applyErr error, gitInfo takodstate.GitInfo) (string, error) {
	recordID := ""
	err := s.convergeRecord(ctx, startTime, duration, rolled, applyErr, gitInfo, &recordID)
	return recordID, err
}

func (s *RollbackSession) convergeRecord(ctx context.Context, startTime time.Time, duration time.Duration, rolled map[string]remotestate.ServiceState, applyErr error, gitInfo takodstate.GitInfo, recordID *string) error {
	e := s.engine
	cfg := s.cfg
	envName := s.envName
	target := s.target
	// Bookkeeping must finish even when the apply was cancelled.
	ctx = context.WithoutCancel(ctx)

	record := BuildRollbackDeploymentForServices(cfg, envName, s.stateHost, startTime, duration, target, rolled, projectRollbackMessage(target.ID, len(rolled)), e.cliVersion, e.cliCommit)
	eventType := "rollback.succeeded"
	if applyErr != nil {
		record.Status = remotestate.StatusFailed
		record.Error = e.RedactSensitive(applyErr.Error())
		eventType = "rollback.partial"
	}
	wrap := func(format string, err error) error {
		if applyErr == nil {
			return fmt.Errorf("rollback succeeded but "+format, err)
		}
		return fmt.Errorf("rollback bookkeeping: "+format, err)
	}
	if err := s.stateManager.SaveDeploymentContext(ctx, record); err != nil {
		if applyErr == nil {
			return RollbackRemoteHistoryError(err)
		}
		return wrap("failed to update remote deployment history: %w", err)
	}
	*recordID = record.ID

	if len(rolled) > 0 {
		actualStateForProxy, err := reconcile.GatherActualStateFromServers(s.sshPool, cfg, envName, s.envServers, nil)
		if err != nil {
			return wrap("failed to gather actual state before proxy reconciliation: %w", err)
		}
		desiredServices, rollbackImageRefs, activeRevisions := RollbackProxyInputsForServices(cfg, envName, s.services, rolled, actualStateForProxy)
		if err := ReconcileProxy(s.deployer, desiredServices, activeRevisions); err != nil {
			return wrap("failed to reconcile proxy: %w", err)
		}
		rolledConfigs := make(map[string]config.ServiceConfig, len(rolled))
		for name := range rolled {
			rolledConfigs[name] = desiredServices[name]
		}
		if err := s.deployer.PruneTakodServiceRevisions(desiredServices, deployplan.DeployedProxyActiveRevisions(rolledConfigs, activeRevisions)); err != nil {
			return wrap("failed to prune stale service revisions: %w", err)
		}
		postNodeActualState, err := reconcile.GatherActualStateByServer(s.sshPool, cfg, envName, s.envServers)
		if err != nil {
			return wrap("failed to gather post-rollback actual state: %w", err)
		}
		postActualState := reconcile.AggregateActualStateByServer(postNodeActualState)
		runtimeImageRefs := deployplan.MergeRuntimeImageRefs(cfg, envName, desiredServices, rollbackImageRefs, postActualState)
		names := make([]string, 0, len(rolled))
		for name := range rolled {
			names = append(names, name)
		}
		sort.Strings(names)
		if err := PersistTakodRuntimeStateWithPlacementBaseline(
			s.sshPool, cfg, envName, s.mutationServers, "rollback",
			desiredServices, runtimeImageRefs, postActualState, postNodeActualState,
			s.deployer.ResolvedAssignments(), s.priorDesired, gitInfo,
			eventType,
			projectRollbackMessage(target.ID, len(rolled)),
			map[string]string{"services": strings.Join(names, ","), "deploymentId": target.ID},
			s.req.Verbose,
		); err != nil {
			return wrap("failed to persist takod state: %w", err)
		}
		e.debug(events.TypeStatePersisted, events.PhaseState, "")
	}

	if cfg.IsMultiServer() && ShouldReplicateDeploymentHistory(cfg) {
		replicator := remotestate.NewStateReplicator(s.sshPool, cfg, envName, cfg.Project.Name, s.req.Verbose)
		history, err := s.stateManager.LoadHistoryContext(ctx)
		if err != nil {
			return wrap("failed to load remote deployment history for replication: %w", err)
		}
		if err := replicator.ReplicateDeploymentContext(ctx, record, history); err != nil {
			return wrap("failed to replicate remote deployment history: %w", err)
		}
		e.debug(events.TypeStateReplicated, events.PhaseState, "")
	}

	e.saveLocalRollbackRecord(cfg, envName, s.envServers, record, fmt.Sprintf("Rolled back project to deployment %s (%d services)", target.ID, len(rolled)))
	return nil
}
