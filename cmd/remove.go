package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	removeServer string
	removeForce  bool
)

var removeCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove all deployed services from a server",
	Long: `Remove all deployed services and containers from a server.

This command:
  - Stops and removes all service containers
  - Removes Docker images for this project
  - Removes proxy configurations
  - Preserves server infrastructure (Docker, Traefik remain installed)
  - Does NOT decommission the server

The server can be reused for new deployments after removal.
To fully decommission a server, use 'tako destroy --decommission'.

Examples:
  tako remove --server prod
  tako remove --server staging --force`,
	RunE: runRemove,
}

func init() {
	rootCmd.AddCommand(removeCmd)
	removeCmd.Flags().StringVarP(&removeServer, "server", "s", "", "Server to remove services from (required)")
	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Skip confirmation prompt")
	removeCmd.MarkFlagRequired("server")
}

func runRemove(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Get environment
	envName := getEnvironmentName(cfg)

	// Get server config
	server, exists := cfg.Servers[removeServer]
	if !exists {
		return fmt.Errorf("server %s not found in configuration", removeServer)
	}

	// Create SSH client (supports both key and password auth)
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     server.Host,
		Port:     server.Port,
		User:     server.User,
		SSHKey:   server.SSHKey,
		Password: server.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to create SSH client: %w", err)
	}
	defer client.Close()

	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}

	fmt.Printf("\n⚠️  WARNING: You are about to REMOVE all services from:\n\n")
	fmt.Printf("   Server:      %s (%s)\n", removeServer, server.Host)
	fmt.Printf("   Project:     %s\n", cfg.Project.Name)
	fmt.Printf("   Environment: %s\n\n", envName)

	fmt.Printf("This will:\n")
	fmt.Printf("   • Stop and remove all containers for this project\n")
	fmt.Printf("   • Remove Docker images\n")
	fmt.Printf("   • Remove proxy configurations\n")
	fmt.Printf("   • Remove deployment state\n\n")

	fmt.Printf("This will NOT:\n")
	fmt.Printf("   • Decommission the server\n")
	fmt.Printf("   • Remove Docker or Traefik\n")
	fmt.Printf("   • Remove persistent volume data\n\n")

	// Confirmation unless --force
	if !removeForce {
		fmt.Printf("Type the project name '%s' to confirm: ", cfg.Project.Name)
		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read input: %w", err)
		}

		input = strings.TrimSpace(input)
		if input != cfg.Project.Name {
			fmt.Println("❌ Confirmation failed. Operation cancelled.")
			return nil
		}
	}

	fmt.Printf("\n🗑️  Removing all services for %s...\n\n", cfg.Project.Name)

	// Stop and remove all containers
	fmt.Printf("→ Stopping containers...\n")
	stopPattern := fmt.Sprintf("%s_%s_", cfg.Project.Name, envName)
	stopCmd := fmt.Sprintf("docker ps -a --filter 'name=%s' --format '{{.Names}}' | xargs -r docker stop", stopPattern)
	if _, err := client.Execute(stopCmd); err != nil && verbose {
		fmt.Printf("  Warning: Error stopping containers: %v\n", err)
	}

	fmt.Printf("→ Removing containers...\n")
	removeContainersCmd := fmt.Sprintf("docker ps -a --filter 'name=%s' --format '{{.Names}}' | xargs -r docker rm -f", stopPattern)
	if _, err := client.Execute(removeContainersCmd); err != nil && verbose {
		fmt.Printf("  Warning: Error removing containers: %v\n", err)
	}

	// Remove Docker images
	fmt.Printf("→ Removing Docker images...\n")
	removeImagesCmd := fmt.Sprintf("docker images '%s/*' --format '{{.Repository}}:{{.Tag}}' | xargs -r docker rmi -f", cfg.Project.Name)
	if _, err := client.Execute(removeImagesCmd); err != nil && verbose {
		fmt.Printf("  Warning: Error removing images: %v\n", err)
	}

	// Remove proxy configurations (Traefik will automatically update when containers are removed)
	fmt.Printf("→ Removing proxy configurations...\n")
	// Traefik uses Docker labels for configuration, so removing containers automatically removes proxy config
	if verbose {
		fmt.Printf("  Traefik will automatically detect removed containers\n")
	}

	// Remove deployment state
	fmt.Printf("→ Removing deployment state...\n")
	removeStateCmd := fmt.Sprintf("sudo rm -rf /var/lib/tako-cli/%s", cfg.Project.Name)
	if _, err := client.Execute(removeStateCmd); err != nil && verbose {
		fmt.Printf("  Warning: Error removing state: %v\n", err)
	}

	// Remove deployment files
	fmt.Printf("→ Removing deployment files...\n")
	removeDeployCmd := fmt.Sprintf("sudo rm -rf /opt/%s", cfg.Project.Name)
	if _, err := client.Execute(removeDeployCmd); err != nil && verbose {
		fmt.Printf("  Warning: Error removing deployment files: %v\n", err)
	}

	// Remove network
	fmt.Printf("→ Removing Docker network...\n")
	networkName := fmt.Sprintf("tako_%s_%s", cfg.Project.Name, envName)
	removeNetworkCmd := fmt.Sprintf("docker network rm %s 2>/dev/null || true", networkName)
	client.Execute(removeNetworkCmd)

	fmt.Printf("\n✓ All services removed from %s\n", removeServer)
	fmt.Printf("\nThe server is still provisioned and ready for new deployments.\n")
	fmt.Printf("To deploy again: tako deploy\n")

	return nil
}
