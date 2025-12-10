package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	liveServer  string
	liveService string
)

var liveCmd = &cobra.Command{
	Use:   "live",
	Short: "Disable maintenance mode and restore normal service",
	Long: `Disable maintenance mode for a service and restore normal traffic routing.

This command removes the maintenance page container and restores
traffic to the main service.

If --server is not specified, defaults to the first server or manager node in Swarm mode.

Examples:
  tako live --service web              # Disable maintenance on default server
  tako live --service web --server prod # Disable on specific server`,
	RunE: runLive,
}

func init() {
	rootCmd.AddCommand(liveCmd)
	liveCmd.Flags().StringVarP(&liveServer, "server", "s", "", "Server to disable maintenance on (default: first/manager server)")
	liveCmd.Flags().StringVar(&liveService, "service", "", "Service to restore (required)")
	liveCmd.MarkFlagRequired("service")
}

func runLive(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Get environment
	envName := getEnvironmentName(cfg)

	// Determine which server to use
	var serverName string
	var server config.ServerConfig

	if liveServer != "" {
		// Use specified server
		var exists bool
		server, exists = cfg.Servers[liveServer]
		if !exists {
			return fmt.Errorf("server %s not found in configuration", liveServer)
		}
		serverName = liveServer
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

	fmt.Printf("🔄 Disabling maintenance mode for %s on %s...\n\n", liveService, serverName)

	// Check if maintenance container exists
	containerName := fmt.Sprintf("%s_%s_maintenance", cfg.Project.Name, liveService)
	checkCmd := fmt.Sprintf("docker ps -a --filter name=%s --format '{{.Names}}'", containerName)
	output, err := client.Execute(checkCmd)
	if err != nil || output == "" {
		return fmt.Errorf("maintenance container not found - service may not be in maintenance mode")
	}

	// Stop and remove maintenance container
	fmt.Printf("→ Removing maintenance container...\n")
	removeCmd := fmt.Sprintf("docker stop %s && docker rm %s", containerName, containerName)
	if _, err := client.Execute(removeCmd); err != nil {
		return fmt.Errorf("failed to remove maintenance container: %w", err)
	}

	// Remove maintenance directory
	maintenanceDir := fmt.Sprintf("/opt/%s/maintenance", cfg.Project.Name)
	client.Execute(fmt.Sprintf("sudo rm -rf %s", maintenanceDir))

	fmt.Printf("✓ Maintenance mode disabled for %s\n", liveService)
	fmt.Printf("\nService is now accepting normal traffic.\n")
	fmt.Printf("Traefik has automatically updated routing.\n")

	return nil
}
