package cmd

import (
	"fmt"
	"strings"

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

In Swarm mode, this scales the service to 0 replicas.

Examples:
  tako stop --service web --server prod
  tako stop --service api --server staging`,
	RunE: runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)
	stopCmd.Flags().StringVarP(&stopServer, "server", "s", "", "Server to stop service on (default: first server)")
	stopCmd.Flags().StringVar(&stopService, "service", "", "Service to stop (required)")
	stopCmd.MarkFlagRequired("service")
}

func runStop(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfigWithInfra(cfgFile, ".tako")
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

	// Get server config - default to first server if not specified
	if stopServer == "" {
		for name := range cfg.Servers {
			stopServer = name
			break
		}
	}

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

	// Swarm service name
	swarmServiceName := fmt.Sprintf("%s_%s_%s", cfg.Project.Name, envName, stopService)

	// Check if service exists
	checkCmd := fmt.Sprintf("docker service inspect %s --format '{{.Spec.Mode.Replicated.Replicas}}' 2>/dev/null", swarmServiceName)
	output, err := client.Execute(checkCmd)
	if err != nil || strings.TrimSpace(output) == "" {
		return fmt.Errorf("service %s not found in swarm", stopService)
	}

	currentReplicas := strings.TrimSpace(output)
	if currentReplicas == "0" {
		fmt.Printf("Service %s is already stopped (0 replicas)\n", stopService)
		return nil
	}

	if verbose {
		fmt.Printf("Current replicas: %s\n", currentReplicas)
	}

	// Scale service to 0 replicas (stop without removing)
	scaleCmd := fmt.Sprintf("docker service scale %s=0", swarmServiceName)
	if verbose {
		fmt.Printf("Executing: %s\n", scaleCmd)
	}

	_, err = client.Execute(scaleCmd)
	if err != nil {
		return fmt.Errorf("failed to stop service: %w", err)
	}

	fmt.Printf("✓ Service %s stopped successfully (scaled to 0 replicas)\n", stopService)
	fmt.Printf("\nThe service is paused but not removed.\n")
	fmt.Printf("To start the service again: tako start --service %s\n", stopService)

	return nil
}
