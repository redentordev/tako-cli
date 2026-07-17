package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var (
	cleanupServer      string
	cleanupKeep        int
	cleanupFull        bool
	cleanupSecure      bool
	cleanupDockerCache bool
	cleanupCacheKeep   string
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Clean up app/stage runtime resources and logs",
	Long: `Clean up app/stage runtime resources to reclaim disk space.

This command helps maintain your servers by:
  - Removing old service images (keeps latest N)
  - Removing stopped service replicas
  - Removing unused Tako project volumes
  - Securing log file permissions

Regular cleanup prevents disk space exhaustion and keeps your
deployment servers lean and efficient.

Shared-node safety:
  - Default cleanup is scoped to the current project and environment
  - It does not remove unrelated project containers, volumes, proxy routes, or images
  - Docker builder cache and dangling image cleanup are global Docker operations;
    use --docker-cache only when you intentionally want to reclaim shared cache

Security:
  - Uses --secure flag to restrict log file permissions
  - Logs are readable only by appropriate system users and root
  - Prevents unauthorized access to request logs

Examples:
  tako cleanup                  # Clean all servers, keep 3 latest images
  tako cleanup --keep 5         # Keep 5 latest images
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
	cleanupCmd.Flags().IntVarP(&cleanupKeep, "keep", "k", 3, "Number of latest images to keep per service")
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
		return fmt.Errorf("failed to load config: %w", err)
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
	targetServerNames := sortedCleanupServerNames(serversToClean)
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

	fmt.Fprintf(out, "🧹 Cleaning up %d server(s)...\n", len(serversToClean))
	fmt.Fprintf(out, "   Keeping %d latest images per service\n\n", keepImages)
	fmt.Fprintf(out, "Scope: project %s, environment %s\n", cfg.Project.Name, envName)
	if cleanupDockerCache {
		fmt.Fprintf(out, "⚠️  Shared Docker cache cleanup enabled: build cache will keep about %s and dangling images may be used by unrelated projects.\n", cleanupCacheKeep)
	} else {
		fmt.Fprintln(out, "Shared Docker cache untouched. Use --docker-cache only when reclaiming node-wide Docker cache intentionally.")
	}
	fmt.Fprintln(out)

	results := collectCleanupNodes(serversToClean, func(serverName string, serverCfg config.ServerConfig) (*takod.CleanupResponse, error) {
		return cleanupSingleRuntimeNode(cmd.Context(), cfg, runtimeFactory, serverName, serverCfg, cleanupRequestForEnvironment(cfg, envName, imageRepositories, externalVolumes, keepImages, cleanupDockerCache, cleanupCacheKeep, cleanupSecure))
	})

	totalErrors := 0
	for _, result := range results {
		fmt.Fprintf(out, "=== Cleaning server: %s (%s) ===\n", result.serverName, result.host)
		if result.err != nil {
			fmt.Fprintf(out, "❌ Cleanup failed: %v\n\n", result.err)
			totalErrors++
			continue
		}

		if len(result.response.Warnings) > 0 {
			totalErrors += len(result.response.Warnings)
			printCleanupWarnings(out, result.response)
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

	ack := engine.ActionResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindActionResult,
		Project:     cfg.Project.Name,
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
		} else if result.response != nil {
			outcome.Warnings = append(outcome.Warnings, result.response.Warnings...)
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

	// Summary
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

func cleanupSingleRuntimeNode(ctx context.Context, cfg *config.Config, factory *nodeclient.Factory, serverName string, serverCfg config.ServerConfig, request takod.CleanupRequest) (*takod.CleanupResponse, error) {
	client, _, err := factory.Client(ctx, serverName)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", serverCfg.Host, err)
	}
	return engine.CleanupViaTakodContext(ctx, client, cfg, request)
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
