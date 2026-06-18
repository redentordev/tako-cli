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
	deployService string
	skipBuild     bool
	deployYes     bool
	allowDirty    bool
	deployForce   bool
)

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

If a step fails, deployment stops and records the failed state for inspection or rollback.`,
	RunE: runDeploy,
}

func init() {
	rootCmd.AddCommand(deployCmd)
	deployCmd.Flags().StringVar(&deployService, "service", "", "Deploy specific service")
	deployCmd.Flags().BoolVar(&skipBuild, "skip-build", false, "Skip building the service image")
	deployCmd.Flags().BoolVarP(&deployYes, "yes", "y", false, "Skip confirmation prompts (non-interactive mode)")
	deployCmd.Flags().BoolVar(&allowDirty, "allow-dirty", false, "Allow deploying with uncommitted local changes")
	deployCmd.Flags().BoolVar(&deployForce, "force", false, "Reconcile selected services even when no config drift is detected")
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
	if err := ensureDeployRuntimeSupported(cfg); err != nil {
		return err
	}

	// Initialize Git client
	gitClient := git.NewClient(".")

	commitInfo, dirtyStatus, err := resolveDeployCommitInfo(gitClient, allowDirty)
	if err != nil {
		return err
	}

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

	// Display commit info
	fmt.Printf("\n📦 Deploying commit:\n")
	fmt.Printf("  Hash:    %s\n", commitInfo.ShortHash)
	fmt.Printf("  Branch:  %s\n", commitInfo.Branch)
	fmt.Printf("  Author:  %s\n", commitInfo.Author)
	fmt.Printf("  Message: %s\n", commitInfo.Message)
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
		services = map[string]config.ServiceConfig{deployService: service}
	}

	fmt.Printf("\n=== Starting deployment ===\n\n")
	fmt.Printf("Project: %s v%s\n", cfg.Project.Name, cfg.Project.Version)
	fmt.Printf("Environment: %s\n", envName)
	fmt.Printf("Runtime: %s\n", cfg.GetRuntimeMode())
	fmt.Printf("State: %s (consistency: %s)\n", cfg.GetStateBackend(), cfg.GetDeployConsistency())
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
		planActualState = filterActualStateForServices(actualState, services)
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

	if plan.IsEmpty() && !hasBuildServices(services) && !deployForce {
		if err := deploy.ReconcileTakodProxy(services); err != nil {
			return fmt.Errorf("failed to reconcile proxy routes: %w", err)
		}
		fmt.Println("\n✓ All services are up-to-date. Proxy routes reconciled.")
		return nil
	}
	if plan.IsEmpty() {
		if deployForce {
			fmt.Println("\n-> No config drift detected; --force will reconcile selected services anyway.")
		} else {
			fmt.Println("\n-> No config drift detected; build services will still be reconciled for the current commit.")
		}
	}
	servicesToDeploy := servicesToDeployForPlan(plan, services, deployForce, deployService != "")
	if skipped := persistentServicesSkippedByForce(services, servicesToDeploy, deployForce, deployService != ""); len(skipped) > 0 {
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
		localStateMgr.LogDeployment(fmt.Sprintf("Git commit: %s", commitInfo.ShortHash))
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
		GitCommit:      commitInfo.Hash,
		GitCommitShort: commitInfo.ShortHash,
		GitBranch:      commitInfo.Branch,
		GitCommitMsg:   commitInfo.Message,
		GitAuthor:      commitInfo.Author,
		CLIVersion:     Version,
		CLICommit:      GitCommit,
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
			Message:     fmt.Sprintf("Starting deployment of `%s` v%s to `%s`\nCommit: `%s` - %s", cfg.Project.Name, cfg.Project.Version, envName, commitInfo.ShortHash, commitInfo.Message),
			Details: map[string]string{
				"version":  cfg.Project.Version,
				"commit":   commitInfo.ShortHash,
				"branch":   commitInfo.Branch,
				"author":   commitInfo.Author,
				"user":     remotestate.GetCurrentUser(),
				"services": fmt.Sprintf("%d", len(services)),
			},
		}); err != nil && verbose {
			fmt.Printf("  Warning: failed to send start notification: %v\n", err)
		}
	}

	deploymentFailed := false
	var deploymentError error
	buildImageTag := commitInfo.Hash
	imageRefs := defaultDeployImageRefs(cfg, envName, services, buildImageTag)

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

		fullImageName := deployImageRef(cfg, envName, serviceName, service, buildImageTag)

		if service.Image != "" {
			// Use pre-built image
			fullImageName = service.Image
		}
		imageRefs[serviceName] = fullImageName

		if err := deploy.DeployServiceTakod(serviceName, &service, fullImageName); err != nil {
			fmt.Printf("  ✗ takod deployment failed: %v\n", err)
			deploymentFailed = true
			deploymentError = fmt.Errorf("takod deployment failed for %s: %w", serviceName, err)
			deployment.Status = remotestate.StatusFailed
			deployment.Error = err.Error()
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
		if err := deploy.ReconcileTakodProxy(proxyServices); err != nil {
			fmt.Printf("  ✗ proxy reconciliation failed: %v\n", err)
			deploymentFailed = true
			deploymentError = fmt.Errorf("proxy reconciliation failed: %w", err)
			deployment.Status = remotestate.StatusFailed
			deployment.Error = err.Error()
		}
	}

	if !deploymentFailed {
		deployment.Status = remotestate.StatusSuccess
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
			runtimeImageRefs = mergeRuntimeImageRefs(cfg, envName, runtimeServices, imageRefs, finalActualState)
		}
		if err := persistTakodRuntimeState(
			sshPool,
			cfg,
			envName,
			serverNames,
			"deploy",
			runtimeServices,
			runtimeImageRefs,
			finalActualState,
			finalNodeActualState,
			gitInfoFromCommit(commitInfo),
			"deploy.succeeded",
			fmt.Sprintf("deployed %d service(s)", len(servicesToDeploy)),
			map[string]string{
				"commit":          commitInfo.ShortHash,
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
				Status:          "success",
				DurationSeconds: int(time.Since(startTime).Seconds()),
				GitCommit:       commitInfo.Hash,
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
					"version": cfg.Project.Version,
					"commit":  commitInfo.ShortHash,
					"user":    remotestate.GetCurrentUser(),
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
			if svc.Proxy != nil {
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
				"commit":   commitInfo.ShortHash,
				"branch":   commitInfo.Branch,
				"user":     remotestate.GetCurrentUser(),
				"services": fmt.Sprintf("%d", len(services)),
				"urls":     fmt.Sprintf("%v", urls),
			},
		})
	}

	// Show service URLs (iterate through services with proxy configured)
	hasPublicServices := false
	servicesWithProxy := []struct {
		name    string
		domains []string
	}{}

	for serviceName, service := range services {
		if service.Proxy != nil && service.Proxy.GetPrimaryDomain() != "" {
			allDomains := service.Proxy.GetAllDomains()
			if !hasPublicServices {
				fmt.Printf("Your application is available at:\n")
				hasPublicServices = true
			}
			fmt.Printf("\n%s:\n", serviceName)
			for _, domain := range allDomains {
				fmt.Printf("  https://%s\n", domain)
			}
			servicesWithProxy = append(servicesWithProxy, struct {
				name    string
				domains []string
			}{serviceName, allDomains})
		}
	}

	// Monitor SSL certificate provisioning if there are public services
	if hasPublicServices {
		fmt.Printf("\n")
		healthChecker := health.NewHealthChecker()

		for _, svc := range servicesWithProxy {
			for _, domain := range svc.domains {
				// Monitor SSL provisioning (max 2 minutes wait)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				if err := healthChecker.MonitorSSLProvisioning(ctx, svc.name, domain, 2*time.Minute); err != nil {
					if verbose {
						fmt.Printf("\n⚠️  SSL certificate not yet available for %s\n", domain)
						fmt.Printf("   This is normal for first deployment. Certificate will be provisioned automatically.\n")
						fmt.Printf("   Re-run tako deploy after DNS propagation to reconcile the service.\n")
					}
				}
				cancel()
				break // Only check first domain per service
			}
		}
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

func filterActualStateForServices(actualState map[string]*reconcile.ActualService, services map[string]config.ServiceConfig) map[string]*reconcile.ActualService {
	if len(actualState) == 0 || len(services) == 0 {
		return map[string]*reconcile.ActualService{}
	}
	filtered := make(map[string]*reconcile.ActualService, len(services))
	for serviceName := range services {
		if actual, ok := actualState[serviceName]; ok {
			filtered[serviceName] = actual
		}
	}
	return filtered
}

func hasBuildServices(services map[string]config.ServiceConfig) bool {
	for _, service := range services {
		if service.Build != "" {
			return true
		}
	}
	return false
}

func servicesToDeployForPlan(plan *reconcile.ReconciliationPlan, services map[string]config.ServiceConfig, force bool, explicitServiceTarget bool) map[string]config.ServiceConfig {
	if len(services) == 0 {
		return map[string]config.ServiceConfig{}
	}
	if plan == nil && !force {
		return cloneServiceMap(services)
	}

	selected := make(map[string]config.ServiceConfig)
	if plan != nil {
		for _, change := range plan.Changes {
			if change.Type != reconcile.ChangeAdd && change.Type != reconcile.ChangeUpdate {
				continue
			}
			service, ok := services[change.ServiceName]
			if ok {
				selected[change.ServiceName] = service
			}
		}
	}

	if force {
		for serviceName, service := range services {
			if explicitServiceTarget || !service.Persistent {
				selected[serviceName] = service
			}
		}
		return selected
	}

	for serviceName, service := range services {
		if service.Build != "" {
			selected[serviceName] = service
		}
	}
	return selected
}

func persistentServicesSkippedByForce(services map[string]config.ServiceConfig, selected map[string]config.ServiceConfig, force bool, explicitServiceTarget bool) []string {
	if !force || explicitServiceTarget || len(services) == 0 {
		return nil
	}
	var skipped []string
	for serviceName, service := range services {
		if !service.Persistent {
			continue
		}
		if _, ok := selected[serviceName]; ok {
			continue
		}
		skipped = append(skipped, serviceName)
	}
	sort.Strings(skipped)
	return skipped
}
