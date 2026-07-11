package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	deployService         string
	skipBuild             bool
	deployYes             bool
	allowDirty            bool
	deployForce           bool
	deploySkipDomainCheck bool
	deployStrictDomains   bool
	deployDomainTimeout   = 2 * time.Minute
	deployDomainTargets   []string
	deployBuildStrategy   string
	deploySource          string
	deployRevision        string
	deployImage           string
	deployArchive         string
	deployPlanOnly        bool
	deployPlanFile        string
)

var blueGreenGraceSleep = time.Sleep

var deployCmd = &cobra.Command{
	Use:          "deploy",
	Short:        "Deploy your application to configured servers",
	SilenceUsage: true,
	Long: `Deploy your application by reconciling desired services on the takod mesh.

The deployment process:
  1. Build or select the service image
  2. Prepare environment takod nodes
  3. Recreate service containers to match desired state
  4. Replicate deployment state

If a step fails, deployment stops and records the failed state for inspection or rollback.

Use 'tako deploy --service web --image registry.example.com/web:sha' to deploy one service from an existing image without building.
Use 'tako deploy --service web --source .' to deploy one service from a targeted build context.
Use 'tako deploy --service web --archive app.tar.gz' to deploy one service from a local source archive.`,
	RunE: runDeploy,
}

func init() {
	rootCmd.AddCommand(deployCmd)
	deployCmd.Flags().StringVar(&deployService, "service", "", "Deploy specific service")
	deployCmd.Flags().BoolVar(&skipBuild, "skip-build", false, "Skip building the service image")
	deployCmd.Flags().BoolVarP(&deployYes, "yes", "y", false, "Skip confirmation prompts (non-interactive mode)")
	deployCmd.Flags().BoolVar(&allowDirty, "allow-dirty", false, "Allow deploying with uncommitted local changes")
	deployCmd.Flags().BoolVar(&deployForce, "force", false, "Reconcile selected services even when no config drift is detected")
	deployCmd.Flags().BoolVar(&deploySkipDomainCheck, "skip-domain-check", false, "Skip post-deploy DNS/TLS checks for public domains")
	deployCmd.Flags().BoolVar(&deployStrictDomains, "strict-domains", false, "Fail deploy if public domains are not DNS/TLS active after waiting")
	deployCmd.Flags().DurationVar(&deployDomainTimeout, "domain-timeout", 2*time.Minute, "Wait up to this duration for public DNS/TLS readiness; 0 checks once")
	deployCmd.Flags().StringArrayVar(&deployDomainTargets, "domain-target", nil, "Expected DNS target; repeat for custom edge/CNAME targets (defaults to proxy server hosts)")
	deployCmd.Flags().StringVar(&deployBuildStrategy, "build-strategy", "", "Override image build strategy: remote, local, or auto")
	deployCmd.Flags().StringVar(&deploySource, "source", "", "Deploy configured project from a non-git source label/path; with --service, override that service build context")
	deployCmd.Flags().StringVar(&deployRevision, "revision", "", "Explicit non-git source revision/build tag")
	deployCmd.Flags().StringVar(&deployImage, "image", "", "Override target service image for this deploy")
	deployCmd.Flags().StringVar(&deployArchive, "archive", "", "Deploy target service from a local source archive (.tar, .tar.gz, .tgz, .zip)")
	deployCmd.Flags().BoolVar(&deployPlanOnly, "plan-only", false, "Compute and show the deployment plan without applying it")
	deployCmd.Flags().StringVar(&deployPlanFile, "plan", "", "Path to a reviewed plan document; apply fails if the computed plan drifted from it")
}

func loadDeployConfig(configPath string) (*config.Config, error) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, &engine.InvalidRequestError{Err: formatDeployConfigError(resolveDeployConfigPath(configPath), err)}
	}
	return cfg, nil
}

func resolveDeployConfigPath(configPath string) string {
	if configPath != "" {
		return configPath
	}
	if _, err := os.Stat("tako.yaml"); err == nil {
		return "tako.yaml"
	}
	if _, err := os.Stat("tako.json"); err == nil {
		return "tako.json"
	}
	return "tako.yaml"
}

func runDeploy(cmd *cobra.Command, args []string) error {
	cfg, err := loadDeployConfig(cfgFile)
	if err != nil {
		return err
	}
	for _, warning := range config.ValidationWarnings(cfg) {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", warning.Message)
	}

	request := engine.DeployRequest{
		Config:          cfg,
		Environment:     getEnvironmentName(cfg),
		Service:         deployService,
		Image:           deployImage,
		Source:          deploySource,
		Archive:         deployArchive,
		Revision:        deployRevision,
		BuildStrategy:   deployBuildStrategy,
		SkipBuild:       skipBuild,
		AllowDirty:      allowDirty,
		Force:           deployForce,
		Verbose:         verbose,
		SkipDomainCheck: deploySkipDomainCheck,
		StrictDomains:   deployStrictDomains,
		DomainTimeout:   deployDomainTimeout,
		DomainTargets:   deployDomainTargets,
	}

	session, err := cliEngine().PlanDeploy(cmd.Context(), request)
	if err != nil {
		return err
	}
	defer session.Close()

	if deployPlanFile != "" {
		if err := verifyPlanFileMatches(deployPlanFile, session.Plan()); err != nil {
			return err
		}
	}
	if deployPlanOnly {
		return emitResultDocument(session.Plan())
	}

	if session.NeedsConfirmation() && !deployYes {
		reason := "deployment plan includes destructive changes"
		if machineOutputEnabled() {
			if err := emitResultDocument(newConfirmationRequiredDocument(reason, session.Plan())); err != nil {
				return err
			}
			return &engine.ConfirmationRequiredError{Reason: reason}
		}
		confirmed, err := confirmDeployAction("\nProceed with deployment? (y/N): ", reason)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Println("Deployment cancelled")
			return nil
		}
	}

	result, err := session.Apply(cmd.Context())
	if result != nil {
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	return err
}

// isNonInteractive checks if running in non-interactive mode
func isNonInteractive() bool {
	return truthyEnv("TAKO_NONINTERACTIVE") || truthyEnv("CI")
}

func truthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func confirmDeployAction(prompt string, reason string) (bool, error) {
	if err := requireDeployPromptAllowed(reason); err != nil {
		return false, err
	}

	fmt.Print(prompt)
	response, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("failed to read confirmation: %w", err)
	}

	return isAffirmative(response), nil
}

func requireDeployPromptAllowed(reason string) error {
	if isNonInteractive() {
		return fmt.Errorf("%s; rerun with --yes to approve in non-interactive mode", reason)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("%s; confirmation requires a terminal or --yes", reason)
	}
	return nil
}

func isAffirmative(response string) bool {
	switch strings.ToLower(strings.TrimSpace(response)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// The deploy pipeline lives in pkg/engine; the aliases below keep the
// historical cmd-level names for the other commands and tests that still
// reference them. They are removed as each command moves onto the engine.

type deployGitReader = engine.GitReader

type deploySourceInfo = engine.SourceInfo

type deployGitStrings = engine.GitStrings

type remoteDeploymentSaver = engine.RemoteDeploymentSaver

type localDeploymentSaver = engine.LocalDeploymentSaver

type deployServiceRemover = engine.ServiceRemover

type takodRevisionPruner = engine.RevisionPruner

func ensureDeployRuntimeSupported(cfg *config.Config) error {
	return engine.EnsureDeployRuntimeSupported(cfg)
}

func validateDeployImageOptions(serviceName string, imageRef string, source string) (string, error) {
	return engine.ValidateImageOptions(serviceName, imageRef, source)
}

func validateDeployArchiveOptions(serviceName string, archivePath string, source string, imageRef string) (string, error) {
	return engine.ValidateArchiveOptions(serviceName, archivePath, source, imageRef)
}

func isSupportedDeployArchive(path string) bool {
	return engine.IsSupportedArchive(path)
}

func deploySourceLabelForImageOverride(source string, imageRef string) string {
	return engine.SourceLabelForImageOverride(source, imageRef)
}

func applyDeployImageOverride(service config.ServiceConfig, imageRef string) config.ServiceConfig {
	return engine.ApplyImageOverride(service, imageRef)
}

func applyDeploySourceOverride(service config.ServiceConfig, source string) config.ServiceConfig {
	return engine.ApplySourceOverride(service, source)
}

func deploySourceLabelForArchive(archivePath string) string {
	return engine.SourceLabelForArchive(archivePath)
}

func applyDeployArchiveOverride(service config.ServiceConfig, buildContext string) config.ServiceConfig {
	return engine.ApplyArchiveOverride(service, buildContext)
}

func deployArchiveBuildTag(explicitRevision string, archivePath string) (string, error) {
	return engine.ArchiveBuildTag(explicitRevision, archivePath)
}

func extractDeployArchive(archivePath string, destDir string) error {
	return engine.ExtractArchive(archivePath, destDir)
}

func resolveDeploySourceInfo(gitClient deployGitReader, allowDirty bool, source string, revision string, imageRef string, now time.Time) (deploySourceInfo, error) {
	return engine.ResolveSourceInfo(gitClient, allowDirty, source, revision, imageRef, now)
}

func deployStartNotificationMessage(project string, version string, envName string, revisionLabel string, revisionValue string, commitMessage string) string {
	return engine.StartNotificationMessage(project, version, envName, revisionLabel, revisionValue, commitMessage)
}

func deployGitStringsFromCommit(commitInfo *git.CommitInfo) deployGitStrings {
	return engine.GitStringsFromCommit(commitInfo)
}

func resolveDeployCommitInfo(gitClient deployGitReader, allowDirty bool) (*git.CommitInfo, string, error) {
	return engine.ResolveCommitInfo(gitClient, allowDirty)
}

func recordFailedDeploymentState(
	remoteSaver remoteDeploymentSaver,
	localSaver localDeploymentSaver,
	deployment *remotestate.DeploymentState,
	cfg *config.Config,
	envName string,
	serverNames []string,
	commitInfo *git.CommitInfo,
	startTime time.Time,
	deploymentErr error,
) error {
	return engine.RecordFailedDeploymentState(remoteSaver, localSaver, deployment, cfg, envName, serverNames, commitInfo, startTime, deploymentErr)
}

func retiredDeploymentServers(previous []string, current []string) []string {
	return engine.RetiredDeploymentServers(previous, current)
}

func deploymentSuccessStatus(manualPending []string) remotestate.DeploymentStatus {
	return engine.DeploymentSuccessStatus(manualPending)
}

func deployActualStateError(err error) error {
	return engine.ActualStateError(err)
}

func deployRemoteHistoryError(err error) error {
	return engine.RemoteHistoryError(err)
}

func applyDeployRemovals(remover deployServiceRemover, plan *reconcile.ReconciliationPlan) error {
	return cliEngine().ApplyRemovals(remover, plan)
}

func reconcileDeployProxy(deploy *deployer.Deployer, services map[string]config.ServiceConfig, activeRevisions map[string]string) error {
	return engine.ReconcileProxy(deploy, services, activeRevisions)
}

func pruneTakodServiceRevisionsAfterGrace(pruner takodRevisionPruner, services map[string]config.ServiceConfig, keepRevisions map[string]string) error {
	return cliEngine().PruneRevisionsAfterGrace(pruner, services, keepRevisions, blueGreenGraceSleep)
}
