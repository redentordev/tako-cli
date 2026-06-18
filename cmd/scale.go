package cmd

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/notification"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takodstate"
	"github.com/spf13/cobra"
)

var scaleCmd = &cobra.Command{
	Use:   "scale SERVICE=REPLICAS [SERVICE=REPLICAS...]",
	Short: "Scale takod services to specified replicas",
	Long: `Scale one or more services to a specified number of replicas.

This command reconciles running takod containers without rebuilding images.

Examples:
  tako scale web=5
  tako scale api=3 web=2
  tako scale worker=10
  tako scale web=0

Note: this changes runtime state immediately. Update replicas in tako.yaml if
you want the next full deploy to preserve the same count.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runScale,
}

func init() {
	rootCmd.AddCommand(scaleCmd)
}

func runScale(cmd *cobra.Command, args []string) error {
	return runScaleTargets(cmd, args)
}

func runScaleTargets(cmd *cobra.Command, args []string) error {
	scaleTargets := make(map[string]int)
	for _, arg := range args {
		parts := strings.Split(arg, "=")
		if len(parts) != 2 {
			return fmt.Errorf("invalid format '%s': expected SERVICE=REPLICAS", arg)
		}

		service := strings.TrimSpace(parts[0])
		replicas, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return fmt.Errorf("invalid replica count for %s: %w", service, err)
		}
		if replicas < 0 {
			return fmt.Errorf("replica count cannot be negative for %s", service)
		}

		scaleTargets[service] = replicas
	}

	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}
	for serviceName := range scaleTargets {
		if _, exists := services[serviceName]; !exists {
			return fmt.Errorf("service '%s' not found in environment %s", serviceName, envName)
		}
	}

	serverNames, err := scaleTargetServers(cfg, envName)
	if err != nil {
		return err
	}
	if len(serverNames) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}

	fmt.Printf("Scaling %d service(s) on %d takod node(s)...\n\n", len(scaleTargets), len(serverNames))

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, serverNames, "scale")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Printf("→ Acquired remote scale leases: %s\n", leaseSet.Summary())
	}

	sourceServerName := serverNames[0]
	sourceServer, err := serverConfigByName(cfg, sourceServerName)
	if err != nil {
		return err
	}
	sourceClient, err := sshPool.GetOrCreateWithAuth(sourceServer.Host, sourceServer.Port, sourceServer.User, sourceServer.SSHKey, sourceServer.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to node %s: %w", sourceServerName, err)
	}

	deploy := deployer.NewDeployerWithPool(sourceClient, cfg, envName, sshPool, verbose)
	deploy.SetCLIVersion(Version)
	if err := deploy.SetTargetServers(serverNames); err != nil {
		return err
	}
	if err := deploy.SetupTakodRuntime(); err != nil {
		return fmt.Errorf("failed to setup takod runtime: %w", err)
	}
	startTime := time.Now()

	actualState, err := reconcile.GatherActualStateFromServers(sshPool, cfg, envName, serverNames, nil)
	if err != nil {
		return fmt.Errorf("failed to gather current replica state: %w", err)
	}

	notifier := scaleNotifier(cfg)
	desiredServices := cloneServiceMap(services)
	scaledImageRefs := make(map[string]string, len(scaleTargets))
	totalErrors := 0
	for serviceName, desiredReplicas := range scaleTargets {
		currentReplicas := 0
		if actual, ok := actualState[serviceName]; ok {
			currentReplicas = actual.Replicas
		}

		fmt.Printf("-> Scaling %s: %d -> %d replicas\n", serviceName, currentReplicas, desiredReplicas)

		service := services[serviceName]
		service.Replicas = desiredReplicas

		imageRef := service.Image
		if imageRef == "" {
			if actual, ok := actualState[serviceName]; ok && actual.Image != "" {
				imageRef = actual.Image
			} else {
				imageRef = cfg.GetFullImageName(serviceName, envName)
			}
		}

		if err := deploy.DeployServiceTakod(serviceName, &service, imageRef); err != nil {
			fmt.Printf("  Failed: %v\n", err)
			totalErrors++
			continue
		}
		desiredServices[serviceName] = service
		scaledImageRefs[serviceName] = imageRef

		fmt.Printf("  ✓ Service %s scaled\n", serviceName)
		if notifier != nil && currentReplicas != desiredReplicas {
			notifier.Notify(notification.ScaleEvent(cfg.Project.Name, envName, serviceName, currentReplicas, desiredReplicas))
		}
	}

	if totalErrors > 0 {
		return fmt.Errorf("scaling completed with %d error(s)", totalErrors)
	}

	if err := deploy.ReconcileTakodProxy(desiredServices); err != nil {
		return fmt.Errorf("scale succeeded but failed to reconcile proxy: %w", err)
	}

	postScaleNodeActualState, err := reconcile.GatherActualStateByServer(sshPool, cfg, envName, serverNames)
	if err != nil {
		return fmt.Errorf("scale succeeded but failed to gather post-scale actual state: %w", err)
	}
	postScaleActualState := reconcile.AggregateActualStateByServer(postScaleNodeActualState)
	runtimeImageRefs := mergeRuntimeImageRefs(cfg, envName, desiredServices, scaledImageRefs, postScaleActualState)
	if err := persistTakodRuntimeState(
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
	); err != nil {
		return fmt.Errorf("scale succeeded but failed to persist takod state: %w", err)
	}

	scaleDuration := time.Since(startTime)
	scaleDeployment := buildScaleDeploymentState(cfg, envName, sourceServer.Host, startTime, scaleDuration, scaleTargets, desiredServices, scaledImageRefs)
	if err := recordScaleDeploymentState(sshPool, sourceClient, cfg, envName, serverNames, scaleDeployment); err != nil {
		return fmt.Errorf("scale succeeded but failed to record deployment history: %w", err)
	}

	fmt.Println("\nAll services scaled successfully.")
	return nil
}

func scaleEventDetails(targets map[string]int) map[string]string {
	details := make(map[string]string, len(targets))
	for serviceName, replicas := range targets {
		details[serviceName] = fmt.Sprintf("%d", replicas)
	}
	return details
}

func buildScaleDeploymentState(
	cfg *config.Config,
	envName string,
	host string,
	startTime time.Time,
	duration time.Duration,
	scaleTargets map[string]int,
	services map[string]config.ServiceConfig,
	imageRefs map[string]string,
) *remotestate.DeploymentState {
	serviceStates := make(map[string]remotestate.ServiceState, len(scaleTargets))
	for _, serviceName := range sortedScaleTargetNames(scaleTargets) {
		service := services[serviceName]
		serviceStates[serviceName] = remotestate.ServiceState{
			Name:     serviceName,
			Image:    imageRefs[serviceName],
			Port:     service.Port,
			Replicas: service.Replicas,
			Env:      redactedEnvKeys(service.Env),
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
		Message:     fmt.Sprintf("scaled %s", scaleTargetSummary(scaleTargets)),
		CLIVersion:  Version,
		CLICommit:   GitCommit,
	}
}

func recordScaleDeploymentState(
	sshPool *ssh.Pool,
	sourceClient *ssh.Client,
	cfg *config.Config,
	envName string,
	serverNames []string,
	deployment *remotestate.DeploymentState,
) error {
	stateManager := remotestate.NewStateManagerWithSocket(sourceClient, cfg.Project.Name, envName, deployment.Host, takodSocketFromConfig(cfg))
	if err := stateManager.SaveDeployment(deployment); err != nil {
		return fmt.Errorf("failed to save remote scale history: %w", err)
	}

	if len(serverNames) > 1 {
		replicator := remotestate.NewStateReplicator(sshPool, cfg, envName, cfg.Project.Name, verbose)
		history, err := stateManager.LoadHistory()
		if err != nil {
			return fmt.Errorf("failed to load scale history for replication: %w", err)
		}
		if err := replicator.ReplicateDeployment(deployment, history); err != nil {
			return fmt.Errorf("failed to replicate scale history: %w", err)
		}
	}

	localMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to initialize local state for scale: %v\n", err)
		}
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
	if err := localMgr.SaveDeployment(localDeployment); err != nil && verbose {
		fmt.Printf("Warning: failed to save local scale state: %v\n", err)
	}
	return nil
}

func scaleTargetSummary(targets map[string]int) string {
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

func scaleTargetServers(cfg *config.Config, envName string) ([]string, error) {
	serverNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, err
	}
	sort.Strings(serverNames)
	return serverNames, nil
}

func scaleNotifier(cfg *config.Config) *notification.Notifier {
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
