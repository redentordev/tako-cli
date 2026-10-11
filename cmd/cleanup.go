package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	cleanupServer      string
	cleanupKeep        int
	cleanupFull        bool
	cleanupSecure      bool
	cleanupDockerCache bool
	cleanupCacheKeep   string
	cleanupDryRun      bool
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Clean up app/stage runtime resources and logs",
	Long: `Clean up app/stage runtime resources to reclaim disk space.

This command helps maintain your servers by:
  - Removing stopped service replicas
  - Removing old service images (keeps latest N per service and environment)
  - Removing unused Tako project volumes
  - Securing log file permissions

Regular cleanup prevents disk space exhaustion and keeps your
deployment servers lean and efficient.

Shared-node safety:
  - Default cleanup is scoped to the current project and environment
  - It does not remove unrelated project containers, volumes, proxy routes, or images
  - Old images are removed only when the node's deployment history attributes
    them to this environment alone; images referenced by another environment,
    by any current deployment, or by any container are kept. If that state
    cannot be read, no images are removed
  - Docker builder cache and dangling image cleanup are global Docker operations;
    use --docker-cache only when you intentionally want to reclaim shared cache
  - --keep counts images per image repository in the target environment only;
    other environments' images are never counted or removed
  - Old-image removal needs a node agent with disk hygiene; older nodes skip it
    with a warning (run 'tako upgrade servers') and run the other steps

Dry run:
  --dry-run asks each node which old images it would remove and why the rest
  are kept, and changes nothing. Nodes without disk hygiene report that a dry
  run is not supported.

Security:
  - Uses --secure flag to restrict log file permissions
  - Logs are readable only by appropriate system users and root
  - Prevents unauthorized access to request logs

Examples:
  tako cleanup                  # Clean all servers, keep 3 latest images
  tako cleanup --keep 5         # Keep 5 latest images
  tako cleanup --dry-run        # Show which old images would be removed
  tako cleanup --server prod    # Clean specific server
  tako cleanup --full           # More aggressive app/stage cleanup
  tako cleanup --docker-cache   # Also prune shared Docker build cache/dangling images
  tako cleanup --docker-cache --docker-cache-keep-storage 10GB
  tako cleanup --secure         # Also secure log permissions`,
	RunE: runCleanup,
}

func init() {
	rootCmd.AddCommand(cleanupCmd)
	cleanupCmd.Flags().StringVarP(&cleanupServer, "server", "s", "", "Specific server to clean (default: all servers)")
	cleanupCmd.Flags().IntVarP(&cleanupKeep, "keep", "k", 3, "Number of latest images to keep per image repository, counted in the target environment only")
	cleanupCmd.Flags().BoolVar(&cleanupDryRun, "dry-run", false, "Report which old images would be removed and why the rest are kept; change nothing")
	cleanupCmd.Flags().BoolVarP(&cleanupFull, "full", "f", false, "Perform more aggressive app/stage cleanup")
	cleanupCmd.Flags().BoolVarP(&cleanupSecure, "secure", "", false, "Also secure log file permissions")
	cleanupCmd.Flags().BoolVar(&cleanupDockerCache, "docker-cache", false, "Also clean Docker builder cache and dangling images shared by all projects")
	cleanupCmd.Flags().StringVar(&cleanupCacheKeep, "docker-cache-keep-storage", takod.DefaultBuildCacheKeepStorage, "Docker builder cache storage budget to keep when --docker-cache is used")
}

func runCleanup(cmd *cobra.Command, args []string) error {
	// Machine modes reserve stdout for parseable output.
	var out io.Writer = os.Stdout
	if machineOutputEnabled() {
		out = os.Stderr
	}

	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	// Determine which servers to clean
	envName := getEnvironmentName(cfg)
	serversToClean, err := resolveEnvironmentServerSet(cfg, envName, cleanupServer)
	if err != nil {
		return err
	}
	serversToClean, targetServerNames, err := schedulableMutationServerSet(cfg, envName, serversToClean, true)
	if err != nil {
		return err
	}
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services for environment %s: %w", envName, err)
	}
	imageRepositories := cleanupImageRepositories(cfg, envName, services)
	externalVolumes := externalVolumeNamesForEnvironment(cfg, envName)
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	runtimeFactory, err := nodeclient.NewFactory(cfg, sshPool, takodSocketFromConfig(cfg))
	if err != nil {
		return err
	}
	defer runtimeFactory.CloseIdleConnections()
	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, targetServerNames, "cleanup")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Fprintf(out, "→ Acquired remote cleanup leases: %s\n", leaseSet.Summary())
	}

	// If full cleanup, keep fewer images
	keepImages := cleanupKeep
	if cleanupFull {
		keepImages = 2
	}

	if cleanupDryRun {
		fmt.Fprintf(out, "🔎 Dry run on %d server(s): nothing will be removed\n", len(serversToClean))
	} else {
		fmt.Fprintf(out, "🧹 Cleaning up %d server(s)...\n", len(serversToClean))
	}
	fmt.Fprintf(out, "   Keeping %d latest images per image repository in this environment\n\n", keepImages)
	fmt.Fprintf(out, "Scope: project %s, environment %s\n", cfg.Project.Name, envName)
	if cleanupDryRun {
		fmt.Fprintln(out, "Only old-image removal is evaluated in a dry run.")
	} else if cleanupDockerCache {
		fmt.Fprintf(out, "⚠️  Shared Docker cache cleanup enabled: build cache will keep about %s and dangling images may be used by unrelated projects.\n", cleanupCacheKeep)
	} else {
		fmt.Fprintln(out, "Shared Docker cache untouched. Use --docker-cache only when reclaiming node-wide Docker cache intentionally.")
	}
	fmt.Fprintln(out)

	var noticesMu sync.Mutex
	notices := map[string][]string{}
	results := collectCleanupNodes(serversToClean, func(serverName string, serverCfg config.ServerConfig) (*takod.CleanupResponse, error) {
		response, nodeNotices, err := cleanupSingleRuntimeNode(cmd.Context(), cfg, runtimeFactory, serverName, serverCfg, cleanupRequestForEnvironment(cfg, envName, imageRepositories, externalVolumes, keepImages, cleanupDockerCache, cleanupCacheKeep, cleanupSecure), cleanupDryRun)
		noticesMu.Lock()
		notices[serverName] = nodeNotices
		noticesMu.Unlock()
		return response, err
	})
	for i := range results {
		results[i].notices = notices[results[i].serverName]
	}

	totalErrors := 0
	for _, result := range results {
		fmt.Fprintf(out, "=== Cleaning server: %s (%s) ===\n", result.serverName, result.host)
		if result.err != nil {
			fmt.Fprintf(out, "❌ Cleanup failed: %v\n\n", result.err)
			totalErrors++
			continue
		}

		// An old node skipping old-image removal is a warning: the run is
		// partial under the ActionResult rules (exit 6).
		for _, notice := range result.notices {
			fmt.Fprintf(out, "  ⚠️  %s\n", notice)
		}
		totalErrors += len(result.notices)
		if len(result.response.Warnings) > 0 {
			totalErrors += len(result.response.Warnings)
			printCleanupWarnings(out, result.response)
		}
		printCleanupHygiene(out, result.response.Hygiene)
		if cleanupDryRun {
			fmt.Fprintf(out, "✓ Dry run on %s complete; nothing was removed\n\n", result.serverName)
			continue
		}
		if verbose {
			if result.response.InitialDiskUsage != "" {
				fmt.Fprintf(out, "  Disk before: %s\n", result.response.InitialDiskUsage)
			}
			if result.response.FinalDiskUsage != "" {
				fmt.Fprintf(out, "  Disk after:  %s\n", result.response.FinalDiskUsage)
			}
			if result.response.ImagesRemoved > 0 || result.response.ContainersRemoved > 0 {
				fmt.Fprintf(out, "  Removed %d image(s), %d stopped container(s)\n", result.response.ImagesRemoved, result.response.ContainersRemoved)
			}
		}

		fmt.Fprintf(out, "✓ Server %s cleaned successfully\n\n", result.serverName)
	}

	ack := cleanupActionResult(cfg.Project.Name, envName, results, totalErrors)

	// Summary
	if cleanupDryRun && totalErrors == 0 {
		fmt.Fprintln(out, "✨ Dry run complete. Run without --dry-run to remove the listed images.")
		return emitResultDocument(ack)
	}
	if totalErrors > 0 {
		fmt.Fprintf(out, "⚠️  Cleanup completed with %d errors\n", totalErrors)
		fmt.Fprintln(out, "   Run with -v (verbose) flag for more details")
		err := &engine.AttentionError{Err: fmt.Errorf("cleanup completed with %d errors", totalErrors)}
		ack.Error = err.Error()
		if emitErr := emitResultDocument(ack); emitErr != nil {
			return emitErr
		}
		return err
	}

	fmt.Fprintln(out, "✨ All servers cleaned successfully!")
	fmt.Fprintln(out, "\n💡 Tip: Run 'tako cleanup' regularly to maintain optimal disk usage")
	fmt.Fprintln(out, "   Consider adding it to your deployment workflow or cron jobs")

	return emitResultDocument(ack)
}

// cleanupActionResult builds the machine-facing acknowledgement. Notices
// (an old node skipping old-image removal) are listed with the node's
// warnings; totalErrors, which counts both, decides a partial outcome.
func cleanupActionResult(project string, envName string, results []cleanupNodeResult, totalErrors int) engine.ActionResult {
	ack := engine.ActionResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindActionResult,
		Project:     project,
		Environment: envName,
		Action:      engine.ActionCleanup,
		Servers:     []engine.ActionNodeOutcome{},
	}
	failedNodes := 0
	for _, result := range results {
		outcome := engine.ActionNodeOutcome{Server: result.serverName, Host: result.host, Done: result.err == nil}
		if result.err != nil {
			outcome.Error = result.err.Error()
			failedNodes++
		} else {
			outcome.Warnings = append(outcome.Warnings, result.notices...)
			if result.response != nil {
				outcome.Warnings = append(outcome.Warnings, result.response.Warnings...)
				outcome.Hygiene = result.response.Hygiene
			}
		}
		ack.Servers = append(ack.Servers, outcome)
	}
	switch {
	case failedNodes == len(results) && failedNodes > 0:
		ack.Outcome = engine.ActionOutcomeFailed
	case totalErrors > 0:
		ack.Outcome = engine.ActionOutcomePartial
	default:
		ack.Outcome = engine.ActionOutcomeOK
	}
	return ack
}

// printCleanupHygiene prints a node's old-image report: each removed (or,
// in a dry run, removable) image and the kept counts by reason.
func printCleanupHygiene(out io.Writer, report *takod.CleanupHygiene) {
	if report == nil {
		return
	}
	verb := "Removed"
	if report.DryRun {
		verb = "Would remove"
	}
	if len(report.Removed) == 0 {
		fmt.Fprintf(out, "  %s no old images\n", verb)
	} else {
		fmt.Fprintf(out, "  %s %d old image(s):\n", verb, len(report.Removed))
		for _, image := range report.Removed {
			name := image.ID
			if len(image.Refs) > 0 {
				name = strings.Join(image.Refs, ", ")
			}
			if image.SizeBytes > 0 {
				fmt.Fprintf(out, "    - %s (%s)\n", name, takodclient.FormatBytes(image.SizeBytes))
			} else {
				fmt.Fprintf(out, "    - %s\n", name)
			}
		}
	}
	if !report.DryRun && report.BytesFreed > 0 {
		fmt.Fprintf(out, "  Freed %s\n", takodclient.FormatBytes(report.BytesFreed))
	}
	for _, failed := range report.Failed {
		fmt.Fprintf(out, "  Kept (Docker refused removal): %s\n", failed)
	}
	if len(report.Kept) > 0 {
		reasons := make([]string, 0, len(report.Kept))
		for reason := range report.Kept {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		parts := make([]string, 0, len(reasons))
		for _, reason := range reasons {
			parts = append(parts, fmt.Sprintf("%d %s", report.Kept[reason], reason))
		}
		fmt.Fprintf(out, "  Kept: %s\n", strings.Join(parts, ", "))
	}
}

func cleanupRequestForEnvironment(cfg *config.Config, envName string, imageRepositories []string, externalVolumes []string, keepImages int, includeDockerCache bool, buildCacheKeepStorage string, secureLogs bool) takod.CleanupRequest {
	request := takod.CleanupRequest{
		Project:                cfg.Project.Name,
		Environment:            envName,
		ImageRepositories:      imageRepositories,
		ExternalVolumes:        externalVolumes,
		KeepImages:             keepImages,
		CleanOldImages:         true,
		CleanStoppedContainers: true,
		CleanDanglingImages:    includeDockerCache,
		CleanBuildCache:        includeDockerCache,
		CleanUnusedVolumes:     true,
		SecureLogPermissions:   secureLogs,
	}
	if includeDockerCache {
		request.BuildCacheKeepStorage = buildCacheKeepStorage
	}
	return request
}

type cleanupNodeAction func(serverName string, serverCfg config.ServerConfig) (*takod.CleanupResponse, error)

type sshClientProvider interface {
	GetOrCreateWithAuth(host string, port int, user string, keyPath string, password string) (*ssh.Client, error)
}

type cleanupNodeResult struct {
	index      int
	serverName string
	host       string
	response   *takod.CleanupResponse
	err        error
	// notices are client-side per-node warnings (old-image removal skipped
	// on a node without disk hygiene).
	notices []string
}

func collectCleanupNodes(servers map[string]config.ServerConfig, action cleanupNodeAction) []cleanupNodeResult {
	names := sortedCleanupServerNames(servers)

	resultCh := make(chan cleanupNodeResult, len(names))
	var wg sync.WaitGroup
	for index, serverName := range names {
		serverCfg := servers[serverName]
		wg.Add(1)
		go func(index int, serverName string, serverCfg config.ServerConfig) {
			defer wg.Done()
			response, err := action(serverName, serverCfg)
			resultCh <- cleanupNodeResult{
				index:      index,
				serverName: serverName,
				host:       serverCfg.Host,
				response:   response,
				err:        err,
			}
		}(index, serverName, serverCfg)
	}
	wg.Wait()
	close(resultCh)

	results := make([]cleanupNodeResult, len(names))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

func sortedCleanupServerNames(servers map[string]config.ServerConfig) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cleanupSingleNode(cfg *config.Config, pool sshClientProvider, serverCfg config.ServerConfig, request takod.CleanupRequest) (*takod.CleanupResponse, error) {
	return cleanupSingleNodeWithExecutor(cfg, pool, serverCfg, request, cleanupViaTakod)
}

func cleanupSingleRuntimeNode(ctx context.Context, cfg *config.Config, factory *nodeclient.Factory, serverName string, serverCfg config.ServerConfig, request takod.CleanupRequest, dryRun bool) (*takod.CleanupResponse, []string, error) {
	client, _, err := factory.Client(ctx, serverName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to %s: %w", serverCfg.Host, err)
	}
	return engine.CleanupNode(ctx, client, cfg, serverName, request, dryRun)
}

func cleanupSingleNodeWithExecutor(cfg *config.Config, pool sshClientProvider, serverCfg config.ServerConfig, request takod.CleanupRequest, execute func(*ssh.Client, *config.Config, takod.CleanupRequest) (*takod.CleanupResponse, error)) (*takod.CleanupResponse, error) {
	if pool == nil {
		return nil, fmt.Errorf("ssh pool is not initialized")
	}
	client, err := pool.GetOrCreateWithAuth(serverCfg.Host, serverCfg.Port, serverCfg.User, serverCfg.SSHKey, serverCfg.Password)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}

	response, err := execute(client, cfg, request)
	if err != nil {
		return nil, err
	}
	return response, nil
}
