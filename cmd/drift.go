package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/drift"
	"github.com/redentordev/tako-cli/pkg/notification"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	driftWatch    bool
	driftInterval time.Duration
	driftWebhook  string
)

var driftCmd = &cobra.Command{
	Use:   "drift",
	Short: "Detect configuration drift",
	Long: `Check for drift between your configuration and running services.

Drift detection compares:
  - Service existence (missing services)
  - Replica counts (services with fewer replicas than expected)
  - Unexpected services (running but not in config)

Examples:
  # Check once
  tako drift

  # Watch continuously (check every 5 minutes)
  tako drift --watch --interval 5m

  # Watch with notifications
  tako drift --watch --webhook https://hooks.slack.com/...
`,
	RunE: runDrift,
}

func init() {
	rootCmd.AddCommand(driftCmd)
	driftCmd.Flags().BoolVar(&driftWatch, "watch", false, "Continuously watch for drift")
	driftCmd.Flags().DurationVar(&driftInterval, "interval", 5*time.Minute, "Check interval for watch mode")
	driftCmd.Flags().StringVar(&driftWebhook, "webhook", "", "Webhook URL for notifications (Slack/Discord/generic)")
}

func runDrift(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)

	serverNames, err := statePullServerNames(cfg, envName, "")
	if err != nil {
		return err
	}
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	// Setup notifier if webhook provided
	var notifier *notification.Notifier
	if driftWebhook != "" {
		notifier = notification.NewNotifier(notification.NotifierConfig{
			Webhook: driftWebhook,
		}, verbose)
	}

	detector := drift.NewDetectorWithActualProvider(cfg, envName, notifier, verbose, func() (map[string]drift.ActualService, error) {
		actual, err := reconcile.GatherActualStateFromServers(sshPool, cfg, envName, serverNames, nil)
		if err != nil {
			return nil, err
		}
		return driftServicesFromReconcile(actual), nil
	})

	if driftWatch {
		return watchDrift(detector, driftInterval)
	}

	return checkDriftOnce(detector)
}

func driftServicesFromReconcile(actual map[string]*reconcile.ActualService) map[string]drift.ActualService {
	services := make(map[string]drift.ActualService, len(actual))
	for serviceName, service := range actual {
		if service == nil {
			continue
		}
		services[serviceName] = drift.ActualService{
			Name:     serviceName,
			Image:    service.Image,
			Replicas: service.Replicas,
			Running:  service.Replicas,
		}
	}
	return services
}

func checkDriftOnce(detector *drift.Detector) error {
	fmt.Printf("=== Drift Detection ===\n\n")

	state, err := detector.CheckOnce()
	if err != nil {
		return fmt.Errorf("drift check failed: %w", err)
	}

	printDriftState(state)
	return nil
}

func watchDrift(detector *drift.Detector, interval time.Duration) error {
	fmt.Printf("=== Continuous Drift Detection ===\n")
	fmt.Printf("Checking every %s (Ctrl+C to stop)\n\n", interval)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		fmt.Println("\nStopping drift detection...")
		cancel()
	}()

	return detector.Start(ctx, interval)
}

func printDriftState(state *drift.DriftState) {
	fmt.Printf("Project:     %s\n", state.Project)
	fmt.Printf("Environment: %s\n", state.Environment)
	fmt.Printf("Last Check:  %s\n", state.LastCheck.Format("2006-01-02 15:04:05"))
	fmt.Printf("Duration:    %s\n\n", state.CheckDuration.Round(time.Millisecond))

	if len(state.Drifts) == 0 {
		fmt.Printf("✓ No drift detected - all %d services are in sync\n", len(state.ServicesOK))
		return
	}

	fmt.Printf("⚠️  Detected %d drift(s):\n\n", len(state.Drifts))

	// Group by severity
	bySeverity := map[string][]drift.DriftReport{
		"critical": {},
		"high":     {},
		"medium":   {},
		"low":      {},
	}

	for _, d := range state.Drifts {
		bySeverity[d.Severity] = append(bySeverity[d.Severity], d)
	}

	severityOrder := []string{"critical", "high", "medium", "low"}
	severityEmoji := map[string]string{
		"critical": "🔴",
		"high":     "🟠",
		"medium":   "🟡",
		"low":      "🔵",
	}

	for _, sev := range severityOrder {
		drifts := bySeverity[sev]
		if len(drifts) == 0 {
			continue
		}

		fmt.Printf("%s %s:\n", severityEmoji[sev], sev)
		for _, d := range drifts {
			fmt.Printf("  • %s (%s)\n", d.Service, d.Type)
			fmt.Printf("    Expected: %s\n", d.Expected)
			fmt.Printf("    Actual:   %s\n", d.Actual)
		}
		fmt.Println()
	}

	if len(state.ServicesOK) > 0 {
		fmt.Printf("✓ %d service(s) OK: %v\n", len(state.ServicesOK), state.ServicesOK)
	}
}
