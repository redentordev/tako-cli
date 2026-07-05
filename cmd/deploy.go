package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/dependency"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/notification"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takod"
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
Use 'tako deploy --service web --source .' to deploy one service from a targeted build context.`,
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
}

func ensureDeployRuntimeSupported(cfg *config.Config) error {
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}
	if !cfg.IsMeshEnabled() {
		return fmt.Errorf("mesh.enabled=false is not supported; single-node deploys use a one-node mesh")
	}
	if cfg.GetStateBackend() != config.StateBackendReplicated {
		return fmt.Errorf("state.backend=%s is not supported; takod deployments use replicated state", cfg.GetStateBackend())
	}
	if cfg.GetDeployConsistency() != config.StateDeployConsistencyLease {
		return fmt.Errorf("state.deployConsistency=%s is not implemented yet; current deploys support lease", cfg.GetDeployConsistency())
	}
	return nil
}

func loadDeployConfig(configPath string) (*config.Config, error) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, formatDeployConfigError(resolveDeployConfigPath(configPath), err)
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

type deployGitReader interface {
	IsRepository() bool
	HasUncommittedChanges() (bool, error)
	GetStatus() (string, error)
	GetCommitInfo(string) (*git.CommitInfo, error)
}

type deploySourceInfo struct {
	CommitInfo    *git.CommitInfo
	DirtyStatus   string
	BuildImageTag string
	StateSource   string
	SourceMode    bool
}

type deployGitStrings struct {
	Hash      string
	ShortHash string
	Branch    string
	Message   string
	Author    string
}

func validateDeployImageOptions(serviceName string, imageRef string, source string) (string, error) {
	trimmedImage := strings.TrimSpace(imageRef)
	if trimmedImage == "" {
		if imageRef != "" {
			return "", fmt.Errorf("--image must not be empty")
		}
		return "", nil
	}
	if strings.TrimSpace(serviceName) == "" {
		return "", fmt.Errorf("--image requires --service to select the target service")
	}
	if strings.TrimSpace(source) != "" {
		return "", fmt.Errorf("--image cannot be combined with --source")
	}
	return trimmedImage, nil
}

func deploySourceLabelForImageOverride(source string, imageRef string) string {
	if strings.TrimSpace(imageRef) != "" && strings.TrimSpace(source) == "" {
		return "image"
	}
	return source
}

func applyDeployImageOverride(service config.ServiceConfig, imageRef string) config.ServiceConfig {
	trimmedImage := strings.TrimSpace(imageRef)
	if trimmedImage == "" {
		return service
	}
	service.Image = trimmedImage
	service.Build = ""
	return service
}

func applyDeploySourceOverride(service config.ServiceConfig, source string) config.ServiceConfig {
	trimmedSource := strings.TrimSpace(source)
	if trimmedSource == "" {
		return service
	}
	service.Build = trimmedSource
	service.Image = ""
	return service
}

func resolveDeploySourceInfo(gitClient deployGitReader, allowDirty bool, source string, revision string, now time.Time) (deploySourceInfo, error) {
	source = strings.TrimSpace(source)
	revision = strings.TrimSpace(revision)
	if source != "" || revision != "" {
		buildTag, err := deployplan.SourceBuildTag(revision, now)
		if err != nil {
			return deploySourceInfo{}, err
		}
		stateSource := source
		if stateSource == "" {
			stateSource = "source"
		}
		return deploySourceInfo{
			BuildImageTag: buildTag,
			StateSource:   stateSource,
			SourceMode:    true,
		}, nil
	}

	commitInfo, dirtyStatus, err := resolveDeployCommitInfo(gitClient, allowDirty)
	if err != nil {
		return deploySourceInfo{}, err
	}
	return deploySourceInfo{
		CommitInfo:    commitInfo,
		DirtyStatus:   dirtyStatus,
		BuildImageTag: commitInfo.Hash,
		StateSource:   "deploy",
	}, nil
}

func deployStartNotificationMessage(project string, version string, envName string, revisionLabel string, revisionValue string, commitMessage string) string {
	message := fmt.Sprintf("Starting deployment of `%s` v%s to `%s`\n%s: `%s`", project, version, envName, revisionLabel, revisionValue)
	if commitMessage != "" {
		message += " - " + commitMessage
	}
	return message
}

func deployGitStringsFromCommit(commitInfo *git.CommitInfo) deployGitStrings {
	if commitInfo == nil {
		return deployGitStrings{}
	}
	return deployGitStrings{
		Hash:      commitInfo.Hash,
		ShortHash: commitInfo.ShortHash,
		Branch:    commitInfo.Branch,
		Message:   commitInfo.Message,
		Author:    commitInfo.Author,
	}
}

func resolveDeployCommitInfo(gitClient deployGitReader, allowDirty bool) (*git.CommitInfo, string, error) {
	if !gitClient.IsRepository() {
		return nil, "", fmt.Errorf("❌ This project is not a Git repository.\n\nPlease initialize Git first:\n  git init\n  git add .\n  git commit -m \"Initial commit\"\n\nGit is required for deployment tracking and rollback functionality.")
	}

	hasChanges, err := gitClient.HasUncommittedChanges()
	if err != nil {
		return nil, "", fmt.Errorf("failed to check git status: %w", err)
	}
	dirtyStatus := ""
	if hasChanges {
		status, err := gitClient.GetStatus()
		if err != nil {
			return nil, "", fmt.Errorf("failed to get git status: %w", err)
		}
		if strings.TrimSpace(status) == "" {
			status = "(dirty worktree)"
		}
		dirtyStatus = strings.TrimSpace(status)
		if !allowDirty {
			return nil, "", fmt.Errorf("cannot deploy with uncommitted changes; commit, stash, or discard changes first:\n%s", dirtyStatus)
		}
	}

	commitInfo, err := gitClient.GetCommitInfo("")
	if err != nil {
		return nil, "", fmt.Errorf("failed to get commit info: %w", err)
	}
	return commitInfo, dirtyStatus, nil
}

type remoteDeploymentSaver interface {
	SaveDeployment(*remotestate.DeploymentState) error
}

type localDeploymentSaver interface {
	SaveDeployment(*localstate.DeploymentState) error
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
	if deployment == nil {
		return fmt.Errorf("deployment state is nil")
	}
	deployment.Status = remotestate.StatusFailed
	deployment.Duration = time.Since(startTime)
	if deploymentErr != nil {
		deployment.Error = deploymentErr.Error()
	} else if deployment.Error == "" {
		deployment.Error = "deployment failed"
	}

	if remoteSaver == nil {
		return fmt.Errorf("remote deployment recorder is nil")
	}
	if err := remoteSaver.SaveDeployment(deployment); err != nil {
		return fmt.Errorf("failed to save failed remote deployment state: %w", err)
	}

	if localSaver != nil {
		localDeployment := &localstate.DeploymentState{
			DeploymentID:    fmt.Sprintf("deploy-%s", startTime.Format("20060102-150405")),
			Timestamp:       startTime,
			Environment:     envName,
			Mode:            cfg.GetRuntimeMode(),
			Servers:         append([]string(nil), serverNames...),
			Status:          "failed",
			DurationSeconds: int(time.Since(startTime).Seconds()),
			TriggeredBy:     remotestate.GetCurrentUser(),
			Notes:           deployment.Error,
		}
		if commitInfo != nil {
			localDeployment.GitCommit = commitInfo.Hash
		}
		if err := localSaver.SaveDeployment(localDeployment); err != nil {
			return fmt.Errorf("failed to save failed local deployment state: %w", err)
		}
	}
	return nil
}

func retiredDeploymentServers(previous []string, current []string) []string {
	currentSet := make(map[string]struct{}, len(current))
	for _, server := range current {
		server = strings.TrimSpace(server)
		if server != "" {
			currentSet[server] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(previous))
	retired := make([]string, 0)
	for _, server := range previous {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		if _, ok := currentSet[server]; ok {
			continue
		}
		if _, ok := seen[server]; ok {
			continue
		}
		seen[server] = struct{}{}
		retired = append(retired, server)
	}
	sort.Strings(retired)
	return retired
}

func warnRetiredDeploymentServers(localStateMgr *localstate.Manager, currentServers []string) {
	if localStateMgr == nil {
		return
	}
	previous, err := localStateMgr.GetCurrentDeployment()
	if err != nil || previous == nil || len(previous.Servers) == 0 {
		return
	}
	retired := retiredDeploymentServers(previous.Servers, currentServers)
	if len(retired) == 0 {
		return
	}
	fmt.Printf("\n⚠ Previous deployment included node(s) no longer in this environment: %s\n", strings.Join(retired, ", "))
	fmt.Println("  Tako cannot stop containers on nodes after their SSH config is removed.")
	fmt.Println("  If the node still exists, re-add it temporarily and run 'tako remove --server <node>' before removing it.")
	fmt.Println("  Use 'tako state forget-node <node> --yes' only to prune replicated state for a retired/destroyed node.")
}

func runDeploy(cmd *cobra.Command, args []string) error {
	// Load deployment configuration
	cfg, err := loadDeployConfig(cfgFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(deployBuildStrategy) != "" {
		if err := cfg.SetBuildStrategy(deployBuildStrategy); err != nil {
			return err
		}
	}
	if err := ensureDeployRuntimeSupported(cfg); err != nil {
		return err
	}
	deployImageRef, err := validateDeployImageOptions(deployService, deployImage, deploySource)
	if err != nil {
		return err
	}
	deploySourceLabel := deploySourceLabelForImageOverride(deploySource, deployImageRef)

	// Initialize source metadata. Default mode requires Git; source mode skips Git validation.
	gitClient := git.NewClient(".")

	sourceInfo, err := resolveDeploySourceInfo(gitClient, allowDirty, deploySourceLabel, deployRevision, time.Now())
	if err != nil {
		return err
	}
	commitInfo := sourceInfo.CommitInfo
	gitStrings := deployGitStringsFromCommit(commitInfo)
	dirtyStatus := sourceInfo.DirtyStatus
	buildImageTag := sourceInfo.BuildImageTag

	// Acquire state lock to prevent concurrent deployments
	stateLock := localstate.NewStateLock(".tako")
	lockInfo, err := stateLock.Acquire("deploy")
	if err != nil {
		return fmt.Errorf("cannot deploy: %w", err)
	}
	defer stateLock.Release(lockInfo)

	if verbose {
		fmt.Printf("→ Acquired deployment lock (ID: %s)\n", lockInfo.ID)
	}

	// Display source info
	if sourceInfo.SourceMode {
		fmt.Printf("\n📦 Deploying source:\n")
		fmt.Printf("  Source:   %s\n", sourceInfo.StateSource)
		fmt.Printf("  Revision: %s\n", buildImageTag)
	} else {
		fmt.Printf("\n📦 Deploying commit:\n")
		fmt.Printf("  Hash:    %s\n", gitStrings.ShortHash)
		fmt.Printf("  Branch:  %s\n", gitStrings.Branch)
		fmt.Printf("  Author:  %s\n", gitStrings.Author)
		fmt.Printf("  Message: %s\n", gitStrings.Message)
	}
	if dirtyStatus != "" {
		fmt.Printf("\n⚠ Deploying with uncommitted local changes (--allow-dirty).\n")
		fmt.Printf("  Deployment history records HEAD only; uncommitted file contents are not recoverable from Git.\n")
		if verbose {
			fmt.Printf("  Dirty files:\n%s\n", dirtyStatus)
		}
	}

	// Get environment and services
	envName := getEnvironmentName(cfg)
	allServices, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	// Create SSH pool
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	// Determine which environment nodes to deploy to.
	envServerNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}
	servers := make(map[string]config.ServerConfig, len(envServerNames))
	serverNames := append([]string(nil), envServerNames...)
	for _, serverName := range serverNames {
		server, exists := cfg.Servers[serverName]
		if !exists {
			return fmt.Errorf("server %s not found in configuration", serverName)
		}
		servers[serverName] = server
	}

	// Determine which services to deploy
	services := allServices
	if deployService != "" {
		service, exists := allServices[deployService]
		if !exists {
			return fmt.Errorf("service %s not found in environment %s", deployService, envName)
		}
		if deployImageRef != "" {
			service = applyDeployImageOverride(service, deployImageRef)
		} else {
			service = applyDeploySourceOverride(service, deploySource)
		}
		services = map[string]config.ServiceConfig{deployService: service}
	}

	fmt.Printf("\n=== Starting deployment ===\n\n")
	fmt.Printf("Project: %s v%s\n", cfg.Project.Name, cfg.Project.Version)
	fmt.Printf("Environment: %s\n", envName)
	fmt.Printf("Runtime: %s\n", cfg.GetRuntimeMode())
	fmt.Printf("State: %s (consistency: %s)\n", cfg.GetStateBackend(), cfg.GetDeployConsistency())
	fmt.Printf("Build strategy: %s\n", cfg.GetBuildStrategy())
	if cfg.IsMeshEnabled() {
		fmt.Printf("Mesh: enabled (%s via %s)\n", cfg.Mesh.NetworkCIDR, cfg.Mesh.Interface)
	} else {
		fmt.Printf("Mesh: disabled\n")
	}
	fmt.Printf("Servers: %d\n", len(servers))
	fmt.Printf("Services: %d\n\n", len(services))

	if len(serverNames) == 0 {
		return fmt.Errorf("no servers configured")
	}

	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, serverNames, "deploy")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Printf("→ Acquired remote deploy leases: %s\n", leaseSet.Summary())
	}

	sourceServerName := serverNames[0]
	sourceServer := servers[sourceServerName]

	// Use one reachable target as the build/source node; runtime state is still
	// persisted and reconciled across the selected mesh.
	sourceClient, err := sshPool.GetOrCreateWithAuth(sourceServer.Host, sourceServer.Port, sourceServer.User, sourceServer.SSHKey, sourceServer.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to server %s: %w", sourceServerName, err)
	}
	stateManager := remotestate.NewStateManagerWithSocket(sourceClient, cfg.Project.Name, envName, sourceServer.Host, takodSocketFromConfig(cfg))

	// Create deployer with pool for takod support
	deploy := deployer.NewDeployerWithPool(sourceClient, cfg, envName, sshPool, verbose)
	deploy.SetCLIVersion(Version)
	deploy.SetSkipBuild(skipBuild)
	if err := deploy.SetTargetServers(serverNames); err != nil {
		return err
	}

	if err := deploy.SetupTakodRuntime(); err != nil {
		return fmt.Errorf("failed to setup takod runtime: %w", err)
	}

	// === AUTO-SYNC STATE ===
	// If local .tako directory doesn't exist but remote state does,
	// automatically sync from remote to help users who cloned the project
	if err := SyncStateOnDeployWithPool(sshPool, cfg, envName); err != nil {
		if verbose {
			fmt.Printf("Warning: auto-sync failed: %v\n", err)
		}
	}

	// === STATE RECONCILIATION ===
	// Compare desired state (config) with actual state (running services)
	// This ensures we properly handle service removals and updates

	if verbose {
		fmt.Printf("\n→ Computing deployment plan...\n")
	}

	// Initialize state manager to track deployments
	localStateMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to initialize local state: %v\n", err)
		}
		localStateMgr = nil // Continue without state management
	}
	warnRetiredDeploymentServers(localStateMgr, serverNames)

	// Gather actual state from running containers across the selected mesh nodes.
	actualState, err := reconcile.GatherActualStateFromServers(sshPool, cfg, envName, serverNames, localStateMgr)
	if err != nil {
		return deployActualStateError(err)
	}

	if verbose && len(actualState) > 0 {
		fmt.Printf("  Found %d running service(s)\n", len(actualState))
	}

	planActualState := actualState
	if deployService != "" {
		planActualState = deployplan.FilterActualStateForServices(actualState, services)
	}

	// Compute reconciliation plan
	plan := reconcile.ComputePlan(cfg.Project.Name, envName, services, planActualState)

	// Show plan to user
	fmt.Println()
	fmt.Print(plan.FormatPlan())

	if plan.NeedsConfirmation() && !deployYes {
		// Ask for confirmation if there are destructive changes
		confirmed, err := confirmDeployAction("\nProceed with deployment? (y/N): ", "deployment plan includes destructive changes")
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Println("Deployment cancelled")
			return nil
		}
	}

	if plan.IsEmpty() && !deployplan.HasBuildServices(services) && !deployForce {
		activeRevisions := deployplan.ProxyActiveRevisions(cfg, envName, services, nil, nil, actualState)
		if err := reconcileDeployProxy(deploy, services, activeRevisions); err != nil {
			return fmt.Errorf("failed to reconcile proxy routes: %w", err)
		}
		fmt.Println("\n✓ All services are up-to-date. Proxy routes reconciled.")
		return nil
	}
	if plan.IsEmpty() {
		if deployForce {
			fmt.Println("\n-> No config drift detected; --force will reconcile selected services anyway.")
		} else if sourceInfo.SourceMode {
			fmt.Println("\n-> No config drift detected; build services will still be reconciled for the current source revision.")
		} else {
			fmt.Println("\n-> No config drift detected; build services will still be reconciled for the current commit.")
		}
	}
	servicesToDeploy := deployplan.ServicesToDeployForPlan(plan, services, deployForce, deployService != "")
	if skipped := deployplan.PersistentServicesSkippedByForce(services, servicesToDeploy, deployForce, deployService != ""); len(skipped) > 0 {
		fmt.Printf("\n-> Skipping persistent service(s) during broad --force: %s\n", strings.Join(skipped, ", "))
		fmt.Println("   Use --service <name> --force when you intentionally need to recreate one.")
	}

	if len(servers) == 1 {
		fmt.Printf("\n🐙 Using takod mesh runtime (one node)\n\n")
	} else {
		fmt.Printf("\n🐙 Using takod mesh runtime (%d nodes)\n\n", len(servers))
	}

	// Log deployment start
	if localStateMgr != nil {
		localStateMgr.LogDeployment(fmt.Sprintf("Starting takod deployment to %s", envName))
		if sourceInfo.SourceMode {
			localStateMgr.LogDeployment(fmt.Sprintf("Source revision: %s", buildImageTag))
		} else {
			localStateMgr.LogDeployment(fmt.Sprintf("Git commit: %s", gitStrings.ShortHash))
		}
	}

	startTime := time.Now()
	deployment := &remotestate.DeploymentState{
		Timestamp:      startTime,
		ProjectName:    cfg.Project.Name,
		Version:        cfg.Project.Version,
		Status:         remotestate.StatusInProgress,
		Services:       make(map[string]remotestate.ServiceState),
		User:           remotestate.GetCurrentUser(),
		Host:           sourceServer.Host,
		GitCommit:      gitStrings.Hash,
		GitCommitShort: gitStrings.ShortHash,
		GitBranch:      gitStrings.Branch,
		GitCommitMsg:   gitStrings.Message,
		GitAuthor:      gitStrings.Author,
		CLIVersion:     Version,
		CLICommit:      GitCommit,
	}

	notificationRevisionLabel := "Commit"
	notificationRevisionValue := gitStrings.ShortHash
	if sourceInfo.SourceMode {
		notificationRevisionLabel = "Revision"
		notificationRevisionValue = buildImageTag
	}

	// Setup notifications if configured
	var notifier *notification.Notifier
	if cfg.Notifications != nil && (cfg.Notifications.Slack != "" || cfg.Notifications.Discord != "" || cfg.Notifications.Webhook != "") {
		notifier = notification.NewNotifier(notification.NotifierConfig{
			SlackWebhook:   cfg.Notifications.Slack,
			DiscordWebhook: cfg.Notifications.Discord,
			Webhook:        cfg.Notifications.Webhook,
		}, verbose)

		// Send deployment started notification
		if err := notifier.Notify(notification.Event{
			Type:        notification.EventDeployStarted,
			Project:     cfg.Project.Name,
			Environment: envName,
			Message:     deployStartNotificationMessage(cfg.Project.Name, cfg.Project.Version, envName, notificationRevisionLabel, notificationRevisionValue, gitStrings.Message),
			Details: map[string]string{
				"version":  cfg.Project.Version,
				"commit":   gitStrings.ShortHash,
				"revision": buildImageTag,
				"branch":   gitStrings.Branch,
				"author":   gitStrings.Author,
				"user":     remotestate.GetCurrentUser(),
				"services": fmt.Sprintf("%d", len(services)),
			},
		}); err != nil && verbose {
			fmt.Printf("  Warning: failed to send start notification: %v\n", err)
		}
	}

	deploymentFailed := false
	var deploymentError error
	imageRefs := deployplan.DefaultDeployImageRefs(cfg, envName, services, buildImageTag)

	// Resolve service deployment order based on dependencies
	resolver := dependency.NewResolver(services, verbose)

	// Optionally infer dependencies from environment variables
	inferredDeps := resolver.InferDependencies()
	resolver.MergeDependencies(inferredDeps)

	// Get deployment order
	deploymentOrder, err := resolver.ResolveOrder()
	if err != nil {
		return fmt.Errorf("failed to resolve service dependencies: %w", err)
	}

	// Deploy each service through takod placement in dependency order
	for _, serviceName := range deploymentOrder {
		service, shouldDeploy := servicesToDeploy[serviceName]
		if !shouldDeploy {
			continue
		}
		fmt.Printf("→ Deploying service: %s\n", serviceName)

		fullImageName := deployplan.ImageRef(cfg, envName, serviceName, service, buildImageTag)

		if service.Image != "" {
			// Use pre-built image
			fullImageName = service.Image
		}
		imageRefs[serviceName] = fullImageName

		deployErr := error(nil)
		if deployplan.ShouldWarmManualPromotionService(serviceName, service, actualState) {
			deployErr = deploy.DeployServiceTakodWarmOnly(serviceName, &service, fullImageName)
		} else {
			deployErr = deploy.DeployServiceTakod(serviceName, &service, fullImageName)
		}
		if deployErr != nil {
			fmt.Printf("  ✗ takod deployment failed: %v\n", deployErr)
			deploymentFailed = true
			deploymentError = fmt.Errorf("takod deployment failed for %s: %w", serviceName, deployErr)
			deployment.Status = remotestate.StatusFailed
			deployment.Error = deployErr.Error()
			break
		}

		fmt.Printf("  ✓ Service %s reconciled by takod\n", serviceName)

		// Save service state
		deployment.Services[serviceName] = remotestate.ServiceState{
			Name:     serviceName,
			Image:    fullImageName,
			Port:     service.Port,
			Replicas: service.Replicas,
			Env:      redactedEnvKeys(service.Env),
		}
	}

	if !deploymentFailed {
		if err := applyDeployRemovals(deploy, plan); err != nil {
			fmt.Printf("  ✗ service removal failed: %v\n", err)
			deploymentFailed = true
			deploymentError = err
			deployment.Status = remotestate.StatusFailed
			deployment.Error = err.Error()
		}
	}

	if !deploymentFailed {
		proxyServices := services
		if deployService != "" {
			proxyServices = cloneServiceMap(allServices)
		}
		manualPending := deployplan.ManualPromotionPendingServices(servicesToDeploy, actualState)
		activeRevisions := deployplan.ProxyActiveRevisions(cfg, envName, proxyServices, servicesToDeploy, imageRefs, actualState)
		if err := reconcileDeployProxy(deploy, proxyServices, activeRevisions); err != nil {
			fmt.Printf("  ✗ proxy reconciliation failed: %v\n", err)
			deploymentFailed = true
			deploymentError = fmt.Errorf("proxy reconciliation failed: %w", err)
			deployment.Status = remotestate.StatusFailed
			deployment.Error = err.Error()
		} else if err := pruneTakodServiceRevisionsAfterGrace(deploy, proxyServices, deployplan.DeployedProxyActiveRevisions(servicesToDeploy, activeRevisions)); err != nil {
			fmt.Printf("  ✗ stale revision cleanup failed: %v\n", err)
			deploymentFailed = true
			deploymentError = fmt.Errorf("stale revision cleanup failed: %w", err)
			deployment.Status = remotestate.StatusFailed
			deployment.Error = err.Error()
		} else if len(manualPending) > 0 {
			fmt.Printf("\n✓ Warming revision ready for manual promotion: %s\n", strings.Join(manualPending, ", "))
			fmt.Printf("  Promote when ready with: tako promote %s -e %s\n", manualPending[0], envName)
		}
	}

	if !deploymentFailed {
		manualPending := deployplan.ManualPromotionPendingServices(servicesToDeploy, actualState)
		deployment.Status = deploymentSuccessStatus(manualPending)
		if len(manualPending) > 0 {
			deployment.Message = fmt.Sprintf("warmed %s for manual promotion", strings.Join(manualPending, ", "))
		}
		deployment.Duration = time.Since(startTime)
		if err := stateManager.SaveDeployment(deployment); err != nil {
			return deployRemoteHistoryError(err)
		}

		finalNodeActualState, err := reconcile.GatherActualStateByServer(sshPool, cfg, envName, serverNames)
		if err != nil {
			return fmt.Errorf("deployment succeeded but failed to gather final actual state: %w", err)
		}
		finalActualState := reconcile.AggregateActualStateByServer(finalNodeActualState)
		runtimeServices := services
		runtimeImageRefs := imageRefs
		if deployService != "" {
			runtimeServices = cloneServiceMap(allServices)
			runtimeImageRefs = deployplan.MergeRuntimeImageRefs(cfg, envName, runtimeServices, imageRefs, finalActualState)
		}
		if err := persistTakodRuntimeState(
			sshPool,
			cfg,
			envName,
			serverNames,
			sourceInfo.StateSource,
			runtimeServices,
			runtimeImageRefs,
			finalActualState,
			finalNodeActualState,
			gitInfoFromCommit(commitInfo),
			"deploy.succeeded",
			fmt.Sprintf("deployed %d service(s)", len(servicesToDeploy)),
			map[string]string{
				"commit":          gitStrings.ShortHash,
				"revision":        buildImageTag,
				"services":        fmt.Sprintf("%d", len(servicesToDeploy)),
				"desiredServices": fmt.Sprintf("%d", len(runtimeServices)),
			},
		); err != nil {
			return fmt.Errorf("deployment succeeded but failed to persist takod state: %w", err)
		}

		// Replicate state to the rest of the mesh.
		if len(servers) > 1 {
			replicator := remotestate.NewStateReplicator(sshPool, cfg, envName, cfg.Project.Name, verbose)
			history, err := stateManager.LoadHistory()
			if err != nil {
				return fmt.Errorf("deployment succeeded but failed to load remote deployment history for replication: %w", err)
			}
			if err := replicator.ReplicateDeployment(deployment, history); err != nil {
				return fmt.Errorf("deployment succeeded but failed to replicate remote deployment history: %w", err)
			}
		}

		// Save local deployment state
		if localStateMgr != nil {
			localDeployment := &localstate.DeploymentState{
				DeploymentID:    fmt.Sprintf("deploy-%s", time.Now().Format("20060102-150405")),
				Timestamp:       startTime,
				Environment:     envName,
				Mode:            cfg.GetRuntimeMode(),
				Servers:         append([]string(nil), serverNames...),
				Status:          string(deployment.Status),
				DurationSeconds: int(time.Since(startTime).Seconds()),
				GitCommit:       gitStrings.Hash,
				TriggeredBy:     remotestate.GetCurrentUser(),
				Notes:           fmt.Sprintf("Deployed %d services to %s runtime", len(servicesToDeploy), cfg.GetRuntimeMode()),
			}
			if err := localStateMgr.SaveDeployment(localDeployment); err != nil && verbose {
				fmt.Printf("Warning: failed to save local deployment state: %v\n", err)
			}
		}

	}

	// Calculate deployment duration
	deploymentDuration := time.Since(startTime)

	if deploymentFailed {
		recordErr := recordFailedDeploymentState(stateManager, localStateMgr, deployment, cfg, envName, serverNames, commitInfo, startTime, deploymentError)
		if recordErr == nil && len(servers) > 1 {
			replicator := remotestate.NewStateReplicator(sshPool, cfg, envName, cfg.Project.Name, verbose)
			history, err := stateManager.LoadHistory()
			if err != nil {
				recordErr = fmt.Errorf("failed to load failed deployment history for replication: %w", err)
			} else if err := replicator.ReplicateDeployment(deployment, history); err != nil {
				recordErr = fmt.Errorf("failed to replicate failed deployment history: %w", err)
			}
		}
		if recordErr != nil && verbose {
			fmt.Printf("Warning: failed to record failed deployment state: %v\n", recordErr)
		}

		// Send failure notification
		if notifier != nil {
			notifier.Notify(notification.Event{
				Type:        notification.EventDeployFailed,
				Project:     cfg.Project.Name,
				Environment: envName,
				Message:     fmt.Sprintf("Deployment of `%s` to `%s` failed after %s", cfg.Project.Name, envName, deploymentDuration.Round(time.Second)),
				Error:       deploymentError.Error(),
				Duration:    deploymentDuration,
				Details: map[string]string{
					"version":  cfg.Project.Version,
					"commit":   gitStrings.ShortHash,
					"revision": buildImageTag,
					"user":     remotestate.GetCurrentUser(),
				},
			})
		}
		if recordErr != nil {
			return fmt.Errorf("takod deployment failed; additionally failed to record failed deployment state: %w", recordErr)
		}
		return fmt.Errorf("takod deployment failed")
	}

	fmt.Printf("\n✓ takod deployment completed!\n")

	// Automatic cleanup after successful deployment.
	if verbose {
		fmt.Printf("\n→ Running automatic cleanup...\n")
	}
	imageRepositories := cleanupImageRepositories(cfg, envName, services)
	externalVolumes := externalVolumeNamesForEnvironment(cfg, envName)
	for serverName, server := range servers {
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err == nil {
			response, cleanupErr := cleanupViaTakod(client, cfg, takod.CleanupRequest{
				Project:                cfg.Project.Name,
				Environment:            envName,
				ImageRepositories:      imageRepositories,
				ExternalVolumes:        externalVolumes,
				KeepImages:             3,
				CleanOldImages:         true,
				CleanStoppedContainers: true,
				CleanDanglingImages:    true,
				CleanBuildCache:        true,
				BuildCacheKeepStorage:  takod.DefaultBuildCacheKeepStorage,
			})
			if cleanupErr != nil && verbose {
				fmt.Printf("  Warning: failed to clean %s: %v\n", serverName, cleanupErr)
				continue
			}
			if cleanupErr == nil && verbose {
				printCleanupWarnings(response)
				fmt.Printf("  ✓ Cleaned up %s\n", serverName)
			}
		}
	}

	fmt.Printf("\n✓ Deployment completed successfully!\n\n")

	// Send success notification
	if notifier != nil {
		// Collect deployed service URLs
		var urls []string
		for _, svc := range services {
			if svc.Proxy != nil && svc.IsPublic() {
				for _, domain := range svc.Proxy.GetAllDomains() {
					urls = append(urls, fmt.Sprintf("https://%s", domain))
				}
			}
		}

		notifier.Notify(notification.Event{
			Type:        notification.EventDeploySucceeded,
			Project:     cfg.Project.Name,
			Environment: envName,
			Message:     fmt.Sprintf("Successfully deployed `%s` v%s to `%s` in %s", cfg.Project.Name, cfg.Project.Version, envName, deploymentDuration.Round(time.Second)),
			Duration:    deploymentDuration,
			Details: map[string]string{
				"version":  cfg.Project.Version,
				"commit":   gitStrings.ShortHash,
				"revision": buildImageTag,
				"branch":   gitStrings.Branch,
				"user":     remotestate.GetCurrentUser(),
				"services": fmt.Sprintf("%d", len(services)),
				"urls":     fmt.Sprintf("%v", urls),
			},
		})
	}

	// Show service URLs (iterate through services with proxy configured)
	hasPublicServices := false
	hasInternalServices := false
	domainSpecs := collectConfiguredDomainSpecs(services, "")

	for serviceName, service := range services {
		if service.Proxy != nil && service.IsPublic() && service.Proxy.GetPrimaryDomain() != "" {
			allDomains := service.Proxy.GetAllDomains()
			if !hasPublicServices {
				fmt.Printf("Your application is available at:\n")
				hasPublicServices = true
			}
			fmt.Printf("\n%s:\n", serviceName)
			for _, domain := range allDomains {
				fmt.Printf("  https://%s\n", domain)
			}
		}
	}

	for serviceName, service := range services {
		if service.Proxy != nil && service.Proxy.IsInternal() && service.Proxy.GetPrimaryHost() != "" {
			if !hasInternalServices {
				fmt.Printf("\nInternal routes:\n")
				hasInternalServices = true
			}
			fmt.Printf("\n%s:\n", serviceName)
			for _, host := range service.Proxy.GetAllHosts() {
				fmt.Printf("  http://%s\n", host)
			}
		}
	}
	if hasInternalServices {
		fmt.Printf("\nRun `tako domains hosts -e %s` to print /etc/hosts entries for internal routes.\n", envName)
	}

	if hasPublicServices && !deploySkipDomainCheck {
		targets, err := domainExpectedTargets(cfg, envName, deployDomainTargets)
		if err != nil {
			if deployStrictDomains {
				return fmt.Errorf("failed to resolve domain check targets: %w", err)
			}
			if verbose {
				fmt.Printf("\n⚠️  Skipping public domain DNS/TLS checks: %v\n", err)
			}
		} else if _, err := monitorDomainStatuses(context.Background(), health.NewHealthChecker(), domainSpecs, domainStatusOptions{
			Timeout:         deployDomainTimeout,
			Strict:          deployStrictDomains,
			ExpectedTargets: targets,
		}); err != nil {
			return err
		}
	} else if hasPublicServices && deploySkipDomainCheck && verbose {
		fmt.Println("\nSkipping public domain DNS/TLS checks (--skip-domain-check).")
	}

	return nil
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

func deployActualStateError(err error) error {
	return fmt.Errorf("failed to gather actual state from takod; refusing to plan against unknown running services: %w", err)
}

func deployRemoteHistoryError(err error) error {
	return fmt.Errorf("deployment succeeded but failed to save remote deployment history: %w", err)
}

type deployServiceRemover interface {
	RemoveServiceTakod(serviceName string) error
}

func applyDeployRemovals(remover deployServiceRemover, plan *reconcile.ReconciliationPlan) error {
	if remover == nil {
		return fmt.Errorf("service remover is nil")
	}
	if plan == nil {
		return nil
	}
	for _, change := range plan.Changes {
		if change.Type != reconcile.ChangeRemove {
			continue
		}
		fmt.Printf("→ Removing service: %s\n", change.ServiceName)
		if err := remover.RemoveServiceTakod(change.ServiceName); err != nil {
			return fmt.Errorf("remove failed for %s: %w", change.ServiceName, err)
		}
		fmt.Printf("  ✓ Service %s removed\n", change.ServiceName)
	}
	return nil
}

func reconcileDeployProxy(deploy *deployer.Deployer, services map[string]config.ServiceConfig, activeRevisions map[string]string) error {
	if len(activeRevisions) > 0 {
		return deploy.ReconcileTakodProxyWithActiveRevisions(services, activeRevisions)
	}
	return deploy.ReconcileTakodProxy(services)
}

type takodRevisionPruner interface {
	PruneTakodServiceRevisions(services map[string]config.ServiceConfig, keepRevisions map[string]string) error
}

func pruneTakodServiceRevisionsAfterGrace(pruner takodRevisionPruner, services map[string]config.ServiceConfig, keepRevisions map[string]string) error {
	if len(keepRevisions) == 0 {
		return nil
	}
	grace, names, err := deployplan.BlueGreenPruneGracePeriod(services, keepRevisions)
	if err != nil {
		return err
	}
	if grace > 0 {
		fmt.Printf("\n-> Retaining previous blue-green revision for %s before pruning: %s\n", grace.Round(time.Millisecond), strings.Join(names, ", "))
		blueGreenGraceSleep(grace)
	}
	return pruner.PruneTakodServiceRevisions(services, keepRevisions)
}

func deploymentSuccessStatus(manualPending []string) remotestate.DeploymentStatus {
	if len(manualPending) > 0 {
		return remotestate.StatusWarmed
	}
	return remotestate.StatusSuccess
}
