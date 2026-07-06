package cmd

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/spf13/cobra"
)

var promoteRevision string

var promoteCmd = &cobra.Command{
	Use:          "promote SERVICE",
	Short:        "Promote a warmed blue-green revision",
	SilenceUsage: true,
	Long: `Promote a warmed blue-green revision.

For services using deploy.strategy=blue_green and promotion=manual, deploy warms
the new revision without moving public traffic. promote switches proxy routes to
the warmed revision, prunes stale revisions, and persists the promoted state.`,
	Args: cobra.ExactArgs(1),
	RunE: runPromote,
}

func init() {
	rootCmd.AddCommand(promoteCmd)
	promoteCmd.Flags().StringVar(&promoteRevision, "revision", "", "Specific warmed revision to promote (full value or unique prefix)")
}

func runPromote(cmd *cobra.Command, args []string) error {
	serviceName := strings.TrimSpace(args[0])
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	request := engine.PromoteRequest{
		Config:      cfg,
		Environment: getEnvironmentName(cfg),
		ServiceName: serviceName,
		Revision:    promoteRevision,
		Verbose:     verbose,
		// shortRevision stays in cmd because `tako ps` also uses it; the
		// engine takes it as a display-formatting seam.
		ShortRevision: shortRevision,
	}

	_, err = cliEngine().Promote(cmd.Context(), request)
	return err
}

// The promote pipeline lives in pkg/engine; the alias below keeps the
// historical cmd-level name for the tests that still reference it.

func selectPromotionRevision(actual *reconcile.ActualService, requested string) (string, error) {
	return engine.SelectPromotionRevision(actual, requested)
}
