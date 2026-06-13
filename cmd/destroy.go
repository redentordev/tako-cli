package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var (
	destroyServer   string
	destroyPurgeAll bool
	destroyForce    bool
)

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Remove deployed application and optionally clean up server setup",
	Long: `Destroy the deployed application and cleanup resources.

This command has two modes:

1. DECOMMISSION MODE (default):
   - Stops and removes application containers
   - Removes application Docker images
   - Removes deployment files
   - Keeps tako-proxy, logs, and server setup
   - Safe for production - can redeploy later

2. PURGE MODE (--purge-all):
   - Everything from decommission mode, PLUS:
   - Removes shared tako-proxy runtime files
   - Removes access logs
   - Removes all Docker resources
   - Complete cleanup - requires server re-setup

Safety Features:
   - Production servers require explicit confirmation
   - Shows what will be removed before proceeding
   - Use --force to skip confirmation prompts

Examples:
   tako destroy                    # Decommission app, keep server setup
   tako destroy --purge-all        # Remove everything (requires confirmation)
   tako destroy --server staging   # Destroy specific server only
   tako destroy --force            # Skip confirmation prompts

⚠️  WARNING: PURGE MODE (--purge-all) removes everything!
   You'll need to run 'tako setup' again to redeploy.`,
	RunE: runDestroy,
}

func init() {
	rootCmd.AddCommand(destroyCmd)
	destroyCmd.Flags().StringVarP(&destroyServer, "server", "s", "", "Specific server to destroy")
	destroyCmd.Flags().BoolVar(&destroyPurgeAll, "purge-all", false, "Remove everything including server setup (DANGEROUS)")
	destroyCmd.Flags().BoolVar(&destroyForce, "force", false, "Skip confirmation prompts")
}

func runDestroy(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Acquire state lock to prevent concurrent operations
	stateLock := state.NewStateLock(".tako")
	lockInfo, err := stateLock.Acquire("destroy")
	if err != nil {
		return fmt.Errorf("cannot destroy: %w", err)
	}
	defer stateLock.Release(lockInfo)

	envName := getEnvironmentName(cfg)
	envServerNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(envServerNames) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}

	// Determine which environment nodes to destroy.
	serversToDestroy := make(map[string]config.ServerConfig)
	targetServerNames := append([]string(nil), envServerNames...)

	if destroyServer != "" {
		// Destroy specific server
		server, ok := cfg.Servers[destroyServer]
		if !ok {
			return fmt.Errorf("server '%s' not found in config", destroyServer)
		}
		found := false
		for _, serverName := range envServerNames {
			if serverName == destroyServer {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("server %s is not part of environment %s", destroyServer, envName)
		}
		serversToDestroy[destroyServer] = server
		targetServerNames = []string{destroyServer}
	} else {
		// Destroy all nodes in the active environment.
		for _, serverName := range envServerNames {
			server, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %s not found in config", serverName)
			}
			serversToDestroy[serverName] = server
		}
	}

	// Show warning and get confirmation
	mode := "DECOMMISSION"
	if destroyPurgeAll {
		mode = "PURGE"
	}

	fmt.Printf("⚠️  WARNING: You are about to %s the following servers:\n\n", mode)
	for serverName, server := range serversToDestroy {
		fmt.Printf("   • %s (%s)\n", serverName, server.Host)
	}

	fmt.Printf("\n%s MODE will:\n", mode)
	fmt.Println("   ✓ Stop and remove all application containers")
	fmt.Println("   ✓ Remove application Docker images")
	fmt.Println("   ✓ Remove deployment files and directories")

	if destroyPurgeAll {
		fmt.Println("   ✓ Remove shared tako-proxy runtime files")
		fmt.Println("   ✓ Remove access logs")
		fmt.Println("   ✓ Prune all unused Docker resources")
		fmt.Println("\n⚠️  You'll need to run 'tako setup' again to redeploy!")
	} else {
		fmt.Println("\nPreserving server setup (tako-proxy, logs, Docker daemon)")
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

	leaseServerName := targetServerNames[0]
	leaseServer := serversToDestroy[leaseServerName]
	leaseClient, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     leaseServer.Host,
		Port:     leaseServer.Port,
		User:     leaseServer.User,
		SSHKey:   leaseServer.SSHKey,
		Password: leaseServer.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to create lease client for %s: %w", leaseServerName, err)
	}
	if err := leaseClient.Connect(); err != nil {
		return fmt.Errorf("failed to connect to lease node %s: %w", leaseServerName, err)
	}
	defer leaseClient.Close()

	stateManager := remotestate.NewStateManager(leaseClient, cfg.Project.Name, leaseServer.Host)
	lease, err := stateManager.AcquireLease("destroy", envName, remotestate.DefaultLeaseTTL)
	if err != nil {
		return fmt.Errorf("cannot acquire remote destroy lease: %w", err)
	}
	defer func() {
		if err := stateManager.ReleaseLease(lease); err != nil && verbose {
			fmt.Printf("Warning: failed to release remote destroy lease: %v\n", err)
		}
	}()
	if verbose {
		fmt.Printf("→ Acquired remote destroy lease on %s (ID: %s)\n", leaseServerName, lease.ID)
	}

	fmt.Printf("\n🗑️  Destroying %d server(s)...\n\n", len(serversToDestroy))

	totalErrors := 0

	for _, serverName := range targetServerNames {
		serverCfg := serversToDestroy[serverName]
		fmt.Printf("=== Destroying server: %s (%s) ===\n", serverName, serverCfg.Host)
		if err := destroySingleServer(serverName, serverCfg, cfg, envName, verbose, destroyPurgeAll); err != nil {
			fmt.Printf("⚠️  Errors destroying %s: %v\n", serverName, err)
			totalErrors++
		} else {
			fmt.Printf("✓ Server %s destroyed\n\n", serverName)
		}
	}

	// Cleanup NFS if configured and purging (only for multi-server)
	if destroyPurgeAll && cfg.IsNFSEnabled() && len(serversToDestroy) > 1 {
		fmt.Printf("\n=== Cleaning up NFS shared storage ===\n\n")
		if err := cleanupNFS(cfg, envName); err != nil {
			fmt.Printf("⚠️  NFS cleanup failed: %v\n", err)
			totalErrors++
		}
	} else if destroyPurgeAll && cfg.IsNFSEnabled() && len(serversToDestroy) == 1 {
		// Single server - just cleanup any local directories that might have been created
		fmt.Printf("\n=== Cleaning up local storage (single-server mode) ===\n\n")
		nfsConfig := cfg.GetNFSConfig()
		if nfsConfig != nil {
			for serverName, serverCfg := range serversToDestroy {
				client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
					Host:     serverCfg.Host,
					Port:     serverCfg.Port,
					User:     serverCfg.User,
					SSHKey:   serverCfg.SSHKey,
					Password: serverCfg.Password,
				})
				if err != nil {
					continue
				}
				if err := client.Connect(); err != nil {
					continue
				}
				defer client.Close()

				// Note: We don't delete the data directories, just inform the user
				fmt.Printf("→ Storage directories on %s preserved:\n", serverName)
				for _, export := range nfsConfig.Exports {
					fmt.Printf("    - %s\n", export.Path)
				}
				fmt.Printf("  Remove manually if no longer needed.\n")
			}
		}
	}

	// Summary
	if totalErrors > 0 {
		fmt.Printf("⚠️  Destroy completed with %d errors\n", totalErrors)
		fmt.Println("   Run with -v (verbose) flag for more details")
	} else {
		fmt.Println("✨ All servers destroyed successfully!")

		if destroyPurgeAll {
			fmt.Println("\n💡 Server setup removed. Run 'tako setup' before next deployment.")
		} else {
			fmt.Println("\n💡 Server setup preserved. You can redeploy without running 'tako setup'.")
		}
	}

	return nil
}

// cleanupNFS removes NFS configuration from all servers
func cleanupNFS(cfg *config.Config, envName string) error {
	nfsConfig := cfg.GetNFSConfig()
	if nfsConfig == nil || !nfsConfig.Enabled {
		return nil
	}

	// Use default environment if not specified
	if envName == "" {
		envName = cfg.GetDefaultEnvironment()
	}
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}

	// Create NFS provisioner
	nfsProvisioner := provisioner.NewNFSProvisioner(cfg.Project.Name, envName, verbose)

	// Build export info for cleanup
	exports := make([]provisioner.NFSExportInfo, 0, len(nfsConfig.Exports))
	for _, export := range nfsConfig.Exports {
		exports = append(exports, provisioner.NFSExportInfo{
			Name:       export.Name,
			Path:       export.Path,
			MountPoint: nfsProvisioner.GetNFSMountPoint(export.Name),
		})
	}

	// Get NFS server name
	nfsServerName, err := cfg.GetNFSServerName(envName)
	if err != nil {
		return fmt.Errorf("failed to determine NFS server: %w", err)
	}

	// Cleanup NFS clients first (unmount before removing server exports)
	for _, serverName := range envServers {
		serverConfig, ok := cfg.Servers[serverName]
		if !ok {
			return fmt.Errorf("server %s not found in config", serverName)
		}
		fmt.Printf("→ Cleaning up NFS on %s...\n", serverName)

		client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
			Host:     serverConfig.Host,
			Port:     serverConfig.Port,
			User:     serverConfig.User,
			SSHKey:   serverConfig.SSHKey,
			Password: serverConfig.Password,
		})
		if err != nil {
			fmt.Printf("  ⚠ Warning: failed to connect to %s: %v\n", serverName, err)
			continue
		}
		if err := client.Connect(); err != nil {
			fmt.Printf("  ⚠ Warning: failed to connect to %s: %v\n", serverName, err)
			continue
		}
		defer client.Close()

		// Cleanup as client (unmount)
		if err := nfsProvisioner.CleanupNFSClient(client, exports); err != nil {
			fmt.Printf("  ⚠ Warning: failed to cleanup NFS client on %s: %v\n", serverName, err)
		}

		// If this is the NFS server, also cleanup server config
		if serverName == nfsServerName {
			if err := nfsProvisioner.CleanupNFSServer(client, exports); err != nil {
				fmt.Printf("  ⚠ Warning: failed to cleanup NFS server on %s: %v\n", serverName, err)
			}
		}

		fmt.Printf("  ✓ NFS cleanup completed on %s\n", serverName)
	}

	fmt.Printf("\n✓ NFS shared storage cleanup completed\n")
	fmt.Printf("  Note: Export directories on the NFS server were preserved.\n")
	fmt.Printf("  Remove manually if needed: %s\n", nfsConfig.Exports[0].Path)

	return nil
}

// destroySingleServer handles destruction of a single server.
func destroySingleServer(serverName string, serverCfg config.ServerConfig, cfg *config.Config, envName string, verbose bool, purgeAll bool) error {
	// Connect to server (supports both key and password auth)
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     serverCfg.Host,
		Port:     serverCfg.Port,
		User:     serverCfg.User,
		SSHKey:   serverCfg.SSHKey,
		Password: serverCfg.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer client.Close()

	// Decommission application
	if err := decommissionApp(client, cfg, envName, verbose); err != nil {
		return fmt.Errorf("decommission failed: %w", err)
	}

	// Purge server setup if requested
	if purgeAll {
		if err := purgeServerSetup(client, cfg, verbose); err != nil {
			return fmt.Errorf("purge failed: %w", err)
		}
	}

	return nil
}

// decommissionApp stops and removes the deployed application
func decommissionApp(client *ssh.Client, cfg *config.Config, envName string, verbose bool) error {
	if verbose {
		fmt.Println("  → Removing takod containers...")
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
		RemoveState:       true,
		RemoveTakodState:  true,
		ProxyFiles:        cleanupProxyFiles(cfg.Project.Name, envName, services),
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

// purgeServerSetup removes shared server-side components installed by Tako.
func purgeServerSetup(client *ssh.Client, cfg *config.Config, verbose bool) error {
	if verbose {
		fmt.Println("  → Removing tako-proxy...")
	}
	response, err := cleanupViaTakod(client, cfg, takod.CleanupRequest{
		Project:              cfg.Project.Name,
		RemoveProxyContainer: true,
		RemoveProxyRuntime:   true,
		PruneDocker:          true,
	})
	if err != nil {
		return err
	}
	if verbose {
		printCleanupWarnings(response)
	}

	if verbose {
		fmt.Println("  ✓ Server setup purged")
	}

	return nil
}
