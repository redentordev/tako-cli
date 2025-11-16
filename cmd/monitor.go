package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/monitoring"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	monitorService string
	monitorOnce    bool
)

var monitorCmd = &cobra.Command{
	Use:   "monitor",
	Short: "Continuously monitor deployed services",
	Long: `Monitor service health and send webhook alerts on failures.

This command continuously checks service health at configured intervals.
When a service fails, it sends a POST request to the configured webhook
with failure details.

For services with domains (load-balanced), it checks the domain endpoint.
For internal services, it checks individual container replicas.
For services without health checks, it verifies containers are running.

The monitor runs indefinitely until stopped with Ctrl+C. Use --once to
run a single check and exit.

Examples:
  tako monitor                  # Monitor all services continuously
  tako monitor --service web    # Monitor specific service only
  tako monitor --once           # Run single check and exit
  tako monitor -v               # Verbose output

Configuration:
Services must have monitoring enabled in tako.yaml:

  services:
    web:
      port: 3000
      proxy:
        domains: [example.com]
      healthCheck:
        path: /health
      monitoring:
        enabled: true
        interval: 60s
        webhook: https://hooks.slack.com/services/xxx

Webhooks receive JSON payloads on service failures:
  {
    "event": "service_down",
    "project": "myapp",
    "service": "web",
    "severity": "critical",
    "details": {
      "error": "Health check failed: 503",
      "replicas_down": 2
    }
  }
`,
	Args: cobra.NoArgs,
	RunE: runMonitor,
}

func init() {
	rootCmd.AddCommand(monitorCmd)
	monitorCmd.Flags().StringVarP(&monitorService, "service", "s", "", "Monitor specific service only")
	monitorCmd.Flags().BoolVar(&monitorOnce, "once", false, "Run single check and exit (don't monitor continuously)")
}

func runMonitor(cmd *cobra.Command, args []string) error {
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

	// Filter to specific service if provided
	if monitorService != "" {
		if _, exists := services[monitorService]; !exists {
			return fmt.Errorf("service '%s' not found in environment %s", monitorService, envName)
		}
	}

	// Verify at least one service has monitoring enabled
	hasMonitoring := false
	for _, service := range services {
		// Skip if filtering and not the selected service
		if monitorService != "" && service.Monitoring == nil {
			continue
		}
		if service.Monitoring != nil && service.Monitoring.Enabled {
			hasMonitoring = true
			break
		}
	}

	if !hasMonitoring {
		return fmt.Errorf("no services have monitoring enabled in environment %s", envName)
	}

	// Create SSH connection pool
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	// Create monitor
	monitor := monitoring.NewMonitor(cfg, sshPool, verbose)

	// Run monitoring
	if monitorOnce {
		return monitor.CheckOnce(envName)
	}

	return monitor.Start(envName)
}
