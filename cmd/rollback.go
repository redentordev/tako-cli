package cmd

import (
	"fmt"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/spf13/cobra"
)

var (
	rollbackService string
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback [deployment-id]",
	Short: "Rollback to previous deployment",
	Long: `Rollback to a previous deployment.

If no deployment-id is provided, rolls back to the most recent successful deployment.
Specify a deployment-id to rollback to a specific deployment.

Rollback reads the freshest reachable deployment history from the mesh.

Use 'tako history' to view available deployments.

Examples:
  tako rollback --service web                 # Rollback to previous deployment
  tako rollback --service web deploy-123      # Rollback to specific deployment`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRollback,
}

func init() {
	rootCmd.AddCommand(rollbackCmd)
	rollbackCmd.Flags().StringVar(&rollbackService, "service", "", "Service to rollback (required)")
	rollbackCmd.MarkFlagRequired("service")
}

func runRollback(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	envName := getEnvironmentName(cfg)

	request := engine.RollbackRequest{
		Config:       cfg,
		Environment:  envName,
		Service:      rollbackService,
		DeploymentID: firstArg(args),
		Verbose:      verbose,
		// The mesh history helpers are shared with other commands (state,
		// history) and stay in cmd; the engine consumes them through these
		// seams.
		HistorySource: func() (string, *remotestate.DeploymentHistory, error) {
			candidate, err := selectRollbackHistorySource(cfg, envName, "")
			if err != nil {
				return "", nil, err
			}
			return candidate.source, candidate.history, nil
		},
		ListDeployments: listDeploymentsFromHistory,
	}

	result, err := cliEngine().Rollback(cmd.Context(), request)
	if result != nil {
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	return err
}

func selectRollbackHistorySource(cfg *config.Config, envName string, requestedServer string) (stateHistoryCandidate, error) {
	histories, err := collectStateDeploymentHistories(cfg, envName, requestedServer, !verbose)
	if err != nil {
		return stateHistoryCandidate{}, fmt.Errorf("failed to load rollback history: %w", err)
	}
	best, ok := bestDeploymentHistory(histories)
	if !ok {
		if requestedServer != "" {
			return stateHistoryCandidate{}, fmt.Errorf("no deployment history found on node %s", requestedServer)
		}
		return stateHistoryCandidate{}, fmt.Errorf("no deployment history found on reachable mesh nodes")
	}
	return best, nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// The rollback pipeline lives in pkg/engine; the aliases below keep the
// historical cmd-level names for the tests that still reference them.

func deploymentFromHistory(history *remotestate.DeploymentHistory, deploymentID string) (*remotestate.DeploymentState, error) {
	return engine.DeploymentFromHistory(history, deploymentID)
}

func selectRollbackTargetFromHistory(history *remotestate.DeploymentHistory, deploymentID string, serviceName string) (*remotestate.DeploymentState, error) {
	return engine.SelectRollbackTargetFromHistory(history, deploymentID, serviceName, listDeploymentsFromHistory)
}

func previousStableServiceDeploymentFromHistory(history *remotestate.DeploymentHistory, serviceName string) (*remotestate.DeploymentState, error) {
	return engine.PreviousStableServiceDeploymentFromHistory(history, serviceName, listDeploymentsFromHistory)
}

func rollbackRemoteHistoryError(err error) error {
	return engine.RollbackRemoteHistoryError(err)
}

func buildRollbackDeployment(
	cfg *config.Config,
	envName string,
	host string,
	startTime time.Time,
	duration time.Duration,
	targetDeployment *remotestate.DeploymentState,
	serviceName string,
	serviceState remotestate.ServiceState,
) *remotestate.DeploymentState {
	return engine.BuildRollbackDeployment(cfg, envName, host, startTime, duration, targetDeployment, serviceName, serviceState, Version, GitCommit)
}

func rollbackProxyInputs(
	cfg *config.Config,
	envName string,
	services map[string]config.ServiceConfig,
	rollbackService string,
	serviceState remotestate.ServiceState,
	actualState map[string]*reconcile.ActualService,
) (map[string]config.ServiceConfig, map[string]string, map[string]string) {
	return engine.RollbackProxyInputs(cfg, envName, services, rollbackService, serviceState, actualState)
}

func rollbackNeedsTargetWorktree(service config.ServiceConfig, targetDeployment *remotestate.DeploymentState) bool {
	return engine.RollbackNeedsTargetWorktree(service, targetDeployment)
}

func rollbackTargetCommit(targetDeployment *remotestate.DeploymentState) string {
	return engine.RollbackTargetCommit(targetDeployment)
}

func withWorkingDirectory(dir string, fn func() error) error {
	return engine.WithWorkingDirectory(dir, fn)
}
