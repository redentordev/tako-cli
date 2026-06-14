package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var (
	removeForce bool
)

var removeCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove deployed services from the environment mesh",
	Long: `Remove deployed services from every node in the active environment.

This command:
  - Stops and removes all service replicas
  - Removes service images for this project
  - Removes proxy configurations
  - Preserves server setup (takod and tako-proxy remain installed)
  - Does NOT decommission the servers

The environment can be reused for new deployments after removal.
To decommission an environment, use 'tako destroy'.

Examples:
  tako remove
  tako remove --force`,
	RunE: runRemove,
}

func init() {
	rootCmd.AddCommand(removeCmd)
	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Skip confirmation prompt")
}

func runRemove(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	// Get environment
	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services for environment %s: %w", envName, err)
	}
	serverNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(serverNames) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}
	servers := make(map[string]config.ServerConfig, len(serverNames))
	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return fmt.Errorf("server %s not found in configuration", serverName)
		}
		servers[serverName] = server
	}

	fmt.Printf("\n⚠️  WARNING: You are about to REMOVE all services from:\n\n")
	fmt.Printf("   Project:     %s\n", cfg.Project.Name)
	fmt.Printf("   Environment: %s\n", envName)
	fmt.Printf("   Servers:     %d\n\n", len(serverNames))
	for _, serverName := range serverNames {
		server := servers[serverName]
		fmt.Printf("   • %s (%s)\n", serverName, server.Host)
	}
	fmt.Println()

	fmt.Printf("This will:\n")
	fmt.Printf("   • Stop and remove all service replicas for this project\n")
	fmt.Printf("   • Remove service images\n")
	fmt.Printf("   • Remove proxy configurations\n")
	fmt.Printf("   • Remove deployment state\n\n")

	fmt.Printf("This will NOT:\n")
	fmt.Printf("   • Decommission the server\n")
	fmt.Printf("   • Remove takod or tako-proxy\n")
	fmt.Printf("   • Remove persistent volume data\n\n")

	// Confirmation unless --force
	if !removeForce {
		fmt.Printf("Type the project name '%s' to confirm: ", cfg.Project.Name)
		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read input: %w", err)
		}

		input = strings.TrimSpace(input)
		if input != cfg.Project.Name {
			fmt.Println("❌ Confirmation failed. Operation cancelled.")
			return nil
		}
	}

	fmt.Printf("\n🗑️  Removing all services for %s...\n\n", cfg.Project.Name)

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, serverNames, "remove")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Printf("→ Acquired remote remove leases: %s\n", leaseSet.Summary())
	}

	fmt.Printf("→ Reconciling cleanup through takod on %d node(s)...\n", len(serverNames))
	request := removeCleanupRequest(cfg, envName, services)
	results := collectCleanupNodes(servers, func(_ string, serverCfg config.ServerConfig) (*takod.CleanupResponse, error) {
		return cleanupSingleNode(cfg, serverCfg, request)
	})

	var errors []string
	for _, result := range results {
		if result.err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", result.serverName, result.err))
			fmt.Printf("  ✗ %s failed: %v\n", result.serverName, result.err)
			continue
		}
		if verbose {
			printCleanupWarnings(result.response)
		}
		fmt.Printf("  ✓ %s removed\n", result.serverName)
	}
	if len(errors) > 0 {
		return fmt.Errorf("remove incomplete: %s", strings.Join(errors, "; "))
	}

	fmt.Printf("\n✓ All services removed from environment %s\n", envName)
	fmt.Printf("\nThe servers are still provisioned and ready for new deployments.\n")
	fmt.Printf("To deploy again: tako deploy\n")

	return nil
}

func removeCleanupRequest(cfg *config.Config, envName string, services map[string]config.ServiceConfig) takod.CleanupRequest {
	return takod.CleanupRequest{
		Project:           cfg.Project.Name,
		Environment:       envName,
		RemoveContainers:  true,
		RemoveImages:      true,
		RemoveNetworks:    true,
		RemoveDeployFiles: true,
		RemoveTakodState:  true,
		ProxyFiles:        cleanupProxyFiles(cfg.Project.Name, envName, services),
	}
}
