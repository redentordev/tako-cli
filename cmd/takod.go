package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var (
	takodSocket                  string
	takodDataDir                 string
	takodNode                    string
	takodActualRefreshInterval   time.Duration
	takodBuildCachePruneInterval time.Duration = takod.DefaultBuildCachePruneInterval
	takodBuildCacheKeepStorage   string        = takod.DefaultBuildCacheKeepStorage
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
	takodRunCmd.Flags().DurationVar(&takodActualRefreshInterval, "actual-refresh-interval", 0, "Refresh node-local actual state at this interval (0 disables)")
	takodRunCmd.Flags().DurationVar(&takodBuildCachePruneInterval, "build-cache-prune-interval", takod.DefaultBuildCachePruneInterval, "Prune Docker build cache at this interval (0 disables)")
	takodRunCmd.Flags().StringVar(&takodBuildCacheKeepStorage, "build-cache-keep-storage", takod.DefaultBuildCacheKeepStorage, "Docker builder cache storage budget to keep during scheduled pruning")
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if verbose {
		fmt.Printf("takod listening on %s with data dir %s\n", socket, dataDir)
	}
	err := takod.NewServerWithOptions(socket, dataDir, Version, takod.ServerOptions{
		NodeName:                takodNode,
		ActualRefreshInterval:   takodActualRefreshInterval,
		BuildCachePruneInterval: takodBuildCachePruneInterval,
		BuildCacheKeepStorage:   takodBuildCacheKeepStorage,
	}).Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
