package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var (
	cleanupServer string
	cleanupKeep   int
	cleanupFull   bool
	cleanupSecure bool
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Clean up node runtime resources and logs",
	Long: `Clean up server resources to reclaim disk space.

This command helps maintain your servers by:
  - Removing old service images (keeps latest N)
  - Removing stopped service replicas
  - Cleaning dangling images
  - Pruning build cache
  - Removing unused volumes
  - Securing log file permissions

Regular cleanup prevents disk space exhaustion and keeps your
deployment servers lean and efficient.

Security:
  - Uses --secure flag to restrict log file permissions
  - Logs are readable only by appropriate system users and root
  - Prevents unauthorized access to request logs

Examples:
  tako cleanup                  # Clean all servers, keep 3 latest images
  tako cleanup --keep 5         # Keep 5 latest images
  tako cleanup --server prod    # Clean specific server
  tako cleanup --full           # Aggressive cleanup
  tako cleanup --secure         # Also secure log permissions`,
	RunE: runCleanup,
}

func init() {
	rootCmd.AddCommand(cleanupCmd)
	cleanupCmd.Flags().StringVarP(&cleanupServer, "server", "s", "", "Specific server to clean (default: all servers)")
	cleanupCmd.Flags().IntVarP(&cleanupKeep, "keep", "k", 3, "Number of latest images to keep per service")
	cleanupCmd.Flags().BoolVarP(&cleanupFull, "full", "f", false, "Perform full cleanup (more aggressive)")
	cleanupCmd.Flags().BoolVarP(&cleanupSecure, "secure", "", false, "Also secure log file permissions")
}

func runCleanup(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Determine which servers to clean
	serversToClean := make(map[string]config.ServerConfig)

	if cleanupServer != "" {
		// Clean specific server
		server, ok := cfg.Servers[cleanupServer]
		if !ok {
			return fmt.Errorf("server '%s' not found in config", cleanupServer)
		}
		serversToClean[cleanupServer] = server
	} else {
		// Clean all servers
		serversToClean = cfg.Servers
	}

	// If full cleanup, keep fewer images
	keepImages := cleanupKeep
	if cleanupFull {
		keepImages = 2
	}

	fmt.Printf("🧹 Cleaning up %d server(s)...\n", len(serversToClean))
	fmt.Printf("   Keeping %d latest images per service\n\n", keepImages)

	totalErrors := 0

	// Clean each server
	for serverName, serverCfg := range serversToClean {
		fmt.Printf("=== Cleaning server: %s (%s) ===\n", serverName, serverCfg.Host)

		// Connect to server (supports both key and password auth)
		client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
			Host:     serverCfg.Host,
			Port:     serverCfg.Port,
			User:     serverCfg.User,
			SSHKey:   serverCfg.SSHKey,
			Password: serverCfg.Password,
		})
		if err != nil {
			fmt.Printf("❌ Failed to connect to %s: %v\n\n", serverName, err)
			totalErrors++
			continue
		}
		if err := client.Connect(); err != nil {
			fmt.Printf("❌ Failed to connect to %s: %v\n\n", serverName, err)
			totalErrors++
			continue
		}

		response, err := cleanupViaTakod(client, cfg, takod.CleanupRequest{
			Project:                cfg.Project.Name,
			KeepImages:             keepImages,
			CleanOldImages:         true,
			CleanStoppedContainers: true,
			CleanDanglingImages:    true,
			CleanBuildCache:        true,
			CleanUnusedVolumes:     true,
			SecureLogPermissions:   cleanupSecure,
			PruneDocker:            cleanupFull,
		})
		if err != nil {
			fmt.Printf("❌ Cleanup failed: %v\n\n", err)
			totalErrors++
			client.Close()
			continue
		}

		if len(response.Warnings) > 0 {
			totalErrors += len(response.Warnings)
			printCleanupWarnings(response)
		}
		if verbose {
			if response.InitialDiskUsage != "" {
				fmt.Printf("  Disk before: %s\n", response.InitialDiskUsage)
			}
			if response.FinalDiskUsage != "" {
				fmt.Printf("  Disk after:  %s\n", response.FinalDiskUsage)
			}
			if response.ImagesRemoved > 0 || response.ContainersRemoved > 0 {
				fmt.Printf("  Removed %d image(s), %d stopped container(s)\n", response.ImagesRemoved, response.ContainersRemoved)
			}
		}

		fmt.Printf("✓ Server %s cleaned successfully\n\n", serverName)
		client.Close()
	}

	// Summary
	if totalErrors > 0 {
		fmt.Printf("⚠️  Cleanup completed with %d errors\n", totalErrors)
		fmt.Println("   Run with -v (verbose) flag for more details")
		return nil
	}

	fmt.Println("✨ All servers cleaned successfully!")
	fmt.Println("\n💡 Tip: Run 'tako cleanup' regularly to maintain optimal disk usage")
	fmt.Println("   Consider adding it to your deployment workflow or cron jobs")

	return nil
}
