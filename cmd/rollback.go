package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/hooks"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/spf13/cobra"
)

var (
	rollbackService           string
	rollbackSkipHooks         bool
	rollbackIncludePersistent bool
	rollbackRunRelease        bool
	rollbackPlanOnly          bool
	rollbackPlanFile          string
	rollbackYes               bool
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback [target]",
	Short: "Roll the environment or one service back to an earlier deployment",
	Long: `Roll back to an earlier deployment recorded in the mesh history.

Without --service, rollback restores every eligible service in the
environment to the state it had at the target deployment, as one operation
under one lock and lease set:
  - Each service's target is its entry in the newest stable record at or
    before the target; services already in that state are left untouched.
  - kind: run and kind: job services are never rolled back (jobs keep the
    newer image and schedule); persistent: true services are skipped unless
    --include-persistent is given; services added after the target keep
    running.
  - deploy.release commands are not re-run unless --run-release is given.
  - Every target image must be retained on a node, rebuildable from a commit
    in the local repository, or pullable from a registry before anything
    changes. 'tako cleanup' can remove old images.
  - Services roll back in dependency order and stop at the first failure.
  - The plan is shown and confirmed first; use -y/--yes to approve without a
    prompt, --plan-only to review, and --plan <file> to apply only a
    reviewed plan.

With --service, only that service rolls back, without a prompt, and its
deploy.release command runs as before.

The target is optional. Without it, rollback picks the previous stable
deployment (project) or the previous stable deployment of the service. It
can be a deployment ID, the short ID 'tako history' prints (a unique ID
prefix), or a git commit (full hash or at least 7 characters).

Rollback reads the freshest reachable deployment history from the mesh.

Examples:
  tako rollback                               # Roll the environment back to the previous deployment
  tako rollback 1767225600 -y                 # Roll back to the deployment shown as 1767225600 in 'tako history'
  tako rollback abc1234 --plan-only           # Review a rollback to commit abc1234
  tako rollback --plan-only --output json > plan.json
  tako rollback --plan plan.json -y           # Apply only the reviewed plan
  tako rollback --service web                 # Roll back one service to its previous deployment
  tako rollback --service web 1767225600      # Roll back one service to a specific deployment`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRollback,
}

func init() {
	rootCmd.AddCommand(rollbackCmd)
	rollbackCmd.Flags().StringVar(&rollbackService, "service", "", "Roll back only this service (no prompt; runs its release command)")
	rollbackCmd.Flags().BoolVar(&rollbackSkipHooks, "skip-hooks", false, skipHooksFlagUsage)
	rollbackCmd.Flags().BoolVar(&rollbackIncludePersistent, "include-persistent", false, "Also roll back persistent: true services (volume data is not rolled back)")
	rollbackCmd.Flags().BoolVar(&rollbackRunRelease, "run-release", false, "Re-run each rolled-back service's deploy.release command from its target image")
	rollbackCmd.Flags().BoolVar(&rollbackPlanOnly, "plan-only", false, "Print the rollback plan and exit without changing anything")
	rollbackCmd.Flags().StringVar(&rollbackPlanFile, "plan", "", "Apply only if the current plan matches this reviewed --plan-only document")
	rollbackCmd.Flags().BoolVarP(&rollbackYes, "yes", "y", false, "Approve the project rollback without a confirmation prompt")
}

// rollbackProjectOnlyFlags names the flags that apply only to a project
// rollback.
var rollbackProjectOnlyFlags = []string{"include-persistent", "run-release", "plan-only", "plan", "yes"}

func validateRollbackFlags(cmd *cobra.Command) error {
	if rollbackService == "" {
		return nil
	}
	for _, name := range rollbackProjectOnlyFlags {
		if flag := cmd.Flags().Lookup(name); flag != nil && flag.Changed {
			return &engine.InvalidRequestError{Err: fmt.Errorf("--%s applies only to a project rollback and cannot be combined with --service", name)}
		}
	}
	return nil
}

func runRollback(cmd *cobra.Command, args []string) error {
	if err := validateRollbackFlags(cmd); err != nil {
		return err
	}
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
	}
	envName := getEnvironmentName(cfg)

	request := engine.RollbackRequest{
		Config:            cfg,
		Environment:       envName,
		Service:           rollbackService,
		DeploymentID:      firstArg(args),
		Verbose:           verbose,
		IncludePersistent: rollbackIncludePersistent,
		RunRelease:        rollbackRunRelease,
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

	hookSession, err := newHookSession(hookCommandRollback, cfg, resolveDeployConfigPath(cfgFile), rollbackSkipHooks)
	if err != nil {
		return err
	}
	if request.Scope() == engine.RollbackScopeProject {
		return runProjectRollback(cmd, cfg, request, hookSession)
	}
	// Every event of the command, including hook and result events, carries
	// data.operation; data.targetDeploymentId follows once it is known.
	stream := cliEngine().EventStream()
	defer stream.SetContextData(map[string]any{"operation": "rollback"})()
	// pre-rollback runs inside the engine once the target is selected and
	// the lock and leases are held; post hooks run after the engine
	// released them.
	var target engine.MutationInfo
	request.BeforeMutation = hookSession.beforeMutation(&target)

	result, err := runRollbackEngine(cmd.Context(), request)
	targetID := target.TargetDeploymentID
	if result != nil {
		targetID = result.DeploymentID
	}
	if targetID != "" {
		defer stream.SetContextData(map[string]any{"targetDeploymentId": targetID})()
	}
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
			result = &engine.RollbackResult{
				APIVersion:   takoapi.APIVersionCurrent,
				Kind:         engine.KindRollbackResult,
				Project:      cfg.Project.Name,
				Environment:  envName,
				Scope:        engine.RollbackScopeService,
				Service:      rollbackService,
				DeploymentID: target.TargetDeploymentID,
				Status:       takoapi.StatusFailed,
				StartedAt:    time.Now(),
				Error:        cliEngine().RedactSensitive(errorText(err)),
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

// runRollbackEngine runs the engine rollback. Seam for tests.
var runRollbackEngine = func(ctx context.Context, request engine.RollbackRequest) (*engine.RollbackResult, error) {
	return cliEngine().Rollback(ctx, request)
}

// rollbackSessionAPI is the slice of *engine.RollbackSession the project
// rollback drives.
type rollbackSessionAPI interface {
	Plan() engine.RollbackPlan
	Apply(ctx context.Context) (*engine.RollbackResult, error)
	Close()
}

// planRollback plans a project rollback and takes the lock and leases.
// Seam for tests.
var planRollback = func(ctx context.Context, request engine.RollbackRequest) (rollbackSessionAPI, error) {
	session, err := cliEngine().PlanRollback(ctx, request)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// rollbackPromptAllowed reports whether a human prompt may run. Seam for
// tests.
var rollbackPromptAllowed = requireDeployPromptAllowed

// confirmRollbackPrompt asks a human to approve the rendered plan. Seam for
// tests.
var confirmRollbackPrompt = func() (bool, error) {
	fmt.Print("\nProceed with rollback? [y/N]: ")
	response, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("failed to read confirmation: %w", err)
	}
	return isAffirmative(response), nil
}

// runProjectRollback plans the project rollback, verifies a reviewed plan,
// asks for approval, and applies it.
func runProjectRollback(cmd *cobra.Command, cfg *config.Config, request engine.RollbackRequest, hookSession *hookSession) error {
	stream := cliEngine().EventStream()
	restoreEvents := stream.SetContextData(map[string]any{"operation": "rollback"})
	defer restoreEvents()

	var target engine.MutationInfo
	request.BeforeMutation = hookSession.beforeMutation(&target)

	session, err := planRollback(cmd.Context(), request)
	if err != nil {
		var unavailable *engine.RollbackUnavailableError
		if errors.As(err, &unavailable) && unavailable.Result != nil {
			if emitErr := emitResultDocument(unavailable.Result); emitErr != nil {
				return emitErr
			}
		}
		return err
	}
	defer session.Close()
	plan := session.Plan()
	defer stream.SetContextData(map[string]any{"targetDeploymentId": plan.Target.ID})()

	if rollbackPlanFile != "" {
		if err := verifyPlanFileMatches(rollbackPlanFile, plan); err != nil {
			return err
		}
	}
	if rollbackPlanOnly {
		if machineOutputEnabled() {
			return emitResultDocument(plan)
		}
		renderRollbackPlan(humanOut(), plan)
		return nil
	}

	needsApproval := len(plan.RolledBackServices()) > 0
	if !machineOutputEnabled() {
		renderRollbackPlan(humanOut(), plan)
	}
	if needsApproval && !rollbackYes {
		reason := fmt.Sprintf("rollback replaces the running version of %d service(s)", len(plan.RolledBackServices()))
		if machineOutputEnabled() {
			document := newOperationConfirmationRequiredDocument(reason, "rollback", plan.Project, plan.Environment, plan.Servers)
			document.Plan = plan
			if err := emitResultDocument(document); err != nil {
				return err
			}
			return &engine.ConfirmationRequiredError{Reason: reason}
		}
		// Like remove (and unlike deploy), a prompt that is not allowed is a
		// confirmation-required error: exit 2.
		if err := rollbackPromptAllowed(reason); err != nil {
			return &engine.ConfirmationRequiredError{Reason: err.Error()}
		}
		confirmed, err := confirmRollbackPrompt()
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(humanOut(), "Rollback cancelled; nothing was changed")
			return nil
		}
	}

	result, err := session.Apply(cmd.Context())
	// Post hooks run after the lock and leases are released.
	session.Close()
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
			result = &engine.RollbackResult{
				APIVersion:   takoapi.APIVersionCurrent,
				Kind:         engine.KindRollbackResult,
				Project:      cfg.Project.Name,
				Environment:  request.Environment,
				Scope:        engine.RollbackScopeProject,
				DeploymentID: plan.Target.ID,
				Version:      plan.Target.Version,
				Services:     notAttemptedRollbackServices(plan.Services),
				Status:       takoapi.StatusFailed,
				StartedAt:    time.Now(),
				Error:        cliEngine().RedactSensitive(errorText(err)),
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

// notAttemptedRollbackServices reports a plan whose apply never started.
func notAttemptedRollbackServices(planned []engine.RollbackServiceOutcome) []engine.RollbackServiceOutcome {
	services := append([]engine.RollbackServiceOutcome(nil), planned...)
	for index := range services {
		if services[index].Action == engine.RollbackActionRolledBack {
			services[index].Action = engine.RollbackActionNotAttempted
		}
	}
	return services
}

// renderRollbackPlan prints the plan table for humans.
func renderRollbackPlan(out io.Writer, plan engine.RollbackPlan) {
	target := plan.Target
	fmt.Fprintf(out, "\nRollback plan for %s (%s)\n", plan.Project, plan.Environment)
	fmt.Fprintf(out, "  Target:   %s (%s)\n", target.DisplayID, target.ID)
	if !target.Timestamp.IsZero() {
		fmt.Fprintf(out, "  Deployed: %s\n", target.Timestamp.Local().Format("2006-01-02 15:04:05"))
	}
	if target.Commit != "" {
		fmt.Fprintf(out, "  Commit:   %s\n", target.Commit)
	}
	if target.Version != "" {
		fmt.Fprintf(out, "  Version:  %s\n", target.Version)
	}
	if target.Message != "" {
		fmt.Fprintf(out, "  Message:  %s\n", target.Message)
	}
	release := "skipped (use --run-release to run them)"
	if plan.ReleaseCommands == engine.RollbackReleaseRun {
		release = "run from each target image"
	}
	fmt.Fprintf(out, "  Release commands: %s\n\n", release)

	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "SERVICE\tACTION\tFROM\tTO\tREPLICAS\tSOURCE\tNOTE")
	for _, service := range plan.Services {
		replicas := "-"
		if service.Replicas > 0 {
			replicas = fmt.Sprintf("%d", service.Replicas)
		}
		note := service.Reason
		if service.SourceDeploymentID != "" && service.Action == engine.RollbackActionRolledBack {
			note = "from " + remotestate.FormatDeploymentID(service.SourceDeploymentID)
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			service.Name, service.Action, dashIfEmpty(service.FromImage), dashIfEmpty(service.ToImage), replicas, dashIfEmpty(service.ImageSource), dashIfEmpty(note))
	}
	_ = writer.Flush()
	for _, warning := range plan.Warnings {
		fmt.Fprintf(out, "\nWarning: %s", warning)
	}
	if len(plan.Warnings) > 0 {
		fmt.Fprintln(out)
	}
	if len(plan.RolledBackServices()) == 0 {
		fmt.Fprintln(out, "\nNothing to roll back: every service already matches the target.")
	}
}

func dashIfEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
