package engine

import (
	"context"
	"fmt"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

// RunRequest deploys one public image to one existing takod node using a
// synthesized single-service config (the configless `tako run` path).
type RunRequest struct {
	// Config is the synthesized, validated configuration.
	Config      *config.Config
	Environment string
	ServiceName string
	Service     config.ServiceConfig
	ImageRef    string
	// ServerName is the generated config server key; ServerDisplay is the
	// operator-facing SSH host used in progress messages.
	ServerName    string
	ServerDisplay string
	EnvVars       map[string]string
	Verbose       bool
}

// RunSession carries an in-flight configless run between Plan and Apply.
type RunSession struct {
	engine *Engine
	req    RunRequest

	planDoc DeployPlan
	plan    *reconcile.ReconciliationPlan

	sshPool      *ssh.Pool
	runtime      *nodeclient.Factory
	leases       *RemoteLeaseSet
	deployer     *deployer.Deployer
	sourceClient any
	server       config.ServerConfig
	actualState  map[string]*reconcile.ActualService
	services     map[string]config.ServiceConfig
	priorDesired *takodstate.DesiredRevision

	closed  bool
	applied bool
}

// Plan returns the serializable plan document.
func (s *RunSession) Plan() DeployPlan {
	return s.planDoc
}

// NeedsConfirmation reports whether the plan mutates an existing service.
func (s *RunSession) NeedsConfirmation() bool {
	return s.plan.NeedsConfirmation()
}

// Close releases leases and SSH connections. Idempotent.
func (s *RunSession) Close() {
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
}

// RunDeploymentPlan computes the single-service reconciliation plan for a
// configless run.
func RunDeploymentPlan(cfg *config.Config, envName string, serviceName string, service config.ServiceConfig, actualState map[string]*reconcile.ActualService) (map[string]config.ServiceConfig, *reconcile.ReconciliationPlan) {
	services := map[string]config.ServiceConfig{serviceName: service}
	planActualState := deployplan.FilterActualStateForServices(actualState, services)
	return services, reconcile.ComputePlan(cfg.Project.Name, envName, services, planActualState)
}

// PlanRun connects to the target node, sets up the takod runtime, gathers
// running state, and computes the reconciliation plan for a configless run.
func (e *Engine) PlanRun(ctx context.Context, req RunRequest) (*RunSession, error) {
	if req.Config == nil {
		return nil, invalidRequestf("run request requires a synthesized config")
	}
	if req.Environment == "" || req.ServiceName == "" || req.ServerName == "" {
		return nil, invalidRequestf("run request requires environment, service name, and server name")
	}
	cfg := req.Config
	server, exists := cfg.Servers[req.ServerName]
	if !exists {
		return nil, invalidRequestf("server %s not found in configuration", req.ServerName)
	}
	if !server.Schedulable() {
		return nil, invalidRequestf("server %s is %s and cannot receive new assignments", req.ServerName, server.Lifecycle)
	}

	for _, value := range req.EnvVars {
		e.RegisterSecret(value)
	}
	e.RegisterSecret(server.Password)
	e.RegisterRegistrySecrets(req.Config)
	e.RegisterACMEDNSSecrets(req.Config)

	session := &RunSession{
		engine: e,
		req:    req,
		server: server,
	}
	ok := false
	defer func() {
		if !ok {
			session.Close()
		}
	}()

	session.sshPool = ssh.NewPool()
	serverNames := []string{req.ServerName}

	leaseSet, err := AcquireRemoteOperationLeasesContext(ctx, session.sshPool, cfg, req.Environment, serverNames, "run")
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

	runtimeFactory, err := nodeclient.NewFactory(cfg, session.sshPool, TakodSocketFromConfig(cfg))
	if err != nil {
		return nil, err
	}
	session.runtime = runtimeFactory
	sourceClient, _, err := runtimeFactory.Client(ctx, req.ServerName)
	if err != nil {
		display := req.ServerDisplay
		if display == "" {
			display = req.ServerName
		}
		return nil, &ConnectivityError{Server: req.ServerName, Err: fmt.Errorf("failed to connect to server %s: %w", display, err)}
	}
	session.sourceClient = sourceClient

	deploy := deployer.NewDeployerWithPool(sourceClient, cfg, req.Environment, session.sshPool, req.Verbose)
	priorDesired, priorAssignments, err := LoadPriorPlacementState(sourceClient, cfg, req.Environment)
	if err != nil {
		return nil, err
	}
	session.priorDesired = priorDesired
	if err := ValidatePriorDesiredServices(priorDesired, map[string]config.ServiceConfig{req.ServiceName: req.Service}); err != nil {
		return nil, err
	}
	deploy.SetPriorAssignments(priorAssignments)
	if err := deploy.ResolveAllAssignments(map[string]config.ServiceConfig{req.ServiceName: req.Service}); err != nil {
		return nil, err
	}
	deploy.SetRuntimeFactory(runtimeFactory)
	deploy.SetBaseContext(ctx)
	deploy.SetEventSink(e.stream)
	deploy.SetCLIVersion(e.cliVersion)
	deploy.SetSkipBuild(true)
	if output := e.buildOutputWriter(); output != nil {
		deploy.SetOutput(output)
	}
	if err := deploy.SetTargetServers(serverNames); err != nil {
		return nil, err
	}
	if err := preflightAndSetupTakodRuntime(deploy, map[string]config.ServiceConfig{req.ServiceName: req.Service}); err != nil {
		return nil, err
	}
	session.deployer = deploy

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	actualState, err := reconcile.GatherActualStateFromServers(session.sshPool, cfg, req.Environment, serverNames, nil)
	if err != nil {
		return nil, ActualStateError(err)
	}
	session.actualState = actualState

	services, plan := RunDeploymentPlan(cfg, req.Environment, req.ServiceName, req.Service, actualState)
	session.services = services
	session.plan = plan

	planDoc := newDeployPlanDocument(cfg.Project.Name, req.Environment, plan, services)
	planDoc.Revision = cfg.Project.Version
	planDoc.Source = "image"
	planDoc.Servers = serverNames
	planDoc.Services = sortedServiceNames(services)
	planDoc.Destructive = plan.NeedsConfirmation()
	planDoc.Empty = plan.IsEmpty()
	planDoc.HumanText = plan.FormatPlan()
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

// Apply executes the planned configless run. The caller gates confirmation.
func (s *RunSession) Apply(ctx context.Context) (*DeployResult, error) {
	if s.closed {
		return nil, fmt.Errorf("run session is closed")
	}
	if s.applied {
		return nil, fmt.Errorf("run session was already applied")
	}
	s.applied = true
	leaseCtx, cancelLeaseContext := s.leases.BindContext(ctx)
	defer cancelLeaseContext()
	ctx = leaseCtx
	if err := s.leases.Err(); err != nil {
		return nil, err
	}
	s.deployer.SetBaseContext(ctx)
	if err := s.deployer.PreflightTakodProxyCapabilities(s.services); err != nil {
		return nil, err
	}

	e := s.engine
	defer e.flushBuildOutput()
	req := s.req
	cfg := req.Config
	envName := req.Environment
	serverNames := []string{req.ServerName}
	services := s.services
	plan := s.plan
	if len(req.Service.Files) > 0 {
		_, _, filesHash, err := s.deployer.PrepareServiceFiles(req.ServiceName, &req.Service)
		if err != nil {
			return nil, fmt.Errorf("failed to fingerprint operator files for %s: %w", req.ServiceName, err)
		}
		req.Service.FilesContentHash = filesHash
		services[req.ServiceName] = req.Service
	}

	stateManager := remotestate.NewStateManagerWithSocket(s.sourceClient, cfg.Project.Name, envName, s.server.Host, TakodSocketFromConfig(cfg))
	startTime := time.Now()
	deployment := &remotestate.DeploymentState{
		Timestamp:   startTime,
		ProjectName: cfg.Project.Name,
		Version:     cfg.Project.Version,
		Status:      remotestate.StatusInProgress,
		Services: map[string]remotestate.ServiceState{
			req.ServiceName: {
				Name:             req.ServiceName,
				Image:            req.ImageRef,
				SharedBuild:      sharedBuildName(req.Service),
				SharedBuildHash:  req.Service.SharedBuildHash,
				FilesContentHash: req.Service.FilesContentHash,
				Files:            historyServiceFiles(req.Service.Files),
				Port:             req.Service.Port,
				Replicas:         req.Service.Replicas,
				Env:              RedactedEnvKeys(req.EnvVars),
			},
		},
		User:       remotestate.GetCurrentUser(),
		Host:       s.server.Host,
		Message:    "deployed image",
		CLIVersion: e.cliVersion,
		CLICommit:  e.cliCommit,
	}
	imageRefs := map[string]string{req.ServiceName: req.ImageRef}

	result := &DeployResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindDeployResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Revision:    cfg.Project.Version,
		PlanHash:    s.planDoc.Hash(),
		StartedAt:   startTime,
	}
	if !plan.IsEmpty() {
		if err := s.deployer.PreflightAssignmentMutations(services); err != nil {
			return nil, err
		}
	}
	if err := PersistTakodDesiredIntentWithPlacementBaseline(s.sshPool, cfg, envName, serverNames, "image", services, imageRefs, s.deployer.ResolvedAssignments(), nil, s.priorDesired, takodstate.GitInfo{}, "recorded stable placement before direct image mutation", req.Verbose); err != nil {
		return nil, err
	}

	if err := RecordStartedDeploymentStateContext(ctx, stateManager, deployment); err != nil {
		return nil, fmt.Errorf("failed to record started deployment state before applying mutations: %w", err)
	}
	e.debug(events.TypeLogLine, events.PhaseState, fmt.Sprintf("→ Recorded in-progress deployment state (%s)\n", deployment.ID))

	recordFailure := func(deployErr error) (*DeployResult, error) {
		recordCtx, recordCancel := failedDeploymentRecordContext(ctx)
		recordErr := RecordFailedDeploymentStateContext(recordCtx, stateManager, nil, deployment, cfg, envName, serverNames, nil, startTime, deployErr)
		recordCancel()
		if recordErr != nil {
			e.emit(events.Event{
				Type:    events.TypeWarning,
				Phase:   events.PhaseState,
				Level:   events.LevelWarn,
				Message: fmt.Sprintf("Warning: failed to record failed deployment state: %v\n", recordErr),
				Data:    map[string]any{"stream": "stderr"},
			})
		}
		result.Status = takoapi.DeploymentStatus(remotestate.StatusFailed)
		result.Error = deployErr.Error()
		result.Duration = time.Since(startTime).Seconds()
		return result, deployErr
	}

	if plan.IsEmpty() {
		e.info(events.TypePlanUpToDate, events.PhaseDeploy, fmt.Sprintf("%s is up-to-date on %s; reconciling proxy and state...\n", req.ServiceName, req.ServerDisplay))
		activeRevisions := deployplan.ProxyActiveRevisions(cfg, envName, services, nil, nil, s.actualState)
		if err := ReconcileProxy(s.deployer, services, activeRevisions); err != nil {
			return recordFailure(fmt.Errorf("proxy reconciliation failed: %w", err))
		}
	} else {
		e.emit(events.Event{
			Type:    events.TypeDeployServiceStarted,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: req.ServiceName,
			Message: fmt.Sprintf("Deploying %s as %s to %s...\n", req.ImageRef, req.ServiceName, req.ServerDisplay),
			Data:    map[string]any{"image": req.ImageRef},
		})
		if err := s.deployer.DeployServiceTakod(req.ServiceName, &req.Service, req.ImageRef); err != nil {
			return recordFailure(fmt.Errorf("takod deployment failed for %s: %w", req.ServiceName, err))
		}
		activeRevisions := deployplan.ProxyActiveRevisions(cfg, envName, services, services, imageRefs, s.actualState)
		if err := ReconcileProxy(s.deployer, services, activeRevisions); err != nil {
			return recordFailure(fmt.Errorf("proxy reconciliation failed: %w", err))
		}
	}

	postNodeActualState, err := reconcile.GatherActualStateByServer(s.sshPool, cfg, envName, serverNames)
	if err != nil {
		return recordFailure(fmt.Errorf("deployment succeeded but failed to gather final actual state: %w", err))
	}
	postActualState := reconcile.AggregateActualStateByServer(postNodeActualState)

	if err := PersistTakodRuntimeStateWithPlacementBaseline(
		s.sshPool,
		cfg,
		envName,
		serverNames,
		"image",
		services,
		imageRefs,
		postActualState,
		postNodeActualState,
		s.deployer.ResolvedAssignments(),
		s.priorDesired,
		takodstate.GitInfo{},
		"run.succeeded",
		fmt.Sprintf("deployed image %s", req.ImageRef),
		map[string]string{
			"image":    req.ImageRef,
			"service":  req.ServiceName,
			"replicas": fmt.Sprintf("%d", req.Service.Replicas),
		},
		req.Verbose,
	); err != nil {
		return recordFailure(fmt.Errorf("deployment succeeded but failed to persist takod state: %w", err))
	}

	deployment.Status = remotestate.StatusSuccess
	deployment.Duration = time.Since(startTime)
	if plan.IsEmpty() {
		deployment.Message = "service up-to-date; proxy and state reconciled"
	} else {
		deployment.Message = "deployed image"
	}
	if err := stateManager.SaveDeploymentContext(ctx, deployment); err != nil {
		return nil, RemoteHistoryError(err)
	}

	action := OutcomeDeployed
	if plan.IsEmpty() {
		action = OutcomeUpToDate
		e.info(events.TypeDeploySucceeded, events.PhaseDeploy, fmt.Sprintf("✓ %s is up-to-date. Proxy routes and takod state reconciled (%s).\n", req.ServiceName, envName))
	} else {
		e.info(events.TypeDeploySucceeded, events.PhaseDeploy, fmt.Sprintf("✓ Deployed %s as %s to %s (%s)\n", req.ImageRef, req.ServiceName, req.ServerDisplay, envName))
	}
	if req.Service.Proxy != nil && req.Service.Proxy.GetPrimaryDomain() != "" {
		url := fmt.Sprintf("https://%s", req.Service.Proxy.GetPrimaryDomain())
		e.info(events.TypeLogLine, events.PhaseDomains, fmt.Sprintf("URL: %s\n", url))
		result.URLs = []string{url}
	}

	result.Status = takoapi.DeploymentStatus(deployment.Status)
	result.Message = deployment.Message
	result.Services = []ServiceOutcome{{
		Name:     req.ServiceName,
		Image:    req.ImageRef,
		Action:   action,
		Replicas: req.Service.Replicas,
	}}
	result.Duration = time.Since(startTime).Seconds()
	return result, nil
}
