package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var (
	destroyPurgeAll bool
	destroyForce    bool
)

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Remove deployed application and optionally clean up app-owned leftovers",
	Long: `Destroy the deployed application and cleanup resources.

This command has two modes:

1. DECOMMISSION MODE (default):
   - Stops and removes application service replicas
   - Removes application service images
   - Removes deployment files
   - Keeps tako-proxy, logs, and server setup
   - Safe for production - can redeploy later

2. PURGE MODE (--purge-all):
   - Everything from decommission mode, PLUS:
   - Prunes unused app-owned volumes
   - Prunes stopped app containers and old app images
   - Keeps shared takod and tako-proxy runtime intact
   - Safe when unrelated projects share the same node

Safety Features:
   - Production servers require explicit confirmation
   - Shows what will be removed before proceeding
   - Use --force to skip confirmation prompts

Examples:
   tako destroy                    # Decommission app, keep server setup
   tako destroy --purge-all        # Also prune app-owned leftovers
   tako destroy --force            # Skip confirmation prompts

PURGE MODE is app/stage scoped. It does not remove shared takod or tako-proxy.`,
	RunE: runDestroy,
}

func init() {
	rootCmd.AddCommand(destroyCmd)
	destroyCmd.Flags().BoolVar(&destroyPurgeAll, "purge-all", false, "Also prune app-owned leftovers after decommission")
	destroyCmd.Flags().BoolVar(&destroyForce, "force", false, "Skip confirmation prompts")
}

func runDestroy(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	// Acquire state lock to prevent concurrent operations
	stateLock := state.NewStateLock(".tako")
	lockInfo, err := stateLock.Acquire("destroy")
	if err != nil {
		return fmt.Errorf("cannot destroy: %w", err)
	}
	defer stateLock.Release(lockInfo)

	envName := getEnvironmentName(cfg)
	serversToDestroy, targetServerNames, err := destroyEnvironmentTargets(cfg, envName)
	if err != nil {
		return err
	}

	// Show warning and get confirmation
	mode := "DECOMMISSION"
	if destroyPurgeAll {
		mode = "PURGE"
	}

	fmt.Printf("⚠️  WARNING: You are about to %s the following servers:\n\n", mode)
	for _, serverName := range targetServerNames {
		server := serversToDestroy[serverName]
		fmt.Printf("   • %s (%s)\n", serverName, server.Host)
	}

	fmt.Printf("\n%s MODE will:\n", mode)
	fmt.Println("   ✓ Stop and remove all application service replicas")
	fmt.Println("   ✓ Remove application service images")
	fmt.Println("   ✓ Remove deployment files and directories")

	if destroyPurgeAll {
		fmt.Println("   ✓ Prune unused app-owned volumes")
		fmt.Println("   ✓ Prune stopped app containers and old app images")
		fmt.Println("\nPreserving shared server setup (takod, tako-proxy, logs)")
	} else {
		fmt.Println("\nPreserving server setup (takod, tako-proxy, logs)")
		fmt.Println("You can redeploy without running 'tako setup' again")
	}

	// Check if any production servers
	hasProduction := false
	for serverName := range serversToDestroy {
		if strings.Contains(strings.ToLower(serverName), "prod") {
			hasProduction = true
			break
		}
	}

	if hasProduction && !destroyForce {
		fmt.Println("\n🚨 PRODUCTION SERVER DETECTED!")
		fmt.Println("   This is a destructive operation.")
	}

	// Get confirmation
	if !destroyForce {
		fmt.Printf("\nType the project name '%s' to confirm: ", cfg.Project.Name)
		reader := bufio.NewReader(os.Stdin)
		confirmation, _ := reader.ReadString('\n')
		confirmation = strings.TrimSpace(confirmation)

		if confirmation != cfg.Project.Name {
			fmt.Println("\n❌ Confirmation failed. Operation cancelled.")
			return nil
		}
	}

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, targetServerNames, "destroy")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Printf("→ Acquired remote destroy leases: %s\n", leaseSet.Summary())
	}

	fmt.Printf("\n🗑️  Destroying %d server(s)...\n\n", len(serversToDestroy))

	totalErrors := 0

	for _, serverName := range targetServerNames {
		serverCfg := serversToDestroy[serverName]
		fmt.Printf("=== Destroying server: %s (%s) ===\n", serverName, serverCfg.Host)
		if err := destroySingleServer(sshPool, serverName, serverCfg, cfg, envName, verbose, destroyPurgeAll); err != nil {
			fmt.Printf("⚠️  Errors destroying %s: %v\n", serverName, err)
			totalErrors++
		} else {
			fmt.Printf("✓ Server %s destroyed\n\n", serverName)
		}
	}

	// Summary
	if totalErrors > 0 {
		fmt.Printf("⚠️  Destroy completed with %d errors\n", totalErrors)
		fmt.Println("   Run with -v (verbose) flag for more details")
		return fmt.Errorf("destroy incomplete: %d server(s) failed", totalErrors)
	} else {
		fmt.Println("✨ All servers destroyed successfully!")

		if destroyPurgeAll {
			fmt.Println("\n💡 App-owned leftovers pruned. Shared server setup was preserved.")
		} else {
			fmt.Println("\n💡 Server setup preserved. You can redeploy without running 'tako setup'.")
		}
	}

	return nil
}

func destroyEnvironmentTargets(cfg *config.Config, envName string) (map[string]config.ServerConfig, []string, error) {
	envServerNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(envServerNames) == 0 {
		return nil, nil, fmt.Errorf("no servers configured for environment %s", envName)
	}

	servers := make(map[string]config.ServerConfig, len(envServerNames))
	for _, serverName := range envServerNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, nil, fmt.Errorf("server %s not found in config", serverName)
		}
		servers[serverName] = server
	}
	return servers, append([]string(nil), envServerNames...), nil
}

// destroySingleServer handles destruction of a single server.
func destroySingleServer(pool sshClientProvider, serverName string, serverCfg config.ServerConfig, cfg *config.Config, envName string, verbose bool, purgeAll bool) error {
	return destroySingleServerWithHooks(pool, serverName, serverCfg, cfg, envName, verbose, purgeAll, decommissionApp, purgeProjectRuntime)
}

func destroySingleServerWithHooks(pool sshClientProvider, serverName string, serverCfg config.ServerConfig, cfg *config.Config, envName string, verbose bool, purgeAll bool, decommission func(*ssh.Client, *config.Config, string, bool) error, purge func(*ssh.Client, *config.Config, string, bool) error) error {
	if pool == nil {
		return fmt.Errorf("ssh pool is not initialized")
	}
	client, err := pool.GetOrCreateWithAuth(serverCfg.Host, serverCfg.Port, serverCfg.User, serverCfg.SSHKey, serverCfg.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", serverName, err)
	}

	// Decommission application
	if err := decommission(client, cfg, envName, verbose); err != nil {
		return fmt.Errorf("decommission failed: %w", err)
	}

	// Purge server setup if requested
	if purgeAll {
		if err := purge(client, cfg, envName, verbose); err != nil {
			return fmt.Errorf("purge failed: %w", err)
		}
	}

	return nil
}

// decommissionApp stops and removes the deployed application
func decommissionApp(client *ssh.Client, cfg *config.Config, envName string, verbose bool) error {
	if verbose {
		fmt.Println("  → Removing takod-managed services...")
	}
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services for environment %s: %w", envName, err)
	}
	response, err := cleanupViaTakod(client, cfg, takod.CleanupRequest{
		Project:           cfg.Project.Name,
		Environment:       envName,
		RemoveContainers:  true,
		RemoveImages:      true,
		RemoveNetworks:    true,
		RemoveDeployFiles: true,
		RemoveTakodState:  true,
		ProxyFiles:        cleanupProxyFiles(cfg.Project.Name, envName, services),
		ImageRepositories: cleanupImageRepositories(cfg, envName, services),
	})
	if err != nil {
		return err
	}
	if verbose {
		printCleanupWarnings(response)
	}

	if verbose {
		fmt.Println("  ✓ Application decommissioned")
	}

	return nil
}

// purgeProjectRuntime removes app-owned leftovers without touching shared takod
// or tako-proxy runtime used by other projects on the same node.
func purgeProjectRuntime(client *ssh.Client, cfg *config.Config, envName string, verbose bool) error {
	if verbose {
		fmt.Println("  → Pruning app-owned leftovers...")
	}
	response, err := cleanupViaTakod(client, cfg, takod.CleanupRequest{
		Project:     cfg.Project.Name,
		Environment: envName,
		PruneDocker: true,
	})
	if err != nil {
		return err
	}
	if verbose {
		printCleanupWarnings(response)
	}

	if verbose {
		fmt.Println("  ✓ App-owned leftovers pruned")
	}

	return nil
}
