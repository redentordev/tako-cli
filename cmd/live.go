package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
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

If --server is not specified, defaults to the primary environment node.

Examples:
  tako live --service web              # Disable maintenance on default server
  tako live --service web --server prod # Disable on specific server`,
	RunE: runLive,
}

func init() {
	rootCmd.AddCommand(liveCmd)
	liveCmd.Flags().StringVarP(&liveServer, "server", "s", "", "Node to disable maintenance on (default: primary node)")
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
		primaryName, err := cfg.GetPrimaryServer(envName)
		if err != nil {
			return fmt.Errorf("failed to get primary node: %w", err)
		}
		serverName = primaryName
		server = cfg.Servers[primaryName]

		if verbose {
			fmt.Printf("Using node: %s (%s)\n", serverName, server.Host)
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

	// Stop and remove maintenance container
	fmt.Printf("→ Removing maintenance container...\n")
	socket := takodSocketFromConfig(cfg)
	networkName := fmt.Sprintf("tako_%s_%s", cfg.Project.Name, envName)
	request := takod.ReconcileServiceRequest{
		Project:     cfg.Project.Name,
		Environment: envName,
		Service:     maintenanceTakodServiceName(liveService),
		Image:       maintenanceImage,
		Network:     networkName,
	}
	if _, err := takodclient.RequestJSON(client, socket, "POST", "/v1/reconcile-service", request); err != nil {
		return fmt.Errorf("failed to remove maintenance container: %w", err)
	}

	// Remove file-provider override from tako-proxy.
	if _, err := takodclient.RequestJSON(client, socket, "DELETE", takodclient.ProxyFileEndpoint(maintenanceProxyConfigFileName(cfg.Project.Name, envName, liveService)), nil); err != nil {
		return fmt.Errorf("failed to remove maintenance proxy config: %w", err)
	}

	fmt.Printf("✓ Maintenance mode disabled for %s\n", liveService)
	fmt.Printf("\nService is now accepting normal traffic.\n")
	fmt.Printf("tako-proxy has removed the maintenance routing override.\n")

	return nil
}
