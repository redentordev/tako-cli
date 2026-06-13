package cmd

import (
	"fmt"
	"os"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takodclient"
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

If --server is not specified, uses the first reachable environment node.

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
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if !cfg.IsTakodRuntime() {
		return fmt.Errorf("runtime.mode=%s is not supported; Tako now uses runtime.mode=takod", cfg.GetRuntimeMode())
	}
	if logsTail < 0 {
		return fmt.Errorf("tail cannot be negative")
	}

	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	if _, exists := services[logsService]; !exists {
		return fmt.Errorf("service %s not found in environment %s", logsService, envName)
	}

	serverName, server, client, err := connectResolvedServer(cfg, envName, logsServer)
	if err != nil {
		return err
	}
	defer client.Close()
	if verbose {
		fmt.Printf("Using node: %s (%s)\n", serverName, server.Host)
	}

	fmt.Printf("Streaming logs from %s on %s (takod)...\n\n", logsService, serverName)

	endpoint := takodclient.LogsEndpoint(cfg.Project.Name, envName, logsService, logsTail, logsFollow)
	if err := takodclient.StreamOutput(client, takodSocketFromConfig(cfg), endpoint, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("failed to stream logs: %w", err)
	}

	return nil
}
