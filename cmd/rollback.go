package cmd

import (
	"fmt"
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

var (
	rollbackService string
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback [deployment-id]",
	Short: "Rollback to previous deployment",
	Long: `Rollback to a previous deployment.

If no deployment-id is provided, rolls back to the most recent successful deployment.
Specify a deployment-id to rollback to a specific deployment.

Rollback reads the freshest reachable deployment history from the mesh.

Use 'tako history' to view available deployments.

Examples:
  tako rollback --service web                 # Rollback to previous deployment
  tako rollback --service web deploy-123      # Rollback to specific deployment`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRollback,
}

func init() {
	rootCmd.AddCommand(rollbackCmd)
	rollbackCmd.Flags().StringVar(&rollbackService, "service", "", "Service to rollback (required)")
	rollbackCmd.MarkFlagRequired("service")
}

func runRollback(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	// Acquire state lock to prevent concurrent operations
	stateLock := localstate.NewStateLock(".tako")
	lockInfo, err := stateLock.Acquire("rollback")
	if err != nil {
		return fmt.Errorf("cannot rollback: %w", err)
	}
	defer stateLock.Release(lockInfo)

	// Get environment and services
	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	// Check service exists in environment
	if _, exists := services[rollbackService]; !exists {
		return fmt.Errorf("service %s not found in environment %s", rollbackService, envName)
	}

	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(envServers) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, envServers, "rollback")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Printf("→ Acquired remote rollback leases: %s\n", leaseSet.Summary())
	}

	historySource, err := selectRollbackHistorySource(cfg, envName, "")
	if err != nil {
		return err
	}
	serverName := historySource.source
	server, err := serverConfigByName(cfg, serverName)
	if err != nil {
		return err
	}
	if verbose {
		fmt.Printf("Reading rollback state from node: %s (%s)\n", serverName, server.Host)
	}

	client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to node %s: %w", serverName, err)
	}

	// Create state manager
	stateManager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, takodSocketFromConfig(cfg))

	// Determine which deployment to rollback to
	if len(args) > 0 {
		fmt.Printf("\n=== Rolling back to deployment: %s ===\n\n", args[0])
	} else {
		fmt.Printf("\n=== Rolling back to previous successful deployment ===\n\n")
	}
	targetDeployment, err := selectRollbackTargetFromHistory(historySource.history, firstArg(args), rollbackService)
	if err != nil {
		return err
	}

	// Display deployment info
	fmt.Printf("Target deployment:\n")
	fmt.Printf("  ID:        %s\n", targetDeployment.ID)
	fmt.Printf("  Timestamp: %s\n", targetDeployment.Timestamp.Format("2006-01-02 15:04:05"))
	fmt.Printf("  Version:   %s\n", targetDeployment.Version)
	fmt.Printf("  User:      %s\n", targetDeployment.User)
	fmt.Printf("\n")

	// Setup notifications if configured
	var notifier *notification.Notifier
	if cfg.Notifications != nil && (cfg.Notifications.Slack != "" || cfg.Notifications.Discord != "" || cfg.Notifications.Webhook != "") {
		notifier = notification.NewNotifier(notification.NotifierConfig{
			SlackWebhook:   cfg.Notifications.Slack,
			DiscordWebhook: cfg.Notifications.Discord,
			Webhook:        cfg.Notifications.Webhook,
		}, verbose)

		// Send rollback started notification
		notifier.Notify(notification.Event{
			Type:        notification.EventRollbackStarted,
			Project:     cfg.Project.Name,
			Environment: envName,
			Service:     rollbackService,
			Message:     fmt.Sprintf("Rolling back `%s` to deployment `%s` (version %s)", rollbackService, targetDeployment.ID, targetDeployment.Version),
			Details: map[string]string{
				"deployment_id": targetDeployment.ID,
				"version":       targetDeployment.Version,
				"user":          remotestate.GetCurrentUser(),
			},
		})
	}

	startTime := time.Now()

	deploy := deployer.NewDeployerWithPool(client, cfg, envName, sshPool, verbose)
	deploy.SetCLIVersion(Version)
	if err := deploy.SetTargetServers(envServers); err != nil {
		return err
	}
	if err := deploy.SetupTakodRuntime(); err != nil {
		return fmt.Errorf("failed to setup takod runtime: %w", err)
	}

	// Perform rollback using state
	serviceState := targetDeployment.Services[rollbackService]
	if err := deploy.RollbackToState(rollbackService, &serviceState); err != nil {
		// Send failure notification
		if notifier != nil {
			notifier.Notify(notification.Event{
				Type:        notification.EventDeployFailed,
				Project:     cfg.Project.Name,
				Environment: envName,
				Service:     rollbackService,
				Message:     fmt.Sprintf("Rollback of `%s` failed", rollbackService),
				Error:       err.Error(),
			})
		}
		return fmt.Errorf("rollback failed: %w", err)
	}

	rollbackDuration := time.Since(startTime)

	rollbackDeployment := buildRollbackDeployment(cfg, envName, server.Host, startTime, rollbackDuration, targetDeployment, rollbackService, serviceState)
	if err := stateManager.SaveDeployment(rollbackDeployment); err != nil {
		return rollbackRemoteHistoryError(err)
	}

	desiredServices := cloneServiceMap(services)
	rollbackConfig := desiredServices[rollbackService]
	rollbackConfig.Image = serviceState.Image
	rollbackConfig.Replicas = serviceState.Replicas
	if len(serviceState.Ports) > 0 {
		rollbackConfig.Port = 0
		rollbackConfig.Proxy = nil
		rollbackConfig.Ports = serviceState.Ports
	} else if serviceState.Port > 0 {
		rollbackConfig.Port = serviceState.Port
	}
	desiredServices[rollbackService] = rollbackConfig
	rollbackImageRefs := map[string]string{rollbackService: serviceState.Image}

	if err := deploy.ReconcileTakodProxy(desiredServices); err != nil {
		return fmt.Errorf("rollback succeeded but failed to reconcile proxy: %w", err)
	}

	postRollbackNodeActualState, err := reconcile.GatherActualStateByServer(sshPool, cfg, envName, envServers)
	if err != nil {
		return fmt.Errorf("rollback succeeded but failed to gather post-rollback actual state: %w", err)
	}
	postRollbackActualState := reconcile.AggregateActualStateByServer(postRollbackNodeActualState)
	runtimeImageRefs := mergeRuntimeImageRefs(cfg, envName, desiredServices, rollbackImageRefs, postRollbackActualState)
	if err := persistTakodRuntimeState(
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
		fmt.Sprintf("rolled back %s to deployment %s", rollbackService, targetDeployment.ID),
		map[string]string{
			"service":      rollbackService,
			"deploymentId": targetDeployment.ID,
		},
	); err != nil {
		return fmt.Errorf("rollback succeeded but failed to persist takod state: %w", err)
	}

	// Replicate updated state to mesh nodes.
	if cfg.IsMultiServer() {
		replicator := remotestate.NewStateReplicator(sshPool, cfg, envName, cfg.Project.Name, verbose)
		history, err := stateManager.LoadHistory()
		if err != nil {
			return fmt.Errorf("rollback succeeded but failed to load remote deployment history for replication: %w", err)
		}
		if err := replicator.ReplicateDeployment(rollbackDeployment, history); err != nil {
			return fmt.Errorf("rollback succeeded but failed to replicate remote deployment history: %w", err)
		}
	}

	saveLocalRollbackState(cfg, envName, envServers, rollbackDeployment, rollbackService, targetDeployment.ID, verbose)

	// Send success notification
	if notifier != nil {
		notifier.Notify(notification.Event{
			Type:        notification.EventRollbackDone,
			Project:     cfg.Project.Name,
			Environment: envName,
			Service:     rollbackService,
			Message:     fmt.Sprintf("Successfully rolled back `%s` to version %s in %s", rollbackService, targetDeployment.Version, rollbackDuration.Round(time.Second)),
			Duration:    rollbackDuration,
			Details: map[string]string{
				"deployment_id": targetDeployment.ID,
				"version":       targetDeployment.Version,
			},
		})
	}

	fmt.Printf("\n✓ Successfully rolled back to deployment %s!\n", targetDeployment.ID)

	return nil
}

func selectRollbackHistorySource(cfg *config.Config, envName string, requestedServer string) (stateHistoryCandidate, error) {
	histories, err := collectStateDeploymentHistories(cfg, envName, requestedServer, !verbose)
	if err != nil {
		return stateHistoryCandidate{}, fmt.Errorf("failed to load rollback history: %w", err)
	}
	best, ok := bestDeploymentHistory(histories)
	if !ok {
		if requestedServer != "" {
			return stateHistoryCandidate{}, fmt.Errorf("no deployment history found on node %s", requestedServer)
		}
		return stateHistoryCandidate{}, fmt.Errorf("no deployment history found on reachable mesh nodes")
	}
	return best, nil
}

func deploymentFromHistory(history *remotestate.DeploymentHistory, deploymentID string) (*remotestate.DeploymentState, error) {
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

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func selectRollbackTargetFromHistory(history *remotestate.DeploymentHistory, deploymentID string, serviceName string) (*remotestate.DeploymentState, error) {
	if deploymentID != "" {
		deployment, err := deploymentFromHistory(history, deploymentID)
		if err != nil {
			return nil, fmt.Errorf("failed to find deployment %s: %w", deploymentID, err)
		}
		if err := validateRollbackTarget(deployment, serviceName); err != nil {
			return nil, err
		}
		return deployment, nil
	}
	deployment, err := previousStableServiceDeploymentFromHistory(history, serviceName)
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

func previousStableServiceDeploymentFromHistory(history *remotestate.DeploymentHistory, serviceName string) (*remotestate.DeploymentState, error) {
	deployments := listDeploymentsFromHistory(history, &remotestate.HistoryOptions{
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

func rollbackRemoteHistoryError(err error) error {
	return fmt.Errorf("rollback succeeded but failed to update remote deployment history: %w", err)
}

func buildRollbackDeployment(
	cfg *config.Config,
	envName string,
	host string,
	startTime time.Time,
	duration time.Duration,
	targetDeployment *remotestate.DeploymentState,
	serviceName string,
	serviceState remotestate.ServiceState,
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
		CLIVersion:     Version,
		CLICommit:      GitCommit,
	}
}

func saveLocalRollbackState(cfg *config.Config, envName string, serverNames []string, deployment *remotestate.DeploymentState, serviceName string, targetDeploymentID string, verbose bool) {
	localStateMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to initialize local state for rollback: %v\n", err)
		}
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
	if err := localStateMgr.SaveDeployment(localDeployment); err != nil && verbose {
		fmt.Printf("Warning: failed to save local rollback state: %v\n", err)
	}
}
