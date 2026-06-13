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
	rollbackServer  string
	rollbackService string
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback [deployment-id]",
	Short: "Rollback to previous deployment",
	Long: `Rollback to a previous deployment.

If no deployment-id is provided, rolls back to the most recent successful deployment.
Specify a deployment-id to rollback to a specific deployment.

If --server is not specified, rollback reads the freshest reachable deployment
history from the mesh.

Use 'tako history' to view available deployments.

Examples:
  tako rollback --service web                 # Rollback to previous deployment
  tako rollback --service web --server prod   # Rollback on specific server
  tako rollback --service web deploy-123      # Rollback to specific deployment`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRollback,
}

func init() {
	rootCmd.AddCommand(rollbackCmd)
	rollbackCmd.Flags().StringVarP(&rollbackServer, "server", "s", "", "Node to read replicated deployment state from")
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

	historySource, err := selectRollbackHistorySource(cfg, envName, rollbackServer)
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
	var targetDeployment *remotestate.DeploymentState

	if len(args) > 0 {
		// Rollback to specific deployment ID
		deploymentID := args[0]
		fmt.Printf("\n=== Rolling back to deployment: %s ===\n\n", deploymentID)

		targetDeployment, err = deploymentFromHistory(historySource.history, deploymentID)
		if err != nil {
			return fmt.Errorf("failed to find deployment %s: %w", deploymentID, err)
		}
	} else {
		// Rollback to most recent successful deployment
		fmt.Printf("\n=== Rolling back to previous successful deployment ===\n\n")

		targetDeployment, err = latestSuccessfulDeploymentFromHistory(historySource.history)
		if err != nil {
			return fmt.Errorf("failed to find previous deployment: %w", err)
		}
	}

	// Verify target deployment has the requested service
	if _, exists := targetDeployment.Services[rollbackService]; !exists {
		return fmt.Errorf("service %s not found in deployment %s", rollbackService, targetDeployment.ID)
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

	// Mark this deployment as rolled back in history
	targetDeployment.Status = remotestate.StatusRolledBack
	if err := stateManager.SaveDeployment(targetDeployment); err != nil {
		return rollbackRemoteHistoryError(err)
	}

	desiredServices := cloneServiceMap(services)
	rollbackConfig := desiredServices[rollbackService]
	rollbackConfig.Image = serviceState.Image
	rollbackConfig.Replicas = serviceState.Replicas
	if serviceState.Port > 0 {
		rollbackConfig.Port = serviceState.Port
	}
	desiredServices[rollbackService] = rollbackConfig
	imageRefs := defaultImageRefs(cfg, envName, desiredServices)
	imageRefs[rollbackService] = serviceState.Image

	if err := deploy.ReconcileTakodProxy(desiredServices); err != nil {
		return fmt.Errorf("rollback succeeded but failed to reconcile proxy: %w", err)
	}

	postRollbackNodeActualState, err := reconcile.GatherActualStateByServer(sshPool, cfg, envName, envServers)
	if err != nil {
		return fmt.Errorf("rollback succeeded but failed to gather post-rollback actual state: %w", err)
	}
	postRollbackActualState := reconcile.AggregateActualStateByServer(postRollbackNodeActualState)
	if err := persistTakodRuntimeState(
		sshPool,
		cfg,
		envName,
		envServers,
		"rollback",
		desiredServices,
		imageRefs,
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
		if err := replicator.ReplicateDeployment(targetDeployment, history); err != nil {
			return fmt.Errorf("rollback succeeded but failed to replicate remote deployment history: %w", err)
		}
	}

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

func rollbackRemoteHistoryError(err error) error {
	return fmt.Errorf("rollback succeeded but failed to update remote deployment history: %w", err)
}

func latestSuccessfulDeploymentFromHistory(history *remotestate.DeploymentHistory) (*remotestate.DeploymentState, error) {
	deployments := listDeploymentsFromHistory(history, &remotestate.HistoryOptions{
		Status:        remotestate.StatusSuccess,
		Limit:         1,
		IncludeFailed: false,
	})
	if len(deployments) == 0 {
		return nil, fmt.Errorf("no successful deployments found")
	}
	return deployments[0], nil
}
