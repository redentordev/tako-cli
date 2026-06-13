package cmd

import (
	"fmt"
	"os"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	logsServer  string
	logsService string
	logsFollow  bool
	logsTail    int
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "View logs from deployed services",
	Long: `View logs from deployed services on remote servers.

If --server is not specified, defaults to the first environment server.

Examples:
  tako logs --service web              # View logs from default server
  tako logs --service web --server prod # View logs from specific server
  tako logs --service web -f            # Follow logs in real-time`,
	RunE: runLogs,
}

func init() {
	rootCmd.AddCommand(logsCmd)
	logsCmd.Flags().StringVarP(&logsServer, "server", "s", "", "Server to view logs from")
	logsCmd.Flags().StringVar(&logsService, "service", "", "Service to view logs from (required)")
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Follow log output")
	logsCmd.Flags().IntVarP(&logsTail, "tail", "n", 100, "Number of lines to show")
	logsCmd.MarkFlagRequired("service")
}

func runLogs(cmd *cobra.Command, args []string) error {
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

	// Check service exists
	if _, exists := services[logsService]; !exists {
		return fmt.Errorf("service %s not found in environment %s", logsService, envName)
	}

	// Determine which server to use
	var serverName string
	var server config.ServerConfig

	if logsServer != "" {
		// Use specified server
		var exists bool
		server, exists = cfg.Servers[logsServer]
		if !exists {
			return fmt.Errorf("server %s not found in configuration", logsServer)
		}
		serverName = logsServer
	} else {
		envServers, err := cfg.GetEnvironmentServers(envName)
		if err != nil {
			return fmt.Errorf("failed to get environment servers: %w", err)
		}

		if len(envServers) == 0 {
			return fmt.Errorf("no servers configured for environment %s", envName)
		}

		serverName = envServers[0]
		server = cfg.Servers[serverName]

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

	containerName := fmt.Sprintf("%s_%s_%s_1", cfg.Project.Name, envName, logsService)
	dockerCmd := fmt.Sprintf("docker logs --tail %d", logsTail)
	if logsFollow {
		dockerCmd += " -f"
	}
	dockerCmd += fmt.Sprintf(" %s", containerName)

	fmt.Printf("Streaming logs from %s on %s (takod)...\n\n", logsService, serverName)

	if err := client.ExecuteStream(dockerCmd, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("failed to stream logs: %w", err)
	}

	return nil
}
