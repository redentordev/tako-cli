package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/ssh"
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

If --server is not specified, defaults to the first server or manager node in Swarm mode.

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
	rollbackCmd.Flags().StringVarP(&rollbackServer, "server", "s", "", "Server to rollback (default: first/manager server)")
	rollbackCmd.Flags().StringVar(&rollbackService, "service", "", "Service to rollback (required)")
	rollbackCmd.MarkFlagRequired("service")
}

func runRollback(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

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

	// Determine which server to use
	var serverName string
	var server config.ServerConfig

	if rollbackServer != "" {
		// Use specified server
		var exists bool
		server, exists = cfg.Servers[rollbackServer]
		if !exists {
			return fmt.Errorf("server %s not found in configuration", rollbackServer)
		}
		serverName = rollbackServer
	} else {
		// Default to first server or manager
		envServers, err := cfg.GetEnvironmentServers(envName)
		if err != nil {
			return fmt.Errorf("failed to get environment servers: %w", err)
		}

		if len(envServers) == 0 {
			return fmt.Errorf("no servers configured for environment %s", envName)
		}

		// If multi-server (Swarm), use manager; otherwise use first server
		if len(envServers) > 1 {
			managerName, err := cfg.GetManagerServer(envName)
			if err != nil {
				return fmt.Errorf("failed to get manager server: %w", err)
			}
			serverName = managerName
			server = cfg.Servers[managerName]
		} else {
			serverName = envServers[0]
			server = cfg.Servers[serverName]
		}

		if verbose {
			fmt.Printf("Using server: %s (%s)\n", serverName, server.Host)
		}
	}

	// Create SSH client
	client, err := ssh.NewClient(server.Host, server.Port, server.User, server.SSHKey)
	if err != nil {
		return fmt.Errorf("failed to create SSH client: %w", err)
	}
	defer client.Close()

	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}

	// Create state manager
	stateManager := state.NewStateManager(client, cfg.Project.Name, server.Host)

	// Determine which deployment to rollback to
	var targetDeployment *state.DeploymentState

	if len(args) > 0 {
		// Rollback to specific deployment ID
		deploymentID := args[0]
		fmt.Printf("\n=== Rolling back to deployment: %s ===\n\n", deploymentID)

		targetDeployment, err = stateManager.GetDeployment(deploymentID)
		if err != nil {
			return fmt.Errorf("failed to find deployment %s: %w", deploymentID, err)
		}
	} else {
		// Rollback to most recent successful deployment
		fmt.Printf("\n=== Rolling back to previous successful deployment ===\n\n")

		targetDeployment, err = stateManager.GetLatestSuccessful()
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

	// Create deployer
	deploy := deployer.NewDeployer(client, cfg, envName, verbose)

	// Perform rollback using state
	serviceState := targetDeployment.Services[rollbackService]
	if err := deploy.RollbackToState(rollbackService, &serviceState); err != nil {
		return fmt.Errorf("rollback failed: %w", err)
	}

	// Mark this deployment as rolled back in history
	targetDeployment.Status = state.StatusRolledBack
	if err := stateManager.SaveDeployment(targetDeployment); err != nil {
		if verbose {
			fmt.Printf("Warning: failed to update deployment status: %v\n", err)
		}
	}

	fmt.Printf("\n✓ Successfully rolled back to deployment %s!\n", targetDeployment.ID)

	return nil
}
