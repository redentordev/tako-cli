package cmd

import (
	"fmt"

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

This command starts containers that were stopped with 'tako stop'.
It does not deploy new code - use 'tako deploy' for that.

Examples:
  tako start --service web --server prod
  tako start --service api --server staging`,
	RunE: runStart,
}

func init() {
	rootCmd.AddCommand(startCmd)
	startCmd.Flags().StringVarP(&startServer, "server", "s", "", "Server to start service on (required)")
	startCmd.Flags().StringVar(&startService, "service", "", "Service to start (required)")
	startCmd.MarkFlagRequired("server")
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

	// Check service exists
	if _, exists := services[startService]; !exists {
		return fmt.Errorf("service %s not found in environment %s", startService, envName)
	}

	// Get server config
	server, exists := cfg.Servers[startServer]
	if !exists {
		return fmt.Errorf("server %s not found in configuration", startServer)
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

	fmt.Printf("▶️  Starting service %s on %s...\n\n", startService, startServer)

	// Start all containers for this service
	pattern := fmt.Sprintf("%s_%s_%s_", cfg.Project.Name, envName, startService)

	// List stopped containers
	listCmd := fmt.Sprintf("docker ps -a --filter 'name=%s' --filter 'status=exited' --format '{{.Names}}'", pattern)
	output, err := client.Execute(listCmd)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	if output == "" || len(output) == 0 {
		fmt.Printf("No stopped containers found for service %s\n", startService)
		fmt.Printf("Hint: Check if containers are already running with 'tako ps'\n")
		return nil
	}

	if verbose {
		fmt.Printf("Containers to start:\n%s\n", output)
	}

	// Start containers
	startCmdStr := fmt.Sprintf("docker ps -a --filter 'name=%s' --filter 'status=exited' --format '{{.Names}}' | xargs -r docker start", pattern)
	if verbose {
		fmt.Printf("Executing: %s\n", startCmdStr)
	}

	_, err = client.Execute(startCmdStr)
	if err != nil {
		return fmt.Errorf("failed to start containers: %w", err)
	}

	fmt.Printf("✓ Service %s started successfully\n", startService)
	fmt.Printf("\nCheck status with: tako ps\n")

	return nil
}
