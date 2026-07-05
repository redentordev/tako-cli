package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takodstate"
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
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}
	service, ok := services[serviceName]
	if !ok {
		return fmt.Errorf("service %s not found in environment %s", serviceName, envName)
	}
	if service.Deploy.Strategy != config.DeployStrategyBlueGreen {
		return fmt.Errorf("service %s does not use deploy.strategy=blue_green", serviceName)
	}
	if service.Deploy.Promotion != config.DeployPromotionManual {
		return fmt.Errorf("service %s does not use deploy.promotion=manual", serviceName)
	}

	serverNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(serverNames) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}

	stateLock := localstate.NewStateLock(".tako")
	lockInfo, err := stateLock.Acquire("promote")
	if err != nil {
		return fmt.Errorf("cannot promote: %w", err)
	}
	defer stateLock.Release(lockInfo)

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, serverNames, "promote")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Printf("→ Acquired remote promote leases: %s\n", leaseSet.Summary())
	}

	sourceServerName := serverNames[0]
	sourceServer, err := serverConfigByName(cfg, sourceServerName)
	if err != nil {
		return err
	}
	sourceClient, err := sshPool.GetOrCreateWithAuth(sourceServer.Host, sourceServer.Port, sourceServer.User, sourceServer.SSHKey, sourceServer.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to node %s: %w", sourceServerName, err)
	}

	deploy := deployer.NewDeployerWithPool(sourceClient, cfg, envName, sshPool, verbose)
	deploy.SetCLIVersion(Version)
	if err := deploy.SetTargetServers(serverNames); err != nil {
		return err
	}
	if err := deploy.SetupTakodRuntime(); err != nil {
		return fmt.Errorf("failed to setup takod runtime: %w", err)
	}

	actualState, err := reconcile.GatherActualStateFromServers(sshPool, cfg, envName, serverNames, nil)
	if err != nil {
		return deployActualStateError(err)
	}
	actual := actualState[serviceName]
	targetRevision, err := selectPromotionRevision(actual, promoteRevision)
	if err != nil {
		return fmt.Errorf("cannot promote %s: %w", serviceName, err)
	}
	targetImage, err := promotionTargetImage(cfg, envName, serviceName, service, actual, targetRevision)
	if err != nil {
		return fmt.Errorf("cannot promote %s: %w", serviceName, err)
	}

	activeRevisions := deployplan.ProxyActiveRevisions(cfg, envName, services, nil, nil, actualState)
	if activeRevisions == nil {
		activeRevisions = make(map[string]string)
	}
	activeRevisions[serviceName] = targetRevision

	fmt.Printf("\n=== Promoting %s ===\n\n", serviceName)
	fmt.Printf("Target revision: %s\n", targetRevision)

	startTime := time.Now()
	if err := deploy.ActivateTakodServiceRevision(serviceName, &service, targetImage); err != nil {
		return fmt.Errorf("failed to activate warmed revision before proxy promotion: %w", err)
	}
	if err := reconcileDeployProxy(deploy, services, activeRevisions); err != nil {
		return fmt.Errorf("failed to promote proxy route: %w", err)
	}
	if err := pruneTakodServiceRevisionsAfterGrace(deploy, map[string]config.ServiceConfig{serviceName: service}, map[string]string{serviceName: targetRevision}); err != nil {
		return fmt.Errorf("proxy promoted but failed to prune stale revisions: %w", err)
	}

	postNodeActualState, err := reconcile.GatherActualStateByServer(sshPool, cfg, envName, serverNames)
	if err != nil {
		return fmt.Errorf("promotion succeeded but failed to gather post-promotion actual state: %w", err)
	}
	postActualState := reconcile.AggregateActualStateByServer(postNodeActualState)
	runtimeImageRefs := deployplan.MergeRuntimeImageRefs(cfg, envName, services, nil, postActualState)
	if err := persistTakodRuntimeState(
		sshPool,
		cfg,
		envName,
		serverNames,
		"promote",
		services,
		runtimeImageRefs,
		postActualState,
		postNodeActualState,
		optionalPromoteGitInfo(),
		"promote.succeeded",
		fmt.Sprintf("promoted %s to revision %s", serviceName, targetRevision),
		map[string]string{
			"service":  serviceName,
			"revision": targetRevision,
		},
	); err != nil {
		return fmt.Errorf("promotion succeeded but failed to persist takod state: %w", err)
	}

	stateManager := remotestate.NewStateManagerWithSocket(sourceClient, cfg.Project.Name, envName, sourceServer.Host, takodSocketFromConfig(cfg))
	promoteDeployment := buildPromoteDeployment(cfg, envName, sourceServer.Host, serviceName, service, postActualState[serviceName], startTime, time.Since(startTime))
	if err := stateManager.SaveDeployment(promoteDeployment); err != nil {
		return fmt.Errorf("promotion succeeded but failed to save deployment history: %w", err)
	}
	if cfg.IsMultiServer() {
		replicator := remotestate.NewStateReplicator(sshPool, cfg, envName, cfg.Project.Name, verbose)
		history, err := stateManager.LoadHistory()
		if err != nil {
			return fmt.Errorf("promotion succeeded but failed to load remote deployment history for replication: %w", err)
		}
		if err := replicator.ReplicateDeployment(promoteDeployment, history); err != nil {
			return fmt.Errorf("promotion succeeded but failed to replicate remote deployment history: %w", err)
		}
	}
	saveLocalPromoteState(cfg, envName, serverNames, serviceName, targetRevision, promoteDeployment, verbose)

	fmt.Printf("\n✓ Promoted %s to revision %s\n", serviceName, shortRevision(targetRevision))
	return nil
}

func promotionTargetImage(cfg *config.Config, envName string, serviceName string, service config.ServiceConfig, actual *reconcile.ActualService, targetRevision string) (string, error) {
	if actual == nil {
		return "", fmt.Errorf("service is not deployed")
	}
	image := strings.TrimSpace(actual.RevisionImages[targetRevision])
	if image == "" {
		image = strings.TrimSpace(actual.Image)
	}
	if image == "" {
		return "", fmt.Errorf("warmed revision does not report an image")
	}
	expected := deployer.ServiceRevisionID(cfg.Project.Name, envName, serviceName, image, service)
	if expected != targetRevision {
		return "", fmt.Errorf("warmed revision %s image %s resolves to revision %s", targetRevision, image, expected)
	}
	return image, nil
}

func selectPromotionRevision(actual *reconcile.ActualService, requested string) (string, error) {
	if actual == nil {
		return "", fmt.Errorf("service is not deployed")
	}
	revisions := promotionCandidateRevisions(actual)
	requested = strings.TrimSpace(requested)
	if requested != "" {
		return matchRequestedPromotionRevision(revisions, requested)
	}
	if len(revisions) == 0 {
		return "", fmt.Errorf("no warmed revision found")
	}
	if len(revisions) > 1 {
		return "", fmt.Errorf("multiple warmed revisions found (%s); rerun with --revision", strings.Join(revisions, ", "))
	}
	return revisions[0], nil
}

func promotionCandidateRevisions(actual *reconcile.ActualService) []string {
	if actual == nil {
		return nil
	}
	seen := make(map[string]bool)
	var revisions []string
	add := func(revision string) {
		revision = strings.TrimSpace(revision)
		if revision == "" || revision == actual.CurrentRevision || seen[revision] {
			return
		}
		seen[revision] = true
		revisions = append(revisions, revision)
	}
	for _, revision := range actual.WarmingRevisions {
		add(revision)
	}
	add(actual.PreviousRevision)
	sort.Strings(revisions)
	return revisions
}

func matchRequestedPromotionRevision(revisions []string, requested string) (string, error) {
	var matches []string
	for _, revision := range revisions {
		if revision == requested || strings.HasPrefix(revision, requested) {
			matches = append(matches, revision)
		}
	}
	switch len(matches) {
	case 0:
		if len(revisions) == 0 {
			return "", fmt.Errorf("no warmed revision found")
		}
		return "", fmt.Errorf("revision %q is not warmed; available warmed revisions: %s", requested, strings.Join(revisions, ", "))
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("revision prefix %q is ambiguous: %s", requested, strings.Join(matches, ", "))
	}
}

func buildPromoteDeployment(
	cfg *config.Config,
	envName string,
	host string,
	serviceName string,
	service config.ServiceConfig,
	actual *reconcile.ActualService,
	start time.Time,
	duration time.Duration,
) *remotestate.DeploymentState {
	serviceState := remotestate.ServiceState{
		Name:     serviceName,
		Port:     service.Port,
		Replicas: service.Replicas,
		Env:      redactedEnvKeys(service.Env),
	}
	if actual != nil {
		serviceState.Image = actual.Image
		serviceState.Replicas = actual.Replicas
		if len(actual.Containers) > 0 {
			serviceState.ContainerID = actual.Containers[0]
		}
	}
	return &remotestate.DeploymentState{
		Timestamp:   start,
		ProjectName: cfg.Project.Name,
		Environment: envName,
		Version:     cfg.Project.Version,
		Status:      remotestate.StatusSuccess,
		Services:    map[string]remotestate.ServiceState{serviceName: serviceState},
		User:        remotestate.GetCurrentUser(),
		Host:        host,
		Duration:    duration,
		Message:     fmt.Sprintf("promoted %s", serviceName),
		CLIVersion:  Version,
		CLICommit:   GitCommit,
	}
}

func optionalPromoteGitInfo() takodstate.GitInfo {
	client := git.NewClient(".")
	if !client.IsRepository() {
		return takodstate.GitInfo{}
	}
	commit, err := client.GetCommitInfo("")
	if err != nil {
		return takodstate.GitInfo{}
	}
	return gitInfoFromCommit(commit)
}

func saveLocalPromoteState(cfg *config.Config, envName string, serverNames []string, serviceName string, revision string, deployment *remotestate.DeploymentState, verbose bool) {
	localStateMgr, err := localstate.NewManager(".", cfg.Project.Name, envName)
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to initialize local state for promotion: %v\n", err)
		}
		return
	}
	localDeployment := &localstate.DeploymentState{
		DeploymentID:    fmt.Sprintf("promote-%s", time.Now().Format("20060102-150405")),
		Timestamp:       deployment.Timestamp,
		Environment:     envName,
		Mode:            cfg.GetRuntimeMode(),
		Servers:         append([]string(nil), serverNames...),
		Status:          string(remotestate.StatusSuccess),
		DurationSeconds: int(deployment.Duration.Seconds()),
		TriggeredBy:     remotestate.GetCurrentUser(),
		Notes:           fmt.Sprintf("Promoted %s to revision %s", serviceName, revision),
	}
	if err := localStateMgr.SaveDeployment(localDeployment); err != nil && verbose {
		fmt.Printf("Warning: failed to save local promote state: %v\n", err)
	}
}
