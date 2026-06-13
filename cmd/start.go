package cmd

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	startServer  string
	startService string
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start a stopped service",
	Long: `Start a previously stopped service on the server.

This command restarts a service that was stopped with 'tako stop'.
It restores the service to its configured replica count.

In Swarm mode, this scales the service back to its configured replicas.

Examples:
  tako start --service web
  tako start --service api --server prod`,
	RunE: runStart,
}

func init() {
	rootCmd.AddCommand(startCmd)
	startCmd.Flags().StringVarP(&startServer, "server", "s", "", "Server to start service on (default: first server)")
	startCmd.Flags().StringVar(&startService, "service", "", "Service to start (required)")
	startCmd.MarkFlagRequired("service")
}

func runStart(cmd *cobra.Command, args []string) error {
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

	// Check service exists and get config
	svcConfig, exists := services[startService]
	if !exists {
		return fmt.Errorf("service %s not found in environment %s", startService, envName)
	}

	// Get target replicas from config (default to 1)
	targetReplicas := svcConfig.Replicas
	if targetReplicas == 0 {
		targetReplicas = 1
	}

	// Get server config - default to first server if not specified
	if startServer == "" {
		for name := range cfg.Servers {
			startServer = name
			break
		}
	}

	server, exists := cfg.Servers[startServer]
	if !exists {
		return fmt.Errorf("server %s not found in configuration", startServer)
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

	fmt.Printf("▶️  Starting service %s on %s...\n\n", startService, startServer)

	// Swarm service name
	swarmServiceName := fmt.Sprintf("%s_%s_%s", cfg.Project.Name, envName, startService)

	// Check if service exists and get current replicas
	checkCmd := fmt.Sprintf("docker service inspect %s --format '{{.Spec.Mode.Replicated.Replicas}}' 2>/dev/null", swarmServiceName)
	output, err := client.Execute(checkCmd)
	if err != nil || strings.TrimSpace(output) == "" {
		return fmt.Errorf("service %s not found in swarm. Deploy it first with 'tako deploy'", startService)
	}

	currentReplicas := strings.TrimSpace(output)
	if currentReplicas != "0" {
		fmt.Printf("Service %s is already running (%s replicas)\n", startService, currentReplicas)
		return nil
	}

	if verbose {
		fmt.Printf("Scaling from 0 to %d replicas\n", targetReplicas)
	}

	// Scale service back to configured replicas
	scaleCmd := fmt.Sprintf("docker service scale %s=%d", swarmServiceName, targetReplicas)
	if verbose {
		fmt.Printf("Executing: %s\n", scaleCmd)
	}

	_, err = client.Execute(scaleCmd)
	if err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	fmt.Printf("✓ Service %s started successfully (%d replicas)\n", startService, targetReplicas)
	fmt.Printf("\nCheck status with: tako ps\n")

	return nil
}
