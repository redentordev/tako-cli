package cmd

import (
	"fmt"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

var scaleCmd = &cobra.Command{
	Use:   "scale SERVICE=REPLICAS [SERVICE=REPLICAS...]",
	Short: "Scale takod services to specified replicas",
	Long: `Scale one or more services to a specified number of replicas.

This command reconciles running takod containers without rebuilding images.

Examples:
  tako scale web=5
  tako scale api=3 web=2
  tako scale worker=10
  tako scale web=0

Note: this changes runtime state immediately. Update replicas in tako.yaml if
you want the next full deploy to preserve the same count.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runScale,
}

func init() {
	rootCmd.AddCommand(scaleCmd)
}

func runScale(cmd *cobra.Command, args []string) error {
	return runScaleTargets(cmd, args)
}

// runScaleTargets is the shared adapter for scale, start, and stop: it parses
// SERVICE=REPLICAS targets, loads config, and runs the engine scale pipeline.
func runScaleTargets(cmd *cobra.Command, args []string) error {
	scaleTargets, err := engine.ParseScaleTargets(args)
	if err != nil {
		return err
	}

	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	request := engine.ScaleRequest{
		Config:      cfg,
		Environment: getEnvironmentName(cfg),
		Targets:     scaleTargets,
		Verbose:     verbose,
	}

	_, err = cliEngine().Scale(cmd.Context(), request)
	return err
}

// The scale pipeline lives in pkg/engine; the aliases below keep the
// historical cmd-level names for the tests that still reference them.

func scaleTargetServers(cfg *config.Config, envName string) ([]string, error) {
	return engine.ScaleTargetServers(cfg, envName)
}

func scaleTargetSummary(targets map[string]int) string {
	return engine.ScaleTargetSummary(targets)
}

func buildScaleDeploymentState(
	cfg *config.Config,
	envName string,
	host string,
	startTime time.Time,
	duration time.Duration,
	scaleTargets map[string]int,
	services map[string]config.ServiceConfig,
	imageRefs map[string]string,
) *remotestate.DeploymentState {
	return engine.BuildScaleDeploymentState(cfg, envName, host, startTime, duration, scaleTargets, services, imageRefs, Version, GitCommit)
}
