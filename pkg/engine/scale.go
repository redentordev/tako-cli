package engine

import (
	"context"
	"fmt"
	"sort"
	"strconv"
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

// KindScaleResult identifies a serialized scale result document.
const KindScaleResult = "ScaleResult"

// ScaleRequest describes one scale operation. Config must be loaded and
// validated; Environment must be resolved.
type ScaleRequest struct {
	Config      *config.Config
	Environment string
	// Targets maps service name to the desired replica count, as parsed from
	// SERVICE=REPLICAS arguments (see ParseScaleTargets).
	Targets map[string]int
	// Verbose enables detailed progress from the deployer and state
	// replicator; debug-level events are emitted regardless and filtered by
	// renderers.
	Verbose bool
}

// ScaleResult is the serializable outcome of Scale.
type ScaleResult struct {
	APIVersion  string                   `json:"apiVersion"`
	Kind        string                   `json:"kind"`
	Project     string                   `json:"project"`
	Environment string                   `json:"environment"`
	Status      takoapi.DeploymentStatus `json:"status"`
	Services    []ServiceOutcome         `json:"services"`
	StartedAt   time.Time                `json:"startedAt"`
	Duration    float64                  `json:"durationSeconds"`
	Message     string                   `json:"message,omitempty"`
	Error       string                   `json:"error,omitempty"`
}

// ParseScaleTargets parses SERVICE=REPLICAS arguments into a target map.
func ParseScaleTargets(args []string) (map[string]int, error) {
	scaleTargets := make(map[string]int)
	for _, arg := range args {
		parts := strings.Split(arg, "=")
		if len(parts) != 2 {
			return nil, invalidRequestf("invalid format '%s': expected SERVICE=REPLICAS", arg)
		}

		service := strings.TrimSpace(parts[0])
		replicas, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, invalidRequestf("invalid replica count for %s: %w", service, err)
		}
		if replicas < 0 {
			return nil, invalidRequestf("replica count cannot be negative for %s", service)
		}

		scaleTargets[service] = replicas
	}
	return scaleTargets, nil
}

// Scale reconciles running takod services to the requested replica counts
// without rebuilding images, then persists runtime state and deployment
// history. Scale has no confirmation step, so it runs as a single call.
func (e *Engine) Scale(ctx context.Context, req ScaleRequest) (*ScaleResult, error) {
	if req.Config == nil {
		return nil, invalidRequestf("scale request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("scale request requires an environment")
	}
	if len(req.Targets) == 0 {
		return nil, invalidRequestf("scale request requires at least one SERVICE=REPLICAS target")
	}
	cfg := req.Config
	envName := req.Environment
	scaleTargets := req.Targets

	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get services: %w", err)
	}
	for serviceName := range scaleTargets {
		service, exists := services[serviceName]
		if !exists {
			return nil, invalidRequestf("service '%s' not found in environment %s", serviceName, envName)
		}
		if service.IsRun() || service.IsJob() {
			return nil, invalidRequestf("service %s is kind: %s and cannot be scaled", serviceName, service.Kind)
		}
	}

	serverNames, err := ScaleTargetServers(cfg, envName)
	if err != nil {
		return nil, err
	}
	if len(serverNames) == 0 {
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

	e.info(events.TypeDeployStarted, events.PhaseDeploy, fmt.Sprintf("Scaling %d service(s) on %d takod node(s)...\n\n", len(scaleTargets), len(serverNames)))

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	leaseSet, err := AcquireRemoteOperationLeasesContext(ctx, sshPool, cfg, envName, serverNames, "scale")
	if err != nil {
		return nil, err
	}
	leaseSet.SetWarnFunc(func(message string) {
		e.debug(events.TypeWarning, events.PhaseDeploy, message)
	})
	defer leaseSet.Release()
	leaseCtx, cancelLeaseContext := leaseSet.BindContext(ctx)
	defer cancelLeaseContext()
	ctx = leaseCtx
	e.debug(events.TypeLogLine, events.PhaseDeploy, fmt.Sprintf("→ Acquired remote scale leases: %s\n", leaseSet.Summary()))

	sourceServerName := serverNames[0]
	sourceServer, exists := cfg.Servers[sourceServerName]
	if !exists {
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
	if err := deploy.PreflightTakodProxyCapabilities(services); err != nil {
		return nil, err
	}
	startTime := time.Now()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	result := &ScaleResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindScaleResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		StartedAt:   startTime,
	}

	actualState, err := reconcile.GatherActualStateFromServers(sshPool, cfg, envName, serverNames, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to gather current replica state: %w", err)
	}
	for serviceName, desiredReplicas := range scaleTargets {
		service := services[serviceName]
		service.Replicas = desiredReplicas
		if service.SharedBuildHash == "" {
			continue
		}
		actual := actualState[serviceName]
		if actual == nil || actual.Image == "" {
			return nil, fmt.Errorf("shared-build service %s has no deployed image to scale; run a deploy first", serviceName)
		}
		if err := deploy.EnsurePreparedServiceImage(serviceName, &service, actual.Image); err != nil {
			return nil, err
		}
	}

	notifier := scaleNotifier(cfg, req.Verbose)
	desiredServices := CloneServiceMap(services)
	scaledImageRefs := make(map[string]string, len(scaleTargets))
	totalErrors := 0
	for serviceName, desiredReplicas := range scaleTargets {
		currentReplicas := 0
		if actual, ok := actualState[serviceName]; ok {
			currentReplicas = actual.Replicas
		}

		e.emit(events.Event{
			Type:    events.TypeDeployServiceStarted,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: serviceName,
			Message: fmt.Sprintf("-> Scaling %s: %d -> %d replicas\n", serviceName, currentReplicas, desiredReplicas),
			Data:    map[string]any{"fromReplicas": currentReplicas, "toReplicas": desiredReplicas},
		})

		service := services[serviceName]
		service.Replicas = desiredReplicas

		imageRef := service.Image
		if imageRef == "" {
			if actual, ok := actualState[serviceName]; ok && actual.Image != "" {
				imageRef = actual.Image
			} else {
				imageRef = deployplan.ImageRef(cfg, envName, serviceName, service, "")
			}
		}

		var deployErr error
		if service.SharedBuildHash != "" {
			deployErr = deploy.DeployPreparedServiceTakod(serviceName, &service, imageRef, false)
		} else {
			deployErr = deploy.DeployServiceTakod(serviceName, &service, imageRef)
		}
		if deployErr != nil {
			e.emit(events.Event{
				Type:    events.TypeDeployServiceFailed,
				Phase:   events.PhaseDeploy,
				Level:   events.LevelError,
				Service: serviceName,
				Message: fmt.Sprintf("  Failed: %v\n", deployErr),
			})
			result.Services = append(result.Services, ServiceOutcome{Name: serviceName, Image: imageRef, Action: OutcomeFailed, Replicas: desiredReplicas, Error: deployErr.Error()})
			totalErrors++
			continue
		}
		desiredServices[serviceName] = service
		scaledImageRefs[serviceName] = imageRef

		e.emit(events.Event{
			Type:    events.TypeDeployServiceReconciled,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: serviceName,
			Message: fmt.Sprintf("  ✓ Service %s scaled\n", serviceName),
			Data:    map[string]any{"image": imageRef, "replicas": desiredReplicas},
		})
		result.Services = append(result.Services, ServiceOutcome{Name: serviceName, Image: imageRef, Action: OutcomeDeployed, Replicas: desiredReplicas})
		if notifier != nil && currentReplicas != desiredReplicas {
			notifier.Notify(notification.ScaleEvent(cfg.Project.Name, envName, serviceName, currentReplicas, desiredReplicas))
		}
	}

	if totalErrors > 0 {
		scaleErr := fmt.Errorf("scaling completed with %d error(s)", totalErrors)
		result.Status = takoapi.DeploymentStatus(remotestate.StatusFailed)
		result.Error = scaleErr.Error()
		result.Duration = time.Since(startTime).Seconds()
		return result, scaleErr
	}

	if err := deploy.ReconcileTakodProxy(desiredServices); err != nil {
		return nil, fmt.Errorf("scale succeeded but failed to reconcile proxy: %w", err)
	}

	postScaleNodeActualState, err := reconcile.GatherActualStateByServer(sshPool, cfg, envName, serverNames)
	if err != nil {
		return nil, fmt.Errorf("scale succeeded but failed to gather post-scale actual state: %w", err)
	}
	postScaleActualState := reconcile.AggregateActualStateByServer(postScaleNodeActualState)
	runtimeImageRefs := deployplan.MergeRuntimeImageRefs(cfg, envName, desiredServices, scaledImageRefs, postScaleActualState)
	if err := PersistTakodRuntimeState(
		sshPool,
		cfg,
		envName,
		serverNames,
		"scale",
		desiredServices,
		runtimeImageRefs,
		postScaleActualState,
		postScaleNodeActualState,
		takodstate.GitInfo{},
		"scale.succeeded",
		fmt.Sprintf("scaled %d service(s)", len(scaleTargets)),
		scaleEventDetails(scaleTargets),
		req.Verbose,
	); err != nil {
		return nil, fmt.Errorf("scale succeeded but failed to persist takod state: %w", err)
	}
	e.debug(events.TypeStatePersisted, events.PhaseState, "")

	scaleDuration := time.Since(startTime)
	scaleDeployment := BuildScaleDeploymentState(cfg, envName, sourceServer.Host, startTime, scaleDuration, scaleTargets, desiredServices, scaledImageRefs, e.cliVersion, e.cliCommit)
	if err := e.recordScaleDeploymentState(ctx, sshPool, sourceClient, cfg, envName, serverNames, scaleDeployment, req.Verbose); err != nil {
		return nil, fmt.Errorf("scale succeeded but failed to record deployment history: %w", err)
	}

	e.info(events.TypeDeploySucceeded, events.PhaseDeploy, "\nAll services scaled successfully.\n")

	result.Status = takoapi.DeploymentStatus(remotestate.StatusSuccess)
	result.Message = scaleDeployment.Message
	result.Duration = time.Since(startTime).Seconds()
	return result, nil
}

func scaleEventDetails(targets map[string]int) map[string]string {
	details := make(map[string]string, len(targets))
	for serviceName, replicas := range targets {
		details[serviceName] = fmt.Sprintf("%d", replicas)
	}
	return details
}

// BuildScaleDeploymentState builds the deployment history record for a
// successful scale operation.
func BuildScaleDeploymentState(
	cfg *config.Config,
	envName string,
	host string,
	startTime time.Time,
	duration time.Duration,
	scaleTargets map[string]int,
	services map[string]config.ServiceConfig,
	imageRefs map[string]string,
	cliVersion string,
	cliCommit string,
) *remotestate.DeploymentState {
	serviceStates := make(map[string]remotestate.ServiceState, len(scaleTargets))
	for _, serviceName := range sortedScaleTargetNames(scaleTargets) {
		service := services[serviceName]
		serviceStates[serviceName] = remotestate.ServiceState{
			Name:             serviceName,
			Image:            imageRefs[serviceName],
			SharedBuild:      sharedBuildName(service),
			SharedBuildHash:  service.SharedBuildHash,
			FilesContentHash: service.FilesContentHash,
			Files:            historyServiceFiles(service.Files),
			Port:             service.Port,
			Replicas:         service.Replicas,
			Env:              RedactedEnvKeys(service.Env),
		}
	}

	return &remotestate.DeploymentState{
		Timestamp:   startTime,
		ProjectName: cfg.Project.Name,
		Environment: envName,
		Version:     cfg.Project.Version,
		Status:      remotestate.StatusSuccess,
		Services:    serviceStates,
		User:        remotestate.GetCurrentUser(),
		Host:        host,
		Duration:    duration,
		Message:     fmt.Sprintf("scaled %s", ScaleTargetSummary(scaleTargets)),
		CLIVersion:  cliVersion,
		CLICommit:   cliCommit,
	}
}

func (e *Engine) recordScaleDeploymentState(
	ctx context.Context,
	sshPool *ssh.Pool,
	sourceClient *ssh.Client,
	cfg *config.Config,
	envName string,
	serverNames []string,
	deployment *remotestate.DeploymentState,
	verbose bool,
) error {
	stateManager := remotestate.NewStateManagerWithSocket(sourceClient, cfg.Project.Name, envName, deployment.Host, TakodSocketFromConfig(cfg))
	if err := stateManager.SaveDeploymentContext(ctx, deployment); err != nil {
		return fmt.Errorf("failed to save remote scale history: %w", err)
	}

	if len(serverNames) > 1 {
		replicator := remotestate.NewStateReplicator(sshPool, cfg, envName, cfg.Project.Name, verbose)
		history, err := stateManager.LoadHistoryContext(ctx)
		if err != nil {
			return fmt.Errorf("failed to load scale history for replication: %w", err)
		}
		if err := replicator.ReplicateDeploymentContext(ctx, deployment, history); err != nil {
			return fmt.Errorf("failed to replicate scale history: %w", err)
		}
	}

	localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		e.debug(events.TypeWarning, events.PhaseState, fmt.Sprintf("Warning: failed to initialize local state for scale: %v\n", err))
		return nil
	}
	localDeployment := &localstate.DeploymentState{
		DeploymentID:    fmt.Sprintf("scale-%s", deployment.Timestamp.Format("20060102-150405")),
		Timestamp:       deployment.Timestamp,
		Environment:     envName,
		Mode:            cfg.GetRuntimeMode(),
		Servers:         append([]string(nil), serverNames...),
		Status:          "success",
		DurationSeconds: int(deployment.Duration.Seconds()),
		TriggeredBy:     remotestate.GetCurrentUser(),
		Notes:           deployment.Message,
	}
	if err := localMgr.SaveDeployment(localDeployment); err != nil {
		e.debug(events.TypeWarning, events.PhaseState, fmt.Sprintf("Warning: failed to save local scale state: %v\n", err))
	}
	return nil
}

// ScaleTargetSummary renders scale targets as a deterministic
// "service=replicas" list.
func ScaleTargetSummary(targets map[string]int) string {
	names := sortedScaleTargetNames(targets)
	parts := make([]string, 0, len(names))
	for _, serviceName := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", serviceName, targets[serviceName]))
	}
	return strings.Join(parts, ", ")
}

func sortedScaleTargetNames(targets map[string]int) []string {
	names := make([]string, 0, len(targets))
	for serviceName := range targets {
		names = append(names, serviceName)
	}
	sort.Strings(names)
	return names
}

// ScaleTargetServers lists the environment's takod nodes in sorted order.
func ScaleTargetServers(cfg *config.Config, envName string) ([]string, error) {
	serverNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, err
	}
	sort.Strings(serverNames)
	return serverNames, nil
}

func scaleNotifier(cfg *config.Config, verbose bool) *notification.Notifier {
	if cfg.Notifications == nil {
		return nil
	}
	if cfg.Notifications.Slack == "" && cfg.Notifications.Discord == "" && cfg.Notifications.Webhook == "" {
		return nil
	}
	return notification.NewNotifier(notification.NotifierConfig{
		SlackWebhook:   cfg.Notifications.Slack,
		DiscordWebhook: cfg.Notifications.Discord,
		Webhook:        cfg.Notifications.Webhook,
	}, verbose)
}
