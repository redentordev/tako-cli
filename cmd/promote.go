package cmd

import (
	"context"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/hooks"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/spf13/cobra"
)

var (
	promoteRevision  string
	promoteSkipHooks bool
)

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
	promoteCmd.Flags().BoolVar(&promoteSkipHooks, "skip-hooks", false, skipHooksFlagUsage)
}

func runPromote(cmd *cobra.Command, args []string) error {
	serviceName := strings.TrimSpace(args[0])
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
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

	hookSession, err := newHookSession(hookCommandPromote, cfg, resolveDeployConfigPath(cfgFile), promoteSkipHooks)
	if err != nil {
		return err
	}
	// pre-promote runs inside the engine once the warmed revision and image
	// are selected and the lock and leases are held.
	var target engine.MutationInfo
	request.BeforeMutation = hookSession.beforeMutation(&target)

	result, err := runPromoteEngine(cmd.Context(), request)
	err = hookSession.finish(cmd.Context(), err, func() hooks.Context {
		values := mutationHookContext(target)
		if result != nil {
			values.Result = string(result.Status)
			values.DurationSeconds = durationPointer(result.Duration)
		}
		return values
	}, func() hooks.Context { return mutationHookContext(target) })
	if outcomes := hookSession.hookOutcomes(); outcomes != nil {
		if result == nil {
			result = &engine.PromoteResult{
				APIVersion:  takoapi.APIVersionCurrent,
				Kind:        engine.KindPromoteResult,
				Project:     cfg.Project.Name,
				Environment: request.Environment,
				Service:     serviceName,
				Revision:    target.Revision,
				Image:       target.Image,
				Status:      takoapi.StatusFailed,
				StartedAt:   time.Now(),
				Error:       cliEngine().RedactSensitive(errorText(err)),
			}
		}
		result.Hooks = outcomes
	}
	if result != nil {
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	return err
}

// runPromoteEngine runs the engine promotion. Seam for tests.
var runPromoteEngine = func(ctx context.Context, request engine.PromoteRequest) (*engine.PromoteResult, error) {
	return cliEngine().Promote(ctx, request)
}

// The promote pipeline lives in pkg/engine; the alias below keeps the
// historical cmd-level name for the tests that still reference it.

func selectPromotionRevision(actual *reconcile.ActualService, requested string) (string, error) {
	return engine.SelectPromotionRevision(actual, requested)
}
