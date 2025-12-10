package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	stopServer  string
	stopService string
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a deployed service without removing it",
	Long: `Stop a running service on the server without removing containers or data.
The service can be restarted later with 'tako start'.

This is useful for:
  - Temporarily stopping a service for maintenance
  - Reducing resource usage when service is not needed
  - Testing failover scenarios

Examples:
  tako stop --service web --server prod
  tako stop --service api --server staging`,
	RunE: runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)
	stopCmd.Flags().StringVarP(&stopServer, "server", "s", "", "Server to stop service on (required)")
	stopCmd.Flags().StringVar(&stopService, "service", "", "Service to stop (required)")
	stopCmd.MarkFlagRequired("server")
	stopCmd.MarkFlagRequired("service")
}

func runStop(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Get environment
	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	// Check service exists
	if _, exists := services[stopService]; !exists {
		return fmt.Errorf("service %s not found in environment %s", stopService, envName)
	}

	// Get server config
	server, exists := cfg.Servers[stopServer]
	if !exists {
		return fmt.Errorf("server %s not found in configuration", stopServer)
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

	fmt.Printf("🛑 Stopping service %s on %s...\n\n", stopService, stopServer)

	// Stop all containers for this service
	pattern := fmt.Sprintf("%s_%s_%s_", cfg.Project.Name, envName, stopService)

	// List containers that will be stopped
	listCmd := fmt.Sprintf("docker ps --filter 'name=%s' --format '{{.Names}}'", pattern)
	output, err := client.Execute(listCmd)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	if output == "" || len(output) == 0 {
		fmt.Printf("No running containers found for service %s\n", stopService)
		return nil
	}

	if verbose {
		fmt.Printf("Containers to stop:\n%s\n", output)
	}

	// Stop containers
	stopCmdStr := fmt.Sprintf("docker ps --filter 'name=%s' --format '{{.Names}}' | xargs -r docker stop", pattern)
	if verbose {
		fmt.Printf("Executing: %s\n", stopCmdStr)
	}

	_, err = client.Execute(stopCmdStr)
	if err != nil {
		return fmt.Errorf("failed to stop containers: %w", err)
	}

	fmt.Printf("✓ Service %s stopped successfully\n", stopService)
	fmt.Printf("\nContainers are stopped but not removed.\n")
	fmt.Printf("To start the service again: tako start --service %s --server %s\n", stopService, stopServer)

	return nil
}
