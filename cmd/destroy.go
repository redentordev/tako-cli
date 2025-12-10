package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/state"
	"github.com/spf13/cobra"
)

var (
	destroyServer   string
	destroyPurgeAll bool
	destroyForce    bool
)

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Remove deployed application and optionally cleanup infrastructure",
	Long: `Destroy the deployed application and cleanup resources.

This command has two modes:

1. DECOMMISSION MODE (default):
   - Stops and removes application containers
   - Removes application Docker images
   - Removes deployment files
   - Keeps Traefik, logs, and server infrastructure
   - Safe for production - can redeploy later

2. PURGE MODE (--purge-all):
   - Everything from decommission mode, PLUS:
   - Removes Traefik reverse proxy configuration
   - Removes access logs
   - Removes all Docker resources
   - Complete cleanup - requires server re-setup

Safety Features:
   - Production servers require explicit confirmation
   - Shows what will be removed before proceeding
   - Use --force to skip confirmation prompts

Examples:
   tako destroy                    # Decommission app, keep infrastructure
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
	destroyCmd.Flags().BoolVar(&destroyPurgeAll, "purge-all", false, "Remove everything including infrastructure (DANGEROUS)")
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

	// Determine which servers to destroy
	serversToDestroy := make(map[string]config.ServerConfig)

	if destroyServer != "" {
		// Destroy specific server
		server, ok := cfg.Servers[destroyServer]
		if !ok {
			return fmt.Errorf("server '%s' not found in config", destroyServer)
		}
		serversToDestroy[destroyServer] = server
	} else {
		// Destroy all servers
		serversToDestroy = cfg.Servers
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
		fmt.Println("   ✓ Remove Traefik reverse proxy configuration")
		fmt.Println("   ✓ Remove access logs")
		fmt.Println("   ✓ Prune all unused Docker resources")
		fmt.Println("\n⚠️  You'll need to run 'tako setup' again to redeploy!")
	} else {
		fmt.Println("\nPreserving infrastructure (Traefik, logs, Docker daemon)")
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

	fmt.Printf("\n🗑️  Destroying %d server(s)...\n\n", len(serversToDestroy))

	// Destroy each server
	totalErrors := 0
	for serverName, serverCfg := range serversToDestroy {
		fmt.Printf("=== Destroying server: %s (%s) ===\n", serverName, serverCfg.Host)

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

		// Decommission application
		if err := decommissionApp(client, cfg.Project.Name, verbose); err != nil {
			fmt.Printf("⚠️  Errors during decommission: %v\n", err)
			totalErrors++
		}

		// Purge infrastructure if requested
		if destroyPurgeAll {
			if err := purgeInfrastructure(client, cfg.Project.Name, verbose); err != nil {
				fmt.Printf("⚠️  Errors during purge: %v\n", err)
				totalErrors++
			}
		}

		fmt.Printf("✓ Server %s destroyed\n\n", serverName)
		client.Close()
	}

	// Summary
	if totalErrors > 0 {
		fmt.Printf("⚠️  Destroy completed with %d errors\n", totalErrors)
		fmt.Println("   Run with -v (verbose) flag for more details")
	} else {
		fmt.Println("✨ All servers destroyed successfully!")

		if destroyPurgeAll {
			fmt.Println("\n💡 Infrastructure removed. Run 'tako setup' before next deployment.")
		} else {
			fmt.Println("\n💡 Infrastructure preserved. You can redeploy without running 'tako setup'.")
		}
	}

	return nil
}

// decommissionApp stops and removes the deployed application
func decommissionApp(client *ssh.Client, projectName string, verbose bool) error {
	// First, remove Swarm services (always use Swarm mode)
	if verbose {
		fmt.Println("  → Removing Swarm services...")
	}

	// List and remove all services for this project
	listServicesCmd := fmt.Sprintf("docker service ls --filter 'name=%s' --format '{{.Name}}'", projectName)
	output, _ := client.Execute(listServicesCmd)
	if strings.TrimSpace(output) != "" {
		removeServicesCmd := fmt.Sprintf("docker service ls --filter 'name=%s' --format '{{.Name}}' | xargs -r docker service rm", projectName)
		client.Execute(removeServicesCmd)
		if verbose {
			fmt.Println("  ✓ Swarm services removed")
		}
	}

	// Remove overlay networks for this project
	if verbose {
		fmt.Println("  → Removing overlay networks...")
	}
	removeNetworkCmd := fmt.Sprintf("docker network ls --filter 'name=tako_%s' --format '{{.Name}}' | xargs -r docker network rm 2>/dev/null || true", projectName)
	client.Execute(removeNetworkCmd)

	if verbose {
		fmt.Println("  → Stopping application containers...")
	}

	// Stop all containers for this project (cleanup any orphaned containers)
	stopCmd := fmt.Sprintf("docker ps -q --filter 'name=%s' | xargs -r docker stop 2>/dev/null || true", projectName)
	client.Execute(stopCmd)

	if verbose {
		fmt.Println("  → Removing application containers...")
	}

	// Remove all containers for this project
	removeCmd := fmt.Sprintf("docker ps -aq --filter 'name=%s' | xargs -r docker rm -f 2>/dev/null || true", projectName)
	client.Execute(removeCmd)

	if verbose {
		fmt.Println("  → Removing application images...")
	}

	// Remove all images for this project (match project name prefix)
	imagesCmd := fmt.Sprintf("docker images --format '{{.Repository}}:{{.Tag}}' | grep '^%s' | xargs -r docker rmi -f 2>/dev/null || true", projectName)
	client.Execute(imagesCmd)

	if verbose {
		fmt.Println("  → Removing deployment files...")
	}

	// Remove deployment directory
	client.Execute(fmt.Sprintf("sudo rm -rf /opt/%s", projectName))

	// Remove deployment state
	client.Execute(fmt.Sprintf("sudo rm -rf /var/lib/%s", projectName))

	// Remove Tako state directory
	client.Execute(fmt.Sprintf("sudo rm -rf /var/lib/tako/%s 2>/dev/null || true", projectName))

	if verbose {
		fmt.Println("  ✓ Application decommissioned")
	}

	return nil
}

// purgeInfrastructure removes all infrastructure (DANGEROUS)
func purgeInfrastructure(client *ssh.Client, projectName string, verbose bool) error {
	if verbose {
		fmt.Println("  → Removing Traefik configuration...")
	}

	// Stop and remove Traefik
	client.Execute("docker stop traefik 2>/dev/null || true")
	client.Execute("docker rm traefik 2>/dev/null || true")

	// Remove Traefik configuration (backup first)
	client.Execute("sudo cp -r /etc/traefik /etc/traefik.bak 2>/dev/null || true")
	client.Execute("sudo rm -rf /etc/traefik")

	if verbose {
		fmt.Println("  → Removing access logs...")
	}

	// Remove logs
	client.Execute(fmt.Sprintf("sudo rm -rf /var/log/traefik/%s-*.log*", projectName))

	if verbose {
		fmt.Println("  → Pruning Docker system...")
	}

	// Prune Docker system
	client.Execute("docker system prune -af --volumes")

	if verbose {
		fmt.Println("  ✓ Infrastructure purged")
	}

	return nil
}
