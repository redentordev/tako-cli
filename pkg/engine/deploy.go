package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/dependency"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/notification"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

// StateAutoSyncFunc refreshes local deployment state from the remote mesh
// before planning. Errors are non-fatal by contract.
type StateAutoSyncFunc func(pool *ssh.Pool, cfg *config.Config, envName string) error

// GraceSleep pauses blue-green pruning; tests may replace it.
var GraceSleep = time.Sleep

// DeploySession carries an in-flight deploy between Plan and Apply. It holds
// the local state lock, remote leases, and SSH pool for the operation; Close
// releases them. A session must be closed exactly once, after Apply or after
// the caller abandons the plan.
type DeploySession struct {
	engine *Engine
	req    DeployRequest

	cfg     *config.Config
	envName string
	workDir string
	verbose bool
	planDoc DeployPlan
	plan    *reconcile.ReconciliationPlan

	sourceInfo  SourceInfo
	gitStrings  GitStrings
	dirtyStatus string
	buildTag    string

	allServices             map[string]config.ServiceConfig
	services                map[string]config.ServiceConfig
	serverNames             []string
	mutationServerNames     []string
	connectivityServerNames []string
	servers                 map[string]config.ServerConfig
	actualState             map[string]*reconcile.ActualService
	actualByNode            map[string]map[string]*reconcile.ActualService
	priorDesired            *takodstate.DesiredRevision

	archiveDir    string
	stateLock     *localstate.StateLock
	lockInfo      *localstate.LockInfo
	sshPool       *ssh.Pool
	runtime       *nodeclient.Factory
	leases        *RemoteLeaseSet
	deployer      *deployer.Deployer
	stateManager  *remotestate.StateManager
	localStateMgr *localstate.Manager
	sourceServer  config.ServerConfig

	closed  bool
	applied bool
}

// Plan returns the serializable plan document for confirmation screens and
// machine output.
func (s *DeploySession) Plan() DeployPlan {
	return s.planDoc
}

// NeedsConfirmation reports whether the plan includes destructive changes
// that need explicit approval before Apply.
func (s *DeploySession) NeedsConfirmation() bool {
	return s.plan.NeedsConfirmation()
}

// Close releases the session's resources: temp build contexts, remote
// leases, SSH connections, and the local state lock. Idempotent.
func (s *DeploySession) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
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
	if s.archiveDir != "" {
		_ = os.RemoveAll(s.archiveDir)
	}
}

func (r *DeployRequest) workDirOrDefault() string {
	if strings.TrimSpace(r.WorkDir) != "" {
		return r.WorkDir
	}
	return "."
}

// PlanDeploy validates the request, acquires the local lock and remote
// leases, gathers running state, and computes the reconciliation plan. The
// returned session must be Closed; call Apply to execute the plan.
func (e *Engine) PlanDeploy(ctx context.Context, req DeployRequest) (*DeploySession, error) {
	defer e.flushBuildOutput()
	if req.Config == nil {
		return nil, invalidRequestf("deploy request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("deploy request requires an environment")
	}
	cfg := req.Config
	if strings.TrimSpace(req.BuildStrategy) != "" {
		if err := cfg.SetBuildStrategy(req.BuildStrategy); err != nil {
			return nil, &InvalidRequestError{Err: err}
		}
	}
	if err := EnsureDeployRuntimeSupported(cfg); err != nil {
		return nil, err
	}

	session := &DeploySession{
		engine:  e,
		req:     req,
		cfg:     cfg,
		envName: req.Environment,
		workDir: req.workDirOrDefault(),
		verbose: req.Verbose,
	}
	ok := false
	defer func() {
		if !ok {
			session.Close()
		}
	}()

	archivePath, err := ValidateArchiveOptions(req.Service, req.Archive, req.Source, req.Image)
	if err != nil {
		return nil, err
	}
	imageRef, err := ValidateImageOptions(req.Service, req.Image, req.Source)
	if err != nil {
		return nil, err
	}
	sourceLabel := SourceLabelForImageOverride(req.Source, imageRef)
	revisionForSourceInfo := req.Revision
	if archivePath != "" {
		sourceLabel = SourceLabelForArchive(archivePath)
		revisionForSourceInfo, err = ArchiveBuildTag(req.Revision, archivePath)
		if err != nil {
			return nil, err
		}
	}

	archiveBuildContext := ""
	if archivePath != "" {
		archiveBuildContext, err = os.MkdirTemp("", "tako-archive-*")
		if err != nil {
			return nil, fmt.Errorf("failed to create archive build context: %w", err)
		}
		session.archiveDir = archiveBuildContext
		if err := ExtractArchive(archivePath, archiveBuildContext); err != nil {
			return nil, fmt.Errorf("failed to extract archive %q: %w", archivePath, err)
		}
	}

	// Initialize source metadata. Default mode requires Git; source mode skips Git validation.
	gitClient := git.NewClient(session.workDir)
	sourceInfo, err := ResolveSourceInfo(gitClient, req.AllowDirty, sourceLabel, revisionForSourceInfo, imageRef, time.Now())
	if err != nil {
		return nil, err
	}
	session.sourceInfo = sourceInfo
	session.gitStrings = GitStringsFromCommit(sourceInfo.CommitInfo)
	session.dirtyStatus = sourceInfo.DirtyStatus
	session.buildTag = sourceInfo.BuildImageTag

	// Acquire state lock to prevent concurrent deployments.
	session.stateLock = localstate.NewStateLock(session.takoDir())
	lockInfo, err := session.stateLock.Acquire("deploy")
	if err != nil {
		holder := ""
		if info, infoErr := session.stateLock.GetLockInfo(); infoErr == nil && info != nil {
			holder = info.Who
		}
		session.stateLock = nil
		return nil, &LockedError{Operation: "deploy", Holder: holder, Err: fmt.Errorf("cannot deploy: %w", err)}
	}
	session.lockInfo = lockInfo
	e.debug(events.TypeLogLine, events.PhasePlan, fmt.Sprintf("→ Acquired deployment lock (ID: %s)\n", lockInfo.ID))

	// Display source info.
	if sourceInfo.SourceMode {
		e.info(events.TypeLogLine, events.PhasePlan, fmt.Sprintf("\n📦 Deploying source:\n  Source:   %s\n  Revision: %s\n", sourceInfo.StateSource, session.buildTag))
	} else {
		e.info(events.TypeLogLine, events.PhasePlan, fmt.Sprintf("\n📦 Deploying commit:\n  Hash:    %s\n  Branch:  %s\n  Author:  %s\n  Message: %s\n", session.gitStrings.ShortHash, session.gitStrings.Branch, session.gitStrings.Author, session.gitStrings.Message))
	}
	if session.dirtyStatus != "" {
		message := "\n⚠ Deploying with uncommitted local changes (--allow-dirty).\n  Deployment history records HEAD only; uncommitted file contents are not recoverable from Git.\n"
		e.warn(events.PhasePlan, message)
		e.debug(events.TypeLogLine, events.PhasePlan, fmt.Sprintf("  Dirty files:\n%s\n", session.dirtyStatus))
	}

	// Get environment services and register their env values with the
	// redactor before any event could leak them.
	allServices, err := cfg.GetServices(session.envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get services: %w", err)
	}
	session.allServices = allServices
	for _, service := range allServices {
		for _, value := range service.Env {
			e.RegisterSecret(value)
		}
	}
	for _, build := range cfg.Builds {
		for _, value := range build.Args {
			e.RegisterSecret(value)
		}
	}
	e.registerServiceSecretValues(session.envName, allServices)
	for _, server := range cfg.Servers {
		e.RegisterSecret(server.Password)
	}
	e.RegisterRegistrySecrets(cfg)
	e.RegisterACMEDNSSecrets(cfg)

	// Create SSH pool.
	session.sshPool = ssh.NewPool()

	// Determine which environment nodes to deploy to.
	envServerNames, err := cfg.GetEnvironmentServers(session.envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	servers := make(map[string]config.ServerConfig, len(envServerNames))
	connectivityServerNames := append([]string(nil), envServerNames...)
	serverNames, err := config.ResolveSchedulableEnvironmentTargets(cfg.Servers, connectivityServerNames, session.envName)
	if err != nil {
		return nil, err
	}
	for _, serverName := range serverNames {
		server, exists := cfg.Servers[serverName]
		if !exists {
			return nil, invalidRequestf("server %s not found in configuration", serverName)
		}
		servers[serverName] = server
	}
	session.serverNames = serverNames
	session.connectivityServerNames = connectivityServerNames
	session.servers = servers

	// Determine which services to deploy.
	services := allServices
	if req.Service != "" {
		service, exists := allServices[req.Service]
		if !exists {
			return nil, invalidRequestf("service %s not found in environment %s", req.Service, session.envName)
		}
		if imageRef != "" {
			service = ApplyImageOverride(service, imageRef)
		} else if archiveBuildContext != "" {
			service = ApplyArchiveOverride(service, archiveBuildContext)
		} else {
			service = ApplySourceOverride(service, req.Source)
		}
		services = map[string]config.ServiceConfig{req.Service: service}
	}
	session.services = services

	meshLine := "Mesh: disabled\n"
	if cfg.IsMeshEnabled() {
		meshLine = fmt.Sprintf("Mesh: enabled (%s via %s)\n", cfg.Mesh.NetworkCIDR, cfg.Mesh.Interface)
	}
	e.info(events.TypeDeployStarted, events.PhasePlan, fmt.Sprintf(
		"\n=== Starting deployment ===\n\nProject: %s v%s\nEnvironment: %s\nRuntime: %s\nState: %s (consistency: %s)\nBuild strategy: %s\n%sServers: %d\nServices: %d\n\n",
		cfg.Project.Name, cfg.Project.Version, session.envName, cfg.GetRuntimeMode(),
		cfg.GetStateBackend(), cfg.GetDeployConsistency(), cfg.GetBuildStrategy(),
		meshLine, len(servers), len(services)))

	if len(serverNames) == 0 {
		return nil, invalidRequestf("no servers configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sourceServerName, err := PreferredRuntimeServer(cfg, serverNames)
	if err != nil {
		return nil, err
	}
	session.sourceServer = servers[sourceServerName]

	// Use one reachable target as the build/source node through the structured
	// runtime transport. Enrolled node 1 can therefore deploy without an SSH
	// round trip or shell while legacy configurations remain SSH-backed.
	runtimeFactory, err := nodeclient.NewFactory(cfg, session.sshPool, TakodSocketFromConfig(cfg))
	if err != nil {
		return nil, err
	}
	session.runtime = runtimeFactory
	sourceRuntime, sourceDecision, err := runtimeFactory.Client(ctx, sourceServerName)
	if err != nil {
		return nil, &ConnectivityError{Server: sourceServerName, Err: fmt.Errorf("failed to connect to runtime on server %s: %w", sourceServerName, err)}
	}
	stateRuntime, stateHost := any(sourceRuntime), session.sourceServer.Host
	if controllerName, enrolled, authorityErr := controllerAuthorityServer(cfg, serverNames); authorityErr != nil {
		return nil, authorityErr
	} else if enrolled {
		controllerRuntime, _, clientErr := runtimeFactory.Client(ctx, controllerName)
		if clientErr != nil {
			return nil, &ConnectivityError{Server: controllerName, Err: fmt.Errorf("connect to authoritative state controller: %w", clientErr)}
		}
		stateRuntime = controllerRuntime
		stateHost = cfg.Servers[controllerName].Host
	}
	session.stateManager = remotestate.NewStateManagerWithSocket(stateRuntime, cfg.Project.Name, session.envName, stateHost, TakodSocketFromConfig(cfg))

	// Create deployer with pool for takod support.
	deploy := deployer.NewDeployerWithPool(nil, cfg, session.envName, session.sshPool, req.Verbose)
	priorDesired, priorAssignments, err := LoadPriorPlacementState(stateRuntime, cfg, session.envName)
	if err != nil {
		return nil, err
	}
	session.priorDesired = priorDesired
	if err := ValidatePriorDesiredServices(priorDesired, allServices); err != nil {
		return nil, err
	}
	deploy.SetPriorAssignments(priorAssignments)
	deploy.SetRuntimeFactory(runtimeFactory)
	deploy.SetCLIVersion(e.cliVersion)
	deploy.SetSkipBuild(req.SkipBuild)
	if output := e.buildOutputWriter(); output != nil {
		deploy.SetOutput(output)
	}
	allServices, err = prepareServiceFileHashes(deploy, allServices)
	if err != nil {
		return nil, err
	}
	if req.Service == "" {
		services = allServices
	} else {
		services, err = prepareServiceFileHashes(deploy, services)
		if err != nil {
			return nil, err
		}
	}
	services, err = prepareRunInputHashes(deploy, services)
	if err != nil {
		return nil, err
	}
	for name, service := range services {
		allServices[name] = service
	}
	session.services = services
	session.allServices = allServices
	if err := deploy.ResolveAllAssignments(allServices); err != nil {
		return nil, err
	}
	deploy.SetEventSink(e.stream)
	session.deployer = deploy
	mutationServerNames := DeployMutationTargets(cfg, allServices, deploy.ResolvedAssignments())
	mutationServerNames = AddPriorDesiredMutationTargets(cfg, mutationServerNames, priorDesired)
	if len(mutationServerNames) == 0 {
		mutationServerNames = []string{sourceServerName}
	}
	environmentTarget := make(map[string]struct{}, len(serverNames))
	for _, name := range serverNames {
		environmentTarget[name] = struct{}{}
	}
	setupTargets := make([]string, 0, len(mutationServerNames))
	for _, name := range mutationServerNames {
		if _, ok := environmentTarget[name]; ok {
			setupTargets = append(setupTargets, name)
		}
	}
	if len(setupTargets) == 0 {
		setupTargets = []string{sourceServerName}
	}
	if err := deploy.SetTargetServers(setupTargets); err != nil {
		return nil, err
	}
	leaseSet, err := AcquireRemoteOperationLeasesContext(ctx, session.sshPool, cfg, session.envName, mutationServerNames, "deploy")
	if err != nil {
		return nil, err
	}
	session.leases = leaseSet
	session.mutationServerNames = mutationServerNames
	leaseCtx, cancelLeaseContext := leaseSet.BindContext(ctx)
	defer cancelLeaseContext()
	ctx = leaseCtx
	leaseSet.SetWarnFunc(func(message string) {
		e.debug(events.TypeWarning, events.PhaseDeploy, message)
	})
	deploy.SetBaseContext(ctx)
	e.debug(events.TypeLogLine, events.PhasePlan, fmt.Sprintf("→ Acquired remote deploy authority: %s\n", leaseSet.Summary()))

	if err := preflightAndSetupTakodRuntime(deploy, allServices); err != nil {
		return nil, err
	}

	// Auto-sync local state from remote when available (best-effort).
	if e.stateAutoSync != nil && sourceDecision.Transport != nodeclient.TransportLocal {
		if err := e.stateAutoSync(session.sshPool, cfg, session.envName); err != nil {
			e.debug(events.TypeWarning, events.PhasePlan, fmt.Sprintf("Warning: auto-sync failed: %v\n", err))
		}
	} else if e.stateAutoSync != nil {
		e.debug(events.TypeLogLine, events.PhasePlan, "→ Local runtime transport selected; remote state is read directly from the local worker ingress\n")
	}

	// Compare desired state (config) with actual state (running services).
	e.debug(events.TypeLogLine, events.PhasePlan, "\n→ Computing deployment plan...\n")

	localStateMgr, err := localstate.NewManager(session.workDir, cfg.Project.Name, session.envName)
	if err != nil {
		e.debug(events.TypeWarning, events.PhasePlan, fmt.Sprintf("Warning: failed to initialize local state: %v\n", err))
		localStateMgr = nil // Continue without state management.
	}
	session.localStateMgr = localStateMgr
	e.warnRetiredDeploymentServers(localStateMgr, connectivityServerNames)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Gather actual state from running containers across the selected mesh nodes.
	actualByNode, err := reconcile.GatherActualStateByServer(session.sshPool, cfg, session.envName, mutationServerNames)
	if err != nil {
		return nil, ActualStateError(err)
	}
	actualState := reconcile.AggregateActualStateByServer(actualByNode)
	session.actualByNode = actualByNode
	session.actualState = actualState
	planImageRefs := deployplan.DefaultDeployImageRefs(cfg, session.envName, allServices, session.buildTag)
	if req.Service != "" {
		if targeted, ok := services[req.Service]; ok && targeted.IsRun() && targeted.ImageFrom != "" && targeted.SharedBuildHash == "" {
			sourceConfig := allServices[targeted.ImageFrom]
			if source := actualState[targeted.ImageFrom]; source != nil && source.Image != "" {
				planImageRefs[targeted.ImageFrom] = source.Image
			} else if sourceConfig.Build != "" {
				return nil, fmt.Errorf("targeted run %s needs a deployed image from build-backed service %s; deploy that service first or run a full deploy", req.Service, targeted.ImageFrom)
			}
		}
	}
	planServices, err := prepareRunPlanServices(services, allServices, planImageRefs)
	if err != nil {
		return nil, err
	}
	runPrerequisites := map[string]config.ServiceConfig{}
	if req.Service != "" {
		runPrerequisites = targetRunPrerequisites(allServices, req.Service)
		runPrerequisites, err = prepareRunInputHashes(deploy, runPrerequisites)
		if err != nil {
			return nil, err
		}
		for name, prerequisite := range runPrerequisites {
			if prerequisite.ImageFrom == "" || prerequisite.SharedBuildHash != "" {
				continue
			}
			source := allServices[prerequisite.ImageFrom]
			if source.Build != "" {
				deployed := actualState[prerequisite.ImageFrom]
				if deployed == nil || deployed.Image == "" {
					return nil, fmt.Errorf("targeted service %s requires deploy-time run %s, but its build-backed imageFrom service %s is not deployed; run a full deploy first", req.Service, name, prerequisite.ImageFrom)
				}
				planImageRefs[prerequisite.ImageFrom] = deployed.Image
			}
		}
		runPrerequisites, err = prepareRunPlanServices(runPrerequisites, allServices, planImageRefs)
		if err != nil {
			return nil, err
		}
	}
	if HasRunServices(planServices) || len(runPrerequisites) > 0 {
		history, historyErr := session.stateManager.LoadHistoryContext(ctx)
		if historyErr != nil && !errors.Is(historyErr, remotestate.ErrNotFound) {
			return nil, fmt.Errorf("failed to load deploy-time run history: %w", historyErr)
		}
		addRunHistoryActual(actualState, planServices, history)
		if err := ensureRunPrerequisitesCompleted(req.Service, runPrerequisites, history); err != nil {
			return nil, err
		}
	}
	if err := validateRunKindTransitions(planServices, actualState); err != nil {
		return nil, err
	}
	if len(actualState) > 0 {
		e.debug(events.TypeLogLine, events.PhasePlan, fmt.Sprintf("  Found %d running service(s)\n", len(actualState)))
	}

	planActualState := actualState
	if req.Service != "" {
		planActualState = deployplan.FilterActualStateForServices(actualState, services)
	}

	// Compute reconciliation plan.
	plan := reconcile.ComputePlan(cfg.Project.Name, session.envName, planServices, planActualState)
	if err := rejectPersistentConfigRemovals(plan); err != nil {
		return nil, err
	}
	if _, err := removalServiceConfigs(plan); err != nil {
		return nil, err
	}
	session.plan = plan

	planDoc := newDeployPlanDocument(cfg.Project.Name, session.envName, plan, services)
	planDoc.Revision = session.buildTag
	planDoc.SharedBuilds = planSharedBuilds(cfg, session.envName, session.buildTag, services)
	planDoc.Source = sourceInfo.StateSource
	planDoc.Servers = append([]string(nil), serverNames...)
	planDoc.Services = sortedServiceNames(services)
	planDoc.Destructive = plan.NeedsConfirmation()
	planDoc.Empty = plan.IsEmpty()
	planDoc.HumanText = plan.FormatPlan()
	if !sourceInfo.SourceMode {
		planDoc.Git = &GitInfo{
			Commit:      session.gitStrings.Hash,
			CommitShort: session.gitStrings.ShortHash,
			Branch:      session.gitStrings.Branch,
			Message:     session.gitStrings.Message,
			Author:      session.gitStrings.Author,
			Dirty:       session.dirtyStatus != "",
		}
	}
	session.planDoc = planDoc

	e.emit(events.Event{
		Type:    events.TypePlanComputed,
		Phase:   events.PhasePlan,
		Level:   events.LevelInfo,
		Message: "\n" + plan.FormatPlan(),
		Data: map[string]any{
			"destructive": planDoc.Destructive,
			"empty":       planDoc.Empty,
			"changes":     len(planDoc.Changes),
			"planHash":    planDoc.Hash(),
		},
	})

	ok = true
	return session, nil
}

func (s *DeploySession) takoDir() string {
	if s.workDir == "." {
		return ".tako"
	}
	return filepath.Join(s.workDir, ".tako")
}

func (e *Engine) warnRetiredDeploymentServers(localStateMgr *localstate.Manager, currentServers []string) {
	if localStateMgr == nil {
		return
	}
	previous, err := localStateMgr.GetCurrentDeployment()
	if err != nil || previous == nil || len(previous.Servers) == 0 {
		return
	}
	retired := RetiredDeploymentServers(previous.Servers, currentServers)
	if len(retired) == 0 {
		return
	}
	message := fmt.Sprintf("\n⚠ Previous deployment included node(s) no longer in this environment: %s\n", strings.Join(retired, ", ")) +
		"  Tako cannot stop containers on nodes after their SSH config is removed.\n" +
		"  If the node still exists, re-add it temporarily and run 'tako remove --server <node>' before removing it.\n" +
		"  Use 'tako state forget-node <node> --yes' only to prune replicated state for a retired/destroyed node.\n"
	e.warn(events.PhasePlan, message)
}

// Apply executes a planned deployment. The caller is responsible for
// confirmation gating; Apply runs the plan unconditionally.
func (s *DeploySession) Apply(ctx context.Context) (*DeployResult, error) {
	if s.closed {
		return nil, fmt.Errorf("deploy session is closed")
	}
	if s.applied {
		return nil, fmt.Errorf("deploy session was already applied")
	}
	s.applied = true
	leaseCtx, cancelLeaseContext := s.leases.BindContext(ctx)
	defer cancelLeaseContext()
	ctx = leaseCtx
	if err := s.leases.Err(); err != nil {
		return nil, err
	}
	s.deployer.SetBaseContext(ctx)
	if err := s.deployer.PreflightTakodProxyCapabilities(s.allServices); err != nil {
		return nil, err
	}

	e := s.engine
	defer e.flushBuildOutput()
	req := s.req
	cfg := s.cfg
	envName := s.envName
	plan := s.plan
	services := s.services
	if err := rejectPersistentConfigRemovals(plan); err != nil {
		return nil, err
	}
	desiredStateServices := s.allServices
	actualState := s.actualState
	serverNames := s.serverNames

	result := &DeployResult{
		APIVersion:  s.planDoc.APIVersion,
		Kind:        KindDeployResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Revision:    s.buildTag,
		Git:         s.planDoc.Git,
		PlanHash:    s.planDoc.Hash(),
		StartedAt:   time.Now(),
	}

	if plan.IsEmpty() && !deployplan.HasBuildServices(services) && !req.Force {
		intentImageRefs := deployplan.MergeRuntimeImageRefs(cfg, envName, desiredStateServices, nil, actualState)
		var baseline *takodstate.DesiredRevision
		if req.Service != "" {
			baseline = s.priorDesired
		}
		if err := PersistTakodDesiredIntentWithPlacementBaseline(s.sshPool, cfg, envName, serverNames, s.sourceInfo.StateSource, desiredStateServices, intentImageRefs, s.deployer.ResolvedAssignments(), nil, baseline, GitInfoFromCommit(s.sourceInfo.CommitInfo), "recorded stable placement before proxy reconciliation", req.Verbose); err != nil {
			return nil, err
		}
		activeRevisions := deployplan.ProxyActiveRevisions(cfg, envName, services, nil, nil, actualState)
		if err := s.reconcileProxy(services, activeRevisions); err != nil {
			return nil, fmt.Errorf("failed to reconcile proxy routes: %w", err)
		}
		e.info(events.TypePlanUpToDate, events.PhaseDeploy, "\n✓ All services are up-to-date. Proxy routes reconciled.\n")
		result.Status = takoapi.DeploymentStatus(remotestate.StatusSuccess)
		result.Message = "all services up-to-date; proxy routes reconciled"
		for _, name := range sortedServiceNames(services) {
			result.Services = append(result.Services, ServiceOutcome{Name: name, Action: OutcomeUpToDate})
		}
		result.Duration = time.Since(result.StartedAt).Seconds()
		return result, nil
	}
	if plan.IsEmpty() {
		if req.Force {
			e.info(events.TypeLogLine, events.PhaseDeploy, "\n-> No config drift detected; --force will reconcile selected services anyway.\n")
		} else if s.sourceInfo.SourceMode {
			e.info(events.TypeLogLine, events.PhaseDeploy, "\n-> No config drift detected; build services will still be reconciled for the current source revision.\n")
		} else {
			e.info(events.TypeLogLine, events.PhaseDeploy, "\n-> No config drift detected; build services will still be reconciled for the current commit.\n")
		}
	}
	servicesToDeploy := deployplan.ServicesToDeployForPlan(plan, services, req.Force, req.Service != "")
	if err := s.deployer.PreflightAssignmentMutations(servicesToDeploy); err != nil {
		return nil, err
	}
	if err := s.deployer.PreflightAssignmentRemovals(removalServiceNames(plan)); err != nil {
		return nil, err
	}
	pendingRemovals, err := removalServiceConfigs(plan)
	if err != nil {
		return nil, err
	}
	if skipped := deployplan.PersistentServicesSkippedByForce(services, servicesToDeploy, req.Force, req.Service != ""); len(skipped) > 0 {
		e.info(events.TypeLogLine, events.PhaseDeploy, fmt.Sprintf("\n-> Skipping persistent service(s) during broad --force: %s\n   Use --service <name> --force when you intentionally need to recreate one.\n", strings.Join(skipped, ", ")))
	}

	if len(s.servers) == 1 {
		e.info(events.TypeLogLine, events.PhaseDeploy, "\n🐙 Using takod mesh runtime (one node)\n\n")
	} else {
		e.info(events.TypeLogLine, events.PhaseDeploy, fmt.Sprintf("\n🐙 Using takod mesh runtime (%d nodes)\n\n", len(s.servers)))
	}

	// Log deployment start.
	if s.localStateMgr != nil {
		s.localStateMgr.LogDeployment(fmt.Sprintf("Starting takod deployment to %s", envName))
		if s.sourceInfo.SourceMode {
			s.localStateMgr.LogDeployment(fmt.Sprintf("Source revision: %s", s.buildTag))
		} else {
			s.localStateMgr.LogDeployment(fmt.Sprintf("Git commit: %s", s.gitStrings.ShortHash))
		}
	}

	startTime := time.Now()
	result.StartedAt = startTime
	deployment := &remotestate.DeploymentState{
		Timestamp:      startTime,
		ProjectName:    cfg.Project.Name,
		Version:        cfg.Project.Version,
		Status:         remotestate.StatusInProgress,
		Services:       make(map[string]remotestate.ServiceState),
		User:           remotestate.GetCurrentUser(),
		Host:           s.sourceServer.Host,
		GitCommit:      s.gitStrings.Hash,
		GitCommitShort: s.gitStrings.ShortHash,
		GitBranch:      s.gitStrings.Branch,
		GitCommitMsg:   s.gitStrings.Message,
		GitAuthor:      s.gitStrings.Author,
		CLIVersion:     e.cliVersion,
		CLICommit:      e.cliCommit,
	}
	notificationRevisionLabel := "Commit"
	notificationRevisionValue := s.gitStrings.ShortHash
	if s.sourceInfo.SourceMode {
		notificationRevisionLabel = "Revision"
		notificationRevisionValue = s.buildTag
	}

	// Setup notifications if configured.
	var notifier *notification.Notifier
	if cfg.Notifications != nil && (cfg.Notifications.Slack != "" || cfg.Notifications.Discord != "" || cfg.Notifications.Webhook != "") {
		notifier = notification.NewNotifier(notification.NotifierConfig{
			SlackWebhook:   cfg.Notifications.Slack,
			DiscordWebhook: cfg.Notifications.Discord,
			Webhook:        cfg.Notifications.Webhook,
		}, req.Verbose)

		if err := notifier.Notify(notification.Event{
			Type:        notification.EventDeployStarted,
			Project:     cfg.Project.Name,
			Environment: envName,
			Message:     StartNotificationMessage(cfg.Project.Name, cfg.Project.Version, envName, notificationRevisionLabel, notificationRevisionValue, s.gitStrings.Message),
			Details: map[string]string{
				"version":  cfg.Project.Version,
				"commit":   s.gitStrings.ShortHash,
				"revision": s.buildTag,
				"branch":   s.gitStrings.Branch,
				"author":   s.gitStrings.Author,
				"user":     remotestate.GetCurrentUser(),
				"services": fmt.Sprintf("%d", len(services)),
			},
		}); err != nil {
			e.debug(events.TypeWarning, events.PhaseNotify, fmt.Sprintf("  Warning: failed to send start notification: %v\n", err))
		}
	}

	deploymentFailed := false
	var deploymentError error
	imageRefs := deployplan.DefaultDeployImageRefs(cfg, envName, services, s.buildTag)
	allImageRefs := deployplan.DefaultDeployImageRefs(cfg, envName, s.allServices, s.buildTag)
	if req.Service != "" {
		if targeted, ok := services[req.Service]; ok && targeted.IsRun() && targeted.ImageFrom != "" && targeted.SharedBuildHash == "" {
			if source := actualState[targeted.ImageFrom]; source != nil && source.Image != "" {
				allImageRefs[targeted.ImageFrom] = source.Image
			}
		}
	}
	for name, service := range services {
		if !service.IsRun() {
			continue
		}
		resolved, _, err := resolveRunImage(service, s.allServices, allImageRefs)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve run %s image: %w", name, err)
		}
		imageRefs[name] = resolved
		allImageRefs[name] = resolved
	}
	intentImageRefs := deployplan.MergeRuntimeImageRefs(cfg, envName, desiredStateServices, nil, actualState)
	for name, service := range pendingRemovals {
		if actual := actualState[name]; actual != nil && actual.Image != "" {
			intentImageRefs[name] = actual.Image
		} else if service.Image != "" {
			intentImageRefs[name] = service.Image
		}
	}
	for name, imageRef := range allImageRefs {
		if _, desired := desiredStateServices[name]; desired && (req.Service == "" || name == req.Service) {
			intentImageRefs[name] = imageRef
		}
	}
	var baseline *takodstate.DesiredRevision
	if req.Service != "" {
		baseline = s.priorDesired
	}
	if err := PersistTakodDesiredIntentWithPlacementBaseline(s.sshPool, cfg, envName, serverNames, s.sourceInfo.StateSource, desiredStateServices, intentImageRefs, s.deployer.ResolvedAssignments(), pendingRemovals, baseline, GitInfoFromCommit(s.sourceInfo.CommitInfo), "recorded stable placement before deploy mutation", req.Verbose); err != nil {
		return nil, err
	}

	// Resolve service deployment order based on dependencies.
	resolverServices := services
	if req.Service != "" {
		resolverServices = s.allServices
	}
	resolver := dependency.NewResolver(resolverServices, req.Verbose)
	inferredDeps := resolver.InferDependencies()
	resolver.MergeDependencies(inferredDeps)
	deploymentOrder, err := resolver.ResolveOrder()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve service dependencies: %w", err)
	}

	if err := RecordStartedDeploymentStateContext(ctx, s.stateManager, deployment); err != nil {
		return nil, fmt.Errorf("failed to record started deployment state before applying mutations: %w", err)
	}
	e.debug(events.TypeLogLine, events.PhaseState, fmt.Sprintf("→ Recorded in-progress deployment state (%s)\n", deployment.ID))
	if err := buildSharedImages(s.deployer, cfg, envName, s.buildTag, servicesToDeploy, req.SkipBuild); err != nil {
		err = fmt.Errorf("%s", e.redactor.Redact(err.Error()))
		deploymentFailed = true
		deploymentError = err
		deployment.Status = remotestate.StatusFailed
		deployment.Error = err.Error()
	}

	// Deploy each service through takod placement in dependency order.
	for _, serviceName := range deploymentOrder {
		if deploymentFailed {
			break
		}
		if err := ctx.Err(); err != nil {
			deploymentFailed = true
			deploymentError = err
			deployment.Status = remotestate.StatusFailed
			deployment.Error = err.Error()
			break
		}
		service, shouldDeploy := servicesToDeploy[serviceName]
		if !shouldDeploy {
			continue
		}
		e.emit(events.Event{
			Type:    events.TypeDeployServiceStarted,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: serviceName,
			Message: fmt.Sprintf("→ Deploying service: %s\n", serviceName),
		})
		if service.IsRun() {
			resolvedImage, pullImage, resolveErr := resolveRunImage(service, s.allServices, allImageRefs)
			if resolveErr != nil {
				deploymentFailed = true
				deploymentError = fmt.Errorf("failed to resolve run %s image: %w", serviceName, resolveErr)
				deployment.Status = remotestate.StatusFailed
				deployment.Error = deploymentError.Error()
				result.Services = append(result.Services, ServiceOutcome{Name: serviceName, Action: OutcomeFailed, Error: deploymentError.Error()})
				break
			}
			imageRefs[serviceName] = resolvedImage
			allImageRefs[serviceName] = resolvedImage
			var availableImageNodes []string
			if req.Service != "" && service.ImageFrom != "" && service.SharedBuildHash == "" && s.allServices[service.ImageFrom].Build != "" {
				availableImageNodes = make([]string, 0)
				for node, nodeState := range s.actualByNode {
					if source := nodeState[service.ImageFrom]; source != nil && source.Image == resolvedImage {
						availableImageNodes = append(availableImageNodes, node)
					}
				}
			}
			runResult, runErr := s.deployer.RunDeployStepOnNodes(serviceName, &service, resolvedImage, pullImage, availableImageNodes)
			outcome := runOutcome(runResult)
			if runErr != nil {
				result.Services = append(result.Services, ServiceOutcome{Name: serviceName, Image: resolvedImage, Action: OutcomeFailed, Error: runErr.Error(), Run: outcome})
				deploymentFailed = true
				deploymentError = fmt.Errorf("deploy-time run failed for %s: %w", serviceName, runErr)
				deployment.Status = remotestate.StatusFailed
				deployment.Error = runErr.Error()
				deployment.Services[serviceName] = runHistoryServiceState(serviceName, service, resolvedImage, runResult)
				break
			}
			result.Services = append(result.Services, ServiceOutcome{Name: serviceName, Image: resolvedImage, Action: OutcomeRan, Run: outcome})
			deployment.Services[serviceName] = runHistoryServiceState(serviceName, service, resolvedImage, runResult)
			continue
		}

		fullImageName := deployplan.ImageRef(cfg, envName, serviceName, service, s.buildTag)
		if service.Image != "" {
			// Use pre-built image.
			fullImageName = service.Image
		}
		imageRefs[serviceName] = fullImageName
		allImageRefs[serviceName] = fullImageName

		warmed := deployplan.ShouldWarmManualPromotionService(serviceName, service, actualState)
		deployErr := error(nil)
		if service.SharedBuildHash != "" {
			deployErr = s.deployer.DeployPreparedServiceTakod(serviceName, &service, fullImageName, warmed)
		} else if warmed {
			deployErr = s.deployer.DeployServiceTakodWarmOnly(serviceName, &service, fullImageName)
		} else {
			deployErr = s.deployer.DeployServiceTakod(serviceName, &service, fullImageName)
		}
		if deployErr != nil {
			e.emit(events.Event{
				Type:    events.TypeDeployServiceFailed,
				Phase:   events.PhaseDeploy,
				Level:   events.LevelError,
				Service: serviceName,
				Message: fmt.Sprintf("  ✗ takod deployment failed: %v\n", deployErr),
			})
			result.Services = append(result.Services, ServiceOutcome{Name: serviceName, Image: fullImageName, Action: OutcomeFailed, Replicas: service.Replicas, Error: deployErr.Error(), Release: releaseOutcomeFor(s.deployer, serviceName)})
			deploymentFailed = true
			deploymentError = fmt.Errorf("takod deployment failed for %s: %w", serviceName, deployErr)
			deployment.Status = remotestate.StatusFailed
			deployment.Error = deployErr.Error()
			break
		}

		e.emit(events.Event{
			Type:    events.TypeDeployServiceReconciled,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: serviceName,
			Message: fmt.Sprintf("  ✓ Service %s reconciled by takod\n", serviceName),
			Data:    map[string]any{"image": fullImageName},
		})
		outcomeAction := OutcomeDeployed
		if warmed {
			outcomeAction = OutcomeWarmed
		}
		result.Services = append(result.Services, ServiceOutcome{Name: serviceName, Image: fullImageName, Action: outcomeAction, Replicas: service.Replicas, Release: releaseOutcomeFor(s.deployer, serviceName)})

		// Save service state.
		deployment.Services[serviceName] = remotestate.ServiceState{
			Name:             serviceName,
			Image:            fullImageName,
			SharedBuild:      sharedBuildName(service),
			SharedBuildHash:  service.SharedBuildHash,
			FilesContentHash: service.FilesContentHash,
			Files:            historyServiceFiles(service.Files),
			Port:             service.Port,
			Replicas:         service.Replicas,
			Env:              RedactedEnvKeys(service.Env),
		}
	}

	if !deploymentFailed {
		jobServices := services
		if req.Service != "" {
			jobServices = CloneServiceMap(s.allServices)
		}
		if HasJobServices(jobServices) || planRemovesJob(plan) {
			if err := s.deployer.ApplyJobSchedules(jobServices); err != nil {
				e.emit(events.Event{Type: events.TypeDeployFailed, Phase: events.PhaseDeploy, Level: events.LevelError, Message: fmt.Sprintf("  ✗ job schedule reconciliation failed: %v\n", err)})
				deploymentFailed = true
				deploymentError = fmt.Errorf("job schedule reconciliation failed: %w", err)
				deployment.Status = remotestate.StatusFailed
				deployment.Error = err.Error()
			}
		}
	}

	if !deploymentFailed {
		if err := s.applyRemovals(plan); err != nil {
			e.emit(events.Event{Type: events.TypeDeployServiceFailed, Phase: events.PhaseDeploy, Level: events.LevelError, Message: fmt.Sprintf("  ✗ service removal failed: %v\n", err)})
			deploymentFailed = true
			deploymentError = err
			deployment.Status = remotestate.StatusFailed
			deployment.Error = err.Error()
		}
	}

	var manualPending []string
	if !deploymentFailed {
		proxyServices := services
		if req.Service != "" {
			proxyServices = CloneServiceMap(s.allServices)
		}
		manualPending = deployplan.ManualPromotionPendingServices(servicesToDeploy, actualState)
		activeRevisions := deployplan.ProxyActiveRevisions(cfg, envName, proxyServices, servicesToDeploy, imageRefs, actualState)
		if err := s.reconcileProxy(proxyServices, activeRevisions); err != nil {
			e.emit(events.Event{Type: events.TypeDeployFailed, Phase: events.PhaseDeploy, Level: events.LevelError, Message: fmt.Sprintf("  ✗ proxy reconciliation failed: %v\n", err)})
			deploymentFailed = true
			deploymentError = fmt.Errorf("proxy reconciliation failed: %w", err)
			deployment.Status = remotestate.StatusFailed
			deployment.Error = err.Error()
		} else if err := s.pruneRevisionsAfterGrace(proxyServices, deployplan.DeployedProxyActiveRevisions(servicesToDeploy, activeRevisions)); err != nil {
			e.emit(events.Event{Type: events.TypeDeployFailed, Phase: events.PhaseDeploy, Level: events.LevelError, Message: fmt.Sprintf("  ✗ stale revision cleanup failed: %v\n", err)})
			deploymentFailed = true
			deploymentError = fmt.Errorf("stale revision cleanup failed: %w", err)
			deployment.Status = remotestate.StatusFailed
			deployment.Error = err.Error()
		} else if len(manualPending) > 0 {
			e.info(events.TypeDeployServiceWarmed, events.PhaseDeploy, fmt.Sprintf("\n✓ Warming revision ready for manual promotion: %s\n  Promote when ready with: tako promote %s -e %s\n", strings.Join(manualPending, ", "), manualPending[0], envName))
		}
	}

	if !deploymentFailed {
		manualPending = deployplan.ManualPromotionPendingServices(servicesToDeploy, actualState)
		deployment.Status = DeploymentSuccessStatus(manualPending)
		if len(manualPending) > 0 {
			deployment.Message = fmt.Sprintf("warmed %s for manual promotion", strings.Join(manualPending, ", "))
		}
		deployment.Duration = time.Since(startTime)
		if err := s.stateManager.SaveDeploymentContext(ctx, deployment); err != nil {
			return nil, RemoteHistoryError(err)
		}

		finalNodeActualState, err := reconcile.GatherActualStateByServer(s.sshPool, cfg, envName, s.mutationServerNames)
		if err != nil {
			return nil, fmt.Errorf("deployment succeeded but failed to gather final actual state: %w", err)
		}
		finalActualState := reconcile.AggregateActualStateByServer(finalNodeActualState)
		runtimeServices := desiredStateServices
		runtimeImageRefs := deployplan.MergeRuntimeImageRefs(cfg, envName, runtimeServices, imageRefs, finalActualState)
		var baseline *takodstate.DesiredRevision
		if req.Service != "" {
			baseline = s.priorDesired
		}
		if err := PersistTakodRuntimeStateWithPlacementBaseline(
			s.sshPool,
			cfg,
			envName,
			serverNames,
			s.sourceInfo.StateSource,
			runtimeServices,
			runtimeImageRefs,
			finalActualState,
			finalNodeActualState,
			s.deployer.ResolvedAssignments(),
			baseline,
			GitInfoFromCommit(s.sourceInfo.CommitInfo),
			"deploy.succeeded",
			fmt.Sprintf("deployed %d service(s)", len(servicesToDeploy)),
			map[string]string{
				"commit":          s.gitStrings.ShortHash,
				"revision":        s.buildTag,
				"services":        fmt.Sprintf("%d", len(servicesToDeploy)),
				"desiredServices": fmt.Sprintf("%d", len(runtimeServices)),
			},
			req.Verbose,
		); err != nil {
			return nil, fmt.Errorf("deployment succeeded but failed to persist takod state: %w", err)
		}
		e.debug(events.TypeStatePersisted, events.PhaseState, "")

		// Replicate state to the rest of the mesh.
		if len(s.servers) > 1 && ShouldReplicateDeploymentHistory(cfg) {
			replicator := remotestate.NewStateReplicator(s.sshPool, cfg, envName, cfg.Project.Name, req.Verbose)
			history, err := s.stateManager.LoadHistoryContext(ctx)
			if err != nil {
				return nil, fmt.Errorf("deployment succeeded but failed to load remote deployment history for replication: %w", err)
			}
			if err := replicator.ReplicateDeploymentContext(ctx, deployment, history); err != nil {
				return nil, fmt.Errorf("deployment succeeded but failed to replicate remote deployment history: %w", err)
			}
			e.debug(events.TypeStateReplicated, events.PhaseState, "")
		}

		// Save local deployment state.
		if s.localStateMgr != nil {
			localDeployment := &localstate.DeploymentState{
				DeploymentID:    fmt.Sprintf("deploy-%s", time.Now().Format("20060102-150405")),
				Timestamp:       startTime,
				Environment:     envName,
				Mode:            cfg.GetRuntimeMode(),
				Servers:         append([]string(nil), serverNames...),
				Status:          string(deployment.Status),
				DurationSeconds: int(time.Since(startTime).Seconds()),
				GitCommit:       s.gitStrings.Hash,
				TriggeredBy:     remotestate.GetCurrentUser(),
				Notes:           fmt.Sprintf("Deployed %d services to %s runtime", len(servicesToDeploy), cfg.GetRuntimeMode()),
			}
			if err := s.localStateMgr.SaveDeployment(localDeployment); err != nil {
				e.debug(events.TypeWarning, events.PhaseState, fmt.Sprintf("Warning: failed to save local deployment state: %v\n", err))
			}
		}
	}

	deploymentDuration := time.Since(startTime)
	result.Duration = deploymentDuration.Seconds()

	if deploymentFailed {
		recordCtx, recordCancel := failedDeploymentRecordContext(ctx)
		recordErr := RecordFailedDeploymentStateContext(recordCtx, s.stateManager, localSaverOrNil(s.localStateMgr), deployment, cfg, envName, serverNames, s.sourceInfo.CommitInfo, startTime, deploymentError)
		if recordErr == nil && len(s.servers) > 1 && ShouldReplicateDeploymentHistory(cfg) {
			replicator := remotestate.NewStateReplicator(s.sshPool, cfg, envName, cfg.Project.Name, req.Verbose)
			history, err := s.stateManager.LoadHistoryContext(recordCtx)
			if err != nil {
				recordErr = fmt.Errorf("failed to load failed deployment history for replication: %w", err)
			} else if err := replicator.ReplicateDeploymentContext(recordCtx, deployment, history); err != nil {
				recordErr = fmt.Errorf("failed to replicate failed deployment history: %w", err)
			}
		}
		recordCancel()
		if recordErr != nil {
			e.debug(events.TypeWarning, events.PhaseState, fmt.Sprintf("Warning: failed to record failed deployment state: %v\n", recordErr))
		}

		// Send failure notification.
		if notifier != nil {
			notifier.Notify(notification.Event{
				Type:        notification.EventDeployFailed,
				Project:     cfg.Project.Name,
				Environment: envName,
				Message:     fmt.Sprintf("Deployment of `%s` to `%s` failed after %s", cfg.Project.Name, envName, deploymentDuration.Round(time.Second)),
				Error:       deploymentError.Error(),
				Duration:    deploymentDuration,
				Details: map[string]string{
					"version":  cfg.Project.Version,
					"commit":   s.gitStrings.ShortHash,
					"revision": s.buildTag,
					"user":     remotestate.GetCurrentUser(),
				},
			})
		}
		result.Status = takoapi.DeploymentStatus(remotestate.StatusFailed)
		result.Error = deploymentError.Error()
		e.emit(events.Event{
			Type:    events.TypeDeployFailed,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelError,
			Message: "",
			Data:    map[string]any{"error": deploymentError.Error()},
		})
		if recordErr != nil {
			return result, fmt.Errorf("takod deployment failed; additionally failed to record failed deployment state: %w", recordErr)
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, fmt.Errorf("takod deployment failed")
	}

	e.info(events.TypeLogLine, events.PhaseCleanup, "\n✓ takod deployment completed!\n")

	// Automatic cleanup after successful deployment.
	e.debug(events.TypeLogLine, events.PhaseCleanup, "\n→ Running automatic cleanup...\n")
	imageRepositories := CleanupImageRepositories(cfg, envName, services)
	externalVolumes := ExternalVolumeNamesForEnvironment(cfg, envName)
	cleanupFactory, factoryErr := nodeclient.NewFactory(cfg, s.sshPool, TakodSocketFromConfig(cfg))
	for serverName := range s.servers {
		if factoryErr != nil {
			e.debug(events.TypeWarning, events.PhaseCleanup, fmt.Sprintf("  Warning: failed to initialize runtime cleanup: %v\n", factoryErr))
			break
		}
		client, _, err := cleanupFactory.Client(ctx, serverName)
		if err == nil {
			response, cleanupErr := CleanupViaTakod(client, cfg, takod.CleanupRequest{
				Project:                cfg.Project.Name,
				Environment:            envName,
				ImageRepositories:      imageRepositories,
				ExternalVolumes:        externalVolumes,
				KeepImages:             3,
				CleanOldImages:         true,
				CleanStoppedContainers: true,
				CleanDanglingImages:    true,
				CleanBuildCache:        true,
				BuildCacheKeepStorage:  takod.DefaultBuildCacheKeepStorage,
			})
			if cleanupErr != nil {
				e.debug(events.TypeWarning, events.PhaseCleanup, fmt.Sprintf("  Warning: failed to clean %s: %v\n", serverName, cleanupErr))
				continue
			}
			if response != nil {
				for _, warning := range response.Warnings {
					e.debug(events.TypeWarning, events.PhaseCleanup, fmt.Sprintf("  Warning: %s\n", warning))
				}
			}
			e.debug(events.TypeCleanupCompleted, events.PhaseCleanup, fmt.Sprintf("  ✓ Cleaned up %s\n", serverName))
		}
	}

	e.info(events.TypeDeploySucceeded, events.PhaseCleanup, "\n✓ Deployment completed successfully!\n\n")

	// Collect deployed service URLs.
	var urls []string
	for _, svc := range services {
		if svc.Proxy != nil && svc.IsPublic() {
			for _, domain := range svc.Proxy.GetAllDomains() {
				urls = append(urls, fmt.Sprintf("https://%s", domain))
			}
		}
	}
	result.URLs = urls

	// Send success notification.
	if notifier != nil {
		notifier.Notify(notification.Event{
			Type:        notification.EventDeploySucceeded,
			Project:     cfg.Project.Name,
			Environment: envName,
			Message:     fmt.Sprintf("Successfully deployed `%s` v%s to `%s` in %s", cfg.Project.Name, cfg.Project.Version, envName, deploymentDuration.Round(time.Second)),
			Duration:    deploymentDuration,
			Details: map[string]string{
				"version":  cfg.Project.Version,
				"commit":   s.gitStrings.ShortHash,
				"revision": s.buildTag,
				"branch":   s.gitStrings.Branch,
				"user":     remotestate.GetCurrentUser(),
				"services": fmt.Sprintf("%d", len(services)),
				"urls":     fmt.Sprintf("%v", urls),
			},
		})
	}

	// Show service URLs (iterate through services with proxy configured).
	hasPublicServices := false
	hasInternalServices := false
	domainSpecs := CollectConfiguredDomainSpecs(services, "")

	var urlText strings.Builder
	for serviceName, service := range services {
		if service.Proxy != nil && service.IsPublic() && service.Proxy.GetPrimaryDomain() != "" {
			allDomains := service.Proxy.GetAllDomains()
			if !hasPublicServices {
				urlText.WriteString("Your application is available at:\n")
				hasPublicServices = true
			}
			urlText.WriteString(fmt.Sprintf("\n%s:\n", serviceName))
			for _, domain := range allDomains {
				urlText.WriteString(fmt.Sprintf("  https://%s\n", domain))
			}
		}
	}
	var internalURLs []string
	for serviceName, service := range services {
		if service.Proxy != nil && service.Proxy.IsInternal() && service.Proxy.GetPrimaryHost() != "" {
			if !hasInternalServices {
				urlText.WriteString("\nInternal routes:\n")
				hasInternalServices = true
			}
			urlText.WriteString(fmt.Sprintf("\n%s:\n", serviceName))
			for _, host := range service.Proxy.GetAllHosts() {
				urlText.WriteString(fmt.Sprintf("  http://%s\n", host))
				internalURLs = append(internalURLs, fmt.Sprintf("http://%s", host))
			}
		}
	}
	if hasInternalServices {
		urlText.WriteString(fmt.Sprintf("\nRun `tako domains hosts -e %s` to print /etc/hosts entries for internal routes.\n", envName))
	}
	if urlText.Len() > 0 {
		e.info(events.TypeLogLine, events.PhaseDomains, urlText.String())
	}
	result.InternalURLs = internalURLs

	if hasPublicServices && !req.SkipDomainCheck {
		targets, err := DomainExpectedTargets(cfg, envName, req.DomainTargets)
		if err != nil {
			if req.StrictDomains {
				return result, fmt.Errorf("failed to resolve domain check targets: %w", err)
			}
			e.debug(events.TypeWarning, events.PhaseDomains, fmt.Sprintf("\n⚠️  Skipping public domain DNS/TLS checks: %v\n", err))
		} else if _, err := e.MonitorDomainStatuses(ctx, health.NewHealthChecker(), domainSpecs, DomainStatusOptions{
			Timeout:         req.DomainTimeout,
			Strict:          req.StrictDomains,
			ExpectedTargets: targets,
		}); err != nil {
			return result, err
		}
	} else if hasPublicServices && req.SkipDomainCheck {
		e.debug(events.TypeLogLine, events.PhaseDomains, "\nSkipping public domain DNS/TLS checks (--skip-domain-check).\n")
	}

	result.Status = takoapi.DeploymentStatus(deployment.Status)
	result.ManualPending = manualPending
	result.Duration = time.Since(startTime).Seconds()
	return result, nil
}

func localSaverOrNil(manager *localstate.Manager) LocalDeploymentSaver {
	if manager == nil {
		return nil
	}
	return manager
}

// ServiceRemover removes services from the runtime during reconciliation.
type ServiceRemover interface {
	RemoveServiceTakod(serviceName string) error
}

func (s *DeploySession) applyRemovals(plan *reconcile.ReconciliationPlan) error {
	return s.engine.ApplyRemovals(s.deployer, plan)
}

// ApplyRemovals removes services the plan marks for removal.
func (e *Engine) ApplyRemovals(remover ServiceRemover, plan *reconcile.ReconciliationPlan) error {
	if remover == nil {
		return fmt.Errorf("service remover is nil")
	}
	if plan == nil {
		return nil
	}
	for _, change := range plan.Changes {
		if change.Type != reconcile.ChangeRemove {
			continue
		}
		e.emit(events.Event{
			Type:    events.TypeDeployServiceStarted,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: change.ServiceName,
			Message: fmt.Sprintf("→ Removing service: %s\n", change.ServiceName),
		})
		if err := remover.RemoveServiceTakod(change.ServiceName); err != nil {
			return fmt.Errorf("remove failed for %s: %w", change.ServiceName, err)
		}
		e.emit(events.Event{
			Type:    events.TypeDeployServiceRemoved,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: change.ServiceName,
			Message: fmt.Sprintf("  ✓ Service %s removed\n", change.ServiceName),
		})
	}
	return nil
}

func removalServiceNames(plan *reconcile.ReconciliationPlan) []string {
	if plan == nil {
		return nil
	}
	var names []string
	for _, change := range plan.Changes {
		if change.Type == reconcile.ChangeRemove {
			names = append(names, change.ServiceName)
		}
	}
	sort.Strings(names)
	return names
}

func removalServiceConfigs(plan *reconcile.ReconciliationPlan) (map[string]config.ServiceConfig, error) {
	pending := make(map[string]config.ServiceConfig)
	if plan == nil {
		return pending, nil
	}
	for _, change := range plan.Changes {
		if change.Type != reconcile.ChangeRemove {
			continue
		}
		if change.OldConfig == nil {
			return nil, invalidRequestf("service %s cannot be removed because its prior configuration is unavailable; restore or explicitly adopt the workload before retrying", change.ServiceName)
		}
		pending[change.ServiceName] = *change.OldConfig
	}
	return pending, nil
}

func rejectPersistentConfigRemovals(plan *reconcile.ReconciliationPlan) error {
	if plan == nil {
		return nil
	}
	for _, change := range plan.Changes {
		if change.Type == reconcile.ChangeNone && change.NewConfig == nil && change.OldConfig != nil && change.OldConfig.Persistent {
			return invalidRequestf("persistent service %s is still running but was removed from config; restore it to tako.yaml so its placement remains authoritative, then use an explicit persistent-workload removal workflow", change.ServiceName)
		}
	}
	return nil
}

// HasJobServices reports whether any service in the map is a kind:job.
func HasJobServices(services map[string]config.ServiceConfig) bool {
	for _, service := range services {
		if service.IsJob() {
			return true
		}
	}
	return false
}

// planRemovesJob reports whether the plan unschedules a job that left the
// config; the declarative jobs-apply pass must still run for that.
func planRemovesJob(plan *reconcile.ReconciliationPlan) bool {
	if plan == nil {
		return false
	}
	for _, change := range plan.Changes {
		if change.Type == reconcile.ChangeRemove && change.OldConfig != nil && change.OldConfig.IsJob() {
			return true
		}
	}
	return false
}

// ProxyReconciler reconciles proxy routes for services.
type ProxyReconciler interface {
	ReconcileTakodProxyWithActiveRevisions(services map[string]config.ServiceConfig, activeRevisions map[string]string) error
	ReconcileTakodProxy(services map[string]config.ServiceConfig) error
}

func (s *DeploySession) reconcileProxy(services map[string]config.ServiceConfig, activeRevisions map[string]string) error {
	return ReconcileProxy(s.deployer, services, activeRevisions)
}

// ReconcileProxy reconciles proxy routes, preferring revision-aware upstreams.
func ReconcileProxy(deploy ProxyReconciler, services map[string]config.ServiceConfig, activeRevisions map[string]string) error {
	if len(activeRevisions) > 0 {
		return deploy.ReconcileTakodProxyWithActiveRevisions(services, activeRevisions)
	}
	return deploy.ReconcileTakodProxy(services)
}

// RevisionPruner prunes stale service revisions.
type RevisionPruner interface {
	PruneTakodServiceRevisions(services map[string]config.ServiceConfig, keepRevisions map[string]string) error
}

func (s *DeploySession) pruneRevisionsAfterGrace(services map[string]config.ServiceConfig, keepRevisions map[string]string) error {
	return s.engine.PruneRevisionsAfterGrace(s.deployer, services, keepRevisions, GraceSleep)
}

// PruneRevisionsAfterGrace prunes stale revisions after the blue-green grace
// period, using the supplied sleep function.
func (e *Engine) PruneRevisionsAfterGrace(pruner RevisionPruner, services map[string]config.ServiceConfig, keepRevisions map[string]string, sleep func(time.Duration)) error {
	if len(keepRevisions) == 0 {
		return nil
	}
	grace, names, err := deployplan.BlueGreenPruneGracePeriod(services, keepRevisions)
	if err != nil {
		return err
	}
	if grace > 0 {
		e.info(events.TypeLogLine, events.PhaseDeploy, fmt.Sprintf("\n-> Retaining previous blue-green revision for %s before pruning: %s\n", grace.Round(time.Millisecond), strings.Join(names, ", ")))
		if sleep == nil {
			sleep = GraceSleep
		}
		sleep(grace)
	}
	return pruner.PruneTakodServiceRevisions(services, keepRevisions)
}
