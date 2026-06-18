package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
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
	Long: `View deployment history stored in the takod mesh.

This shows all past deployments with their status, timestamp, and services.
You can view details and rollback to any previous deployment.`,
	RunE: runHistory,
}

func init() {
	rootCmd.AddCommand(historyCmd)
	historyCmd.Flags().StringVarP(&historyServer, "server", "s", "", "Server to view history from instead of the full mesh")
	historyCmd.Flags().IntVarP(&historyLimit, "limit", "n", 10, "Number of deployments to show")
	historyCmd.Flags().StringVar(&historyStatus, "status", "", "Filter by status (success, failed, rolled_back)")
}

func runHistory(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	envName := getEnvironmentName(cfg)

	opts := &state.HistoryOptions{
		Limit:         historyLimit,
		IncludeFailed: true,
	}
	if historyStatus != "" {
		opts.Status = state.DeploymentStatus(historyStatus)
	}

	histories, err := collectStateDeploymentHistories(cfg, envName, historyServer, false)
	if err != nil {
		return fmt.Errorf("failed to load deployment history: %w", err)
	}
	best, ok := bestDeploymentHistory(histories)
	if !ok {
		fmt.Println("No deployments found")
		return nil
	}
	deployments := listDeploymentsFromHistory(best.history, opts)

	if len(deployments) == 0 {
		fmt.Println("No deployments found")
		return nil
	}

	// Display deployments
	fmt.Printf("\nDeployment History for %s from %s\n\n", cfg.Project.Name, best.source)
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
	fmt.Print(historyNextSteps())

	return nil
}

func historyNextSteps() string {
	return "\nTo rollback to a specific deployment: tako rollback <deployment-id>\n" +
		"To inspect service logs: tako logs --service <service> --tail 200\n" +
		"To inspect proxy access logs: tako access <service>\n\n"
}

func listDeploymentsFromHistory(history *state.DeploymentHistory, opts *state.HistoryOptions) []*state.DeploymentState {
	if history == nil {
		return nil
	}
	if opts == nil {
		opts = &state.HistoryOptions{Limit: 10, IncludeFailed: true}
	}

	var result []*state.DeploymentState
	for _, dep := range history.Deployments {
		if dep == nil {
			continue
		}
		if opts.Status != "" && dep.Status != opts.Status {
			continue
		}
		if opts.Service != "" {
			if _, exists := dep.Services[opts.Service]; !exists {
				continue
			}
		}
		if !opts.Since.IsZero() && dep.Timestamp.Before(opts.Since) {
			continue
		}
		if !opts.IncludeFailed && dep.Status == state.StatusFailed {
			continue
		}
		result = append(result, dep)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Timestamp.After(result[j].Timestamp)
	})
	if opts.Limit > 0 && len(result) > opts.Limit {
		result = result[:opts.Limit]
	}
	return result
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
