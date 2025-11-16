package cmd

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	historyServer string
	historyLimit  int
	historyStatus string
)

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "View deployment history",
	Long: `View deployment history stored on the server.

This shows all past deployments with their status, timestamp, and services.
You can view details and rollback to any previous deployment.`,
	RunE: runHistory,
}

func init() {
	rootCmd.AddCommand(historyCmd)
	historyCmd.Flags().StringVarP(&historyServer, "server", "s", "production", "Server to view history from")
	historyCmd.Flags().IntVarP(&historyLimit, "limit", "n", 10, "Number of deployments to show")
	historyCmd.Flags().StringVar(&historyStatus, "status", "", "Filter by status (success, failed, rolled_back)")
}

func runHistory(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Get server config
	server, exists := cfg.Servers[historyServer]
	if !exists {
		return fmt.Errorf("server %s not found in configuration", historyServer)
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

	// Create state manager
	stateManager := state.NewStateManager(client, cfg.Project.Name, server.Host)

	// List deployments
	opts := &state.HistoryOptions{
		Limit:         historyLimit,
		IncludeFailed: true,
	}

	if historyStatus != "" {
		opts.Status = state.DeploymentStatus(historyStatus)
	}

	deployments, err := stateManager.ListDeployments(opts)
	if err != nil {
		return fmt.Errorf("failed to load deployment history: %w", err)
	}

	if len(deployments) == 0 {
		fmt.Println("No deployments found")
		return nil
	}

	// Display deployments
	fmt.Printf("\n📋 Deployment History for %s on %s\n\n", cfg.Project.Name, historyServer)
	fmt.Println(strings.Repeat("─", 120))
	fmt.Printf("%-12s %-10s %-20s %-10s %-10s %-15s %-40s\n", "ID", "COMMIT", "TIMESTAMP", "VERSION", "STATUS", "DURATION", "MESSAGE")
	fmt.Println(strings.Repeat("─", 120))

	for _, dep := range deployments {
		// Format timestamp
		timestamp := dep.Timestamp.Format("2006-01-02 15:04:05")

		// Format status with color
		status := formatStatus(dep.Status)

		// Format commit hash
		commit := dep.GitCommitShort
		if commit == "" {
			commit = "-"
		}

		// Format commit message
		commitMsg := dep.GitCommitMsg
		if commitMsg == "" {
			commitMsg = "-"
		}
		if len(commitMsg) > 38 {
			commitMsg = commitMsg[:35] + "..."
		}

		// Format duration
		duration := state.FormatDuration(dep.Duration)

		fmt.Printf("%-12s %-10s %-20s %-10s %-10s %-15s %-40s\n",
			state.FormatDeploymentID(dep.ID),
			commit,
			timestamp,
			dep.Version,
			status,
			duration,
			commitMsg,
		)

		// Show error if failed
		if dep.Status == state.StatusFailed && dep.Error != "" {
			fmt.Printf("             Error: %s\n", dep.Error)
		}
	}

	fmt.Println(strings.Repeat("─", 120))
	fmt.Printf("\nShowing %d deployment(s). Use --limit to show more.\n", len(deployments))
	fmt.Printf("\nTo rollback to a specific deployment: tako rollback <deployment-id>\n")
	fmt.Printf("To view detailed logs: tako logs show <deployment-id> (coming soon)\n\n")

	return nil
}

func formatStatus(status state.DeploymentStatus) string {
	switch status {
	case state.StatusSuccess:
		return "✓ success"
	case state.StatusFailed:
		return "✗ failed"
	case state.StatusRolledBack:
		return "↺ rolled_back"
	case state.StatusInProgress:
		return "⋯ in_progress"
	default:
		return string(status)
	}
}
