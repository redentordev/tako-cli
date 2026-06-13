package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/spf13/cobra"
)

var (
	startServer  string
	startService string
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start a stopped service",
	Long: `Start a stopped takod service by reconciling it back to its configured
replica count.

Examples:
  tako start --service web
  tako start --service api --server prod`,
	RunE: runStart,
}

func init() {
	rootCmd.AddCommand(startCmd)
	startCmd.Flags().StringVarP(&startServer, "server", "s", "", "Server to target")
	startCmd.Flags().StringVar(&startService, "service", "", "Service to start (required)")
	startCmd.MarkFlagRequired("service")
}

func runStart(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	service, ok := services[startService]
	if !ok {
		return fmt.Errorf("service %s not found in environment %s", startService, envName)
	}

	replicas := service.Replicas
	if replicas <= 0 {
		replicas = 1
	}

	return runScaleWithServer(cmd, []string{fmt.Sprintf("%s=%d", startService, replicas)}, startServer)
}
