package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var (
	takodSocket                  string
	takodDataDir                 string
	takodNode                    string
	takodIdentityFile            string
	takodMembershipFile          string
	takodActualRefreshInterval   time.Duration
	takodBuildCachePruneInterval time.Duration = takod.DefaultBuildCachePruneInterval
	takodBuildCacheKeepStorage   string        = takod.DefaultBuildCacheKeepStorage
	takodMinimumFreeDiskBytes    int64         = platform.DefaultResourcePolicy().MinimumFreeDiskBytes
	takodReservedMemoryBytes     int64         = platform.DefaultResourcePolicy().ReservedMemoryBytes
	takodMaximumConcurrentBuilds int
	takodDockerDataRoot          string = "/var/lib/docker"

	takodHygieneInterval               time.Duration
	takodHygieneCheckInterval          time.Duration = takod.DefaultHygieneCheckInterval
	takodImageRetention                int           = takod.DefaultImageRetention
	takodBuildCachePressureKeepStorage string        = takod.DefaultBuildCachePressureKeepStorage
	takodDefaultContainerLogging       string        = "on"
	takodDefaultContainerLogMaxSize    string        = takod.DefaultContainerLogMaxSize
	takodDefaultContainerLogMaxFile    int           = takod.DefaultContainerLogMaxFile
)

var takodCmd = &cobra.Command{
	Use:   "takod",
	Short: "Run the takod node agent",
	Long: `Run the node-local takod agent.

The agent listens on a Unix socket and exposes node-local runtime status for
CLI status, actual-state discovery, service container reconcile, and proxy
runtime operations.`,
}

var takodRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the takod Unix-socket server",
	RunE:  runTakod,
}

func init() {
	rootCmd.AddCommand(takodCmd)
	takodCmd.AddCommand(takodRunCmd)

	takodRunCmd.Flags().StringVar(&takodSocket, "socket", "", "Unix socket path")
	takodRunCmd.Flags().StringVar(&takodDataDir, "data-dir", "", "takod data directory")
	takodRunCmd.Flags().StringVar(&takodNode, "node", "", "Configured Tako node name")
	takodRunCmd.Flags().StringVar(&takodIdentityFile, "identity-file", nodeidentity.DefaultPath, "Root-owned Tako installation identity file")
	takodRunCmd.Flags().StringVar(&takodMembershipFile, "membership-file", "", "Protected controller membership state (derived from data-dir when omitted)")
	takodRunCmd.Flags().DurationVar(&takodActualRefreshInterval, "actual-refresh-interval", 0, "Refresh node-local actual state at this interval (0 disables)")
	takodRunCmd.Flags().DurationVar(&takodBuildCachePruneInterval, "build-cache-prune-interval", takod.DefaultBuildCachePruneInterval, "Prune Docker build cache at this interval (0 disables)")
	takodRunCmd.Flags().StringVar(&takodBuildCacheKeepStorage, "build-cache-keep-storage", takod.DefaultBuildCacheKeepStorage, "Docker builder cache storage budget to keep during scheduled pruning")
	takodRunCmd.Flags().Int64Var(&takodMinimumFreeDiskBytes, "minimum-free-disk-bytes", platform.DefaultResourcePolicy().MinimumFreeDiskBytes, "Reject disk-growing operations below this free-disk floor (0 disables)")
	takodRunCmd.Flags().Int64Var(&takodReservedMemoryBytes, "reserved-memory-bytes", platform.DefaultResourcePolicy().ReservedMemoryBytes, "Host memory kept unavailable to isolated verification work")
	takodRunCmd.Flags().IntVar(&takodMaximumConcurrentBuilds, "max-concurrent-builds", 0, "Maximum concurrent image builds (0 keeps legacy unlimited behavior)")
	takodRunCmd.Flags().StringVar(&takodDockerDataRoot, "docker-data-root", "/var/lib/docker", "Docker data-root filesystem used for image and volume admission")
	takodRunCmd.Flags().DurationVar(&takodHygieneInterval, "hygiene-interval", 0, "Run disk hygiene maintenance (image GC, then build-cache prune) at this interval; defaults to --build-cache-prune-interval (0 disables autonomous hygiene)")
	takodRunCmd.Flags().DurationVar(&takodHygieneCheckInterval, "hygiene-check-interval", takod.DefaultHygieneCheckInterval, "Check free disk for soft or hard pressure at this interval")
	takodRunCmd.Flags().IntVar(&takodImageRetention, "image-retention", takod.DefaultImageRetention, "Built images kept per repository and environment by the scheduled image GC (minimum 1)")
	takodRunCmd.Flags().StringVar(&takodBuildCachePressureKeepStorage, "build-cache-pressure-keep-storage", takod.DefaultBuildCachePressureKeepStorage, "Docker builder cache storage budget kept by a hard disk-pressure reclaim")
	takodRunCmd.Flags().StringVar(&takodDefaultContainerLogging, "default-container-logging", "on", "Bound logs of services without a logging block with json-file rotation (on or off)")
	takodRunCmd.Flags().StringVar(&takodDefaultContainerLogMaxSize, "default-container-log-max-size", takod.DefaultContainerLogMaxSize, "Default container log max-size (e.g. 20m)")
	takodRunCmd.Flags().IntVar(&takodDefaultContainerLogMaxFile, "default-container-log-max-file", takod.DefaultContainerLogMaxFile, "Default container log max-file")
}

func runTakod(cmd *cobra.Command, args []string) error {
	socket := takodSocket
	dataDir := takodDataDir

	if socket == "" || dataDir == "" {
		cfg, err := config.LoadConfig(cfgFile)
		if err == nil {
			if socket == "" && cfg.Runtime != nil && cfg.Runtime.Agent != nil {
				socket = cfg.Runtime.Agent.Socket
			}
			if dataDir == "" && cfg.Runtime != nil && cfg.Runtime.Agent != nil {
				dataDir = cfg.Runtime.Agent.DataDir
			}
		}
	}
	if socket == "" {
		socket = "/run/tako/takod.sock"
	}
	if dataDir == "" {
		dataDir = "/var/lib/tako"
	}

	options, err := takodHygieneOptions(cmd)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	metricsOptions, err := takodMetricsOptions()
	if err != nil {
		return err
	}
	if verbose {
		fmt.Printf("takod listening on %s with data dir %s\n", socket, dataDir)
	}
	options.NodeName = takodNode
	options.IdentityFile = takodIdentityFile
	options.MembershipFile = takodMembershipFile
	options.ActualRefreshInterval = takodActualRefreshInterval
	options.BuildCachePruneInterval = takodBuildCachePruneInterval
	options.BuildCacheKeepStorage = takodBuildCacheKeepStorage
	options.MinimumFreeDiskBytes = takodMinimumFreeDiskBytes
	options.ReservedMemoryBytes = takodReservedMemoryBytes
	options.MaximumConcurrentBuilds = takodMaximumConcurrentBuilds
	options.DockerDataRoot = takodDockerDataRoot
	options.Metrics = metricsOptions
	err = takod.NewServerWithOptions(socket, dataDir, Version, options).Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// takodHygieneOptions validates the disk hygiene flags. Without
// --hygiene-interval the maintenance tick keeps the legacy
// --build-cache-prune-interval, which is what setup-written units set.
func takodHygieneOptions(cmd *cobra.Command) (takod.ServerOptions, error) {
	var options takod.ServerOptions
	if cmd != nil && cmd.Flags().Changed("hygiene-interval") {
		if takodHygieneInterval < 0 {
			return options, fmt.Errorf("--hygiene-interval cannot be negative")
		}
		interval := takodHygieneInterval
		options.HygieneInterval = &interval
	}
	if takodHygieneCheckInterval < time.Minute {
		return options, fmt.Errorf("--hygiene-check-interval must be at least 1m")
	}
	if takodImageRetention < 1 {
		return options, fmt.Errorf("--image-retention must be at least 1")
	}
	if !takod.ValidBuildCacheKeepStorage(takodBuildCachePressureKeepStorage) {
		return options, fmt.Errorf("invalid --build-cache-pressure-keep-storage %q", takodBuildCachePressureKeepStorage)
	}
	if err := takod.ValidateDefaultContainerLogging(takodDefaultContainerLogging, takodDefaultContainerLogMaxSize, takodDefaultContainerLogMaxFile); err != nil {
		return options, err
	}
	options.HygieneCheckInterval = takodHygieneCheckInterval
	options.ImageRetention = takodImageRetention
	options.BuildCachePressureKeepStorage = takodBuildCachePressureKeepStorage
	options.DefaultContainerLoggingOff = takodDefaultContainerLogging == "off"
	options.DefaultContainerLogMaxSize = takodDefaultContainerLogMaxSize
	options.DefaultContainerLogMaxFile = takodDefaultContainerLogMaxFile
	return options, nil
}
