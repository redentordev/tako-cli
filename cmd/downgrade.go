package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/swarm"
	"github.com/spf13/cobra"
)

var downgradeCmd = &cobra.Command{
	Use:   "downgrade",
	Short: "Downgrade from Docker Swarm to single-server mode",
	Long: `Gracefully downgrade from Docker Swarm multi-server mode to single-server mode.

This command will:
  1. Backup SSL certificates and Swarm configuration
  2. Stop all Swarm services
  3. Leave the Swarm cluster
  4. Clean up overlay networks and Swarm artifacts
  5. Deploy Traefik as a regular container
  6. Prepare for single-server deployments

⚠️  WARNING: This will cause brief downtime during the transition.
Your data and volumes will be preserved.

Use this when:
  - Reducing from multiple servers to a single server
  - Simplifying infrastructure
  - Cost optimization

After downgrade, redeploy your services with 'tako deploy'`,
	RunE: runDowngrade,
}

func init() {
	rootCmd.AddCommand(downgradeCmd)
}

func runDowngrade(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Get environment
	envName := getEnvironmentName(cfg)

	// Create SSH pool
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	// Get environment configuration
	env, exists := cfg.Environments[envName]
	if !exists {
		return fmt.Errorf("environment %s not found", envName)
	}

	// Check server count
	if len(env.Servers) > 1 {
		return fmt.Errorf("downgrade is only for transitioning to single-server mode\nYou currently have %d servers configured. Remove servers from your config first.", len(env.Servers))
	}

	if len(env.Servers) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}

	// Get the single server
	serverName := env.Servers[0]
	serverConfig, exists := cfg.Servers[serverName]
	if !exists {
		return fmt.Errorf("server %s not found in configuration", serverName)
	}

	fmt.Printf("\n=== Swarm Downgrade ===\n\n")
	fmt.Printf("Environment: %s\n", envName)
	fmt.Printf("Server: %s (%s)\n\n", serverName, serverConfig.Host)

	// Connect to server (supports both key and password auth)
	client, err := sshPool.GetOrCreateWithAuth(serverConfig.Host, serverConfig.Port, serverConfig.User, serverConfig.SSHKey, serverConfig.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}

	// Create downgrade manager
	downgradeMgr := swarm.NewDowngradeManager(client, cfg.Project.Name, envName, verbose)

	// Check if Swarm is active
	if !downgradeMgr.ShouldDowngrade(len(env.Servers)) {
		fmt.Println("Swarm is not active on this server. No downgrade needed.")
		return nil
	}

	// Show warning and get confirmation
	fmt.Println("⚠️  This will:")
	fmt.Println("   • Stop all Swarm services")
	fmt.Println("   • Leave the Swarm cluster")
	fmt.Println("   • Remove overlay networks")
	fmt.Println("   • Cause brief downtime (~30s)")
	fmt.Println()
	fmt.Println("✓  This will preserve:")
	fmt.Println("   • Data volumes")
	fmt.Println("   • SSL certificates")
	fmt.Println("   • Docker images")
	fmt.Println()

	if !confirmAction("Continue with downgrade?") {
		fmt.Println("\nDowngrade cancelled.")
		return nil
	}

	// Execute downgrade
	fmt.Println()
	if err := downgradeMgr.DowngradeToSingleServer(); err != nil {
		return fmt.Errorf("downgrade failed: %w", err)
	}

	fmt.Printf("\n✓ Successfully downgraded to single-server mode!\n\n")
	fmt.Println("Next steps:")
	fmt.Println("  1. Update your tako.yaml to remove extra servers")
	fmt.Println("  2. Run 'tako deploy' to redeploy services")
	fmt.Println()
	fmt.Println("Backup location: /root/swarm-backup/")

	return nil
}

// confirmAction prompts user for confirmation
func confirmAction(message string) bool {
	fmt.Printf("%s (yes/no): ", message)
	var response string
	fmt.Scanln(&response)
	return response == "yes" || response == "y"
}
