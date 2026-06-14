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
	removeServer string
	removeForce  bool
)

var removeCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove all deployed services from a server",
	Long: `Remove all deployed services from a server.

This command:
  - Stops and removes all service replicas
  - Removes service images for this project
  - Removes proxy configurations
  - Preserves server setup (takod and tako-proxy remain installed)
  - Does NOT decommission the server

The server can be reused for new deployments after removal.
To decommission an environment, use 'tako destroy'.

Examples:
  tako remove --server prod
  tako remove --server staging --force`,
	RunE: runRemove,
}

func init() {
	rootCmd.AddCommand(removeCmd)
	removeCmd.Flags().StringVarP(&removeServer, "server", "s", "", "Server to remove services from (required)")
	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Skip confirmation prompt")
	removeCmd.MarkFlagRequired("server")
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

	serverName, server, client, err := connectResolvedServer(cfg, envName, removeServer)
	if err != nil {
		return err
	}
	defer client.Close()

	fmt.Printf("\n⚠️  WARNING: You are about to REMOVE all services from:\n\n")
	fmt.Printf("   Server:      %s (%s)\n", serverName, server.Host)
	fmt.Printf("   Project:     %s\n", cfg.Project.Name)
	fmt.Printf("   Environment: %s\n\n", envName)

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
	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, []string{serverName}, "remove")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Printf("→ Acquired remote remove lease: %s\n", leaseSet.Summary())
	}

	fmt.Printf("→ Reconciling cleanup through takod...\n")
	response, err := cleanupViaTakod(client, cfg, takod.CleanupRequest{
		Project:           cfg.Project.Name,
		Environment:       envName,
		RemoveContainers:  true,
		RemoveImages:      true,
		RemoveNetworks:    true,
		RemoveDeployFiles: true,
		RemoveTakodState:  true,
		ProxyFiles:        cleanupProxyFiles(cfg.Project.Name, envName, services),
	})
	if err != nil {
		return fmt.Errorf("failed to cleanup through takod: %w", err)
	}
	if verbose {
		printCleanupWarnings(response)
	}

	fmt.Printf("\n✓ All services removed from %s\n", serverName)
	fmt.Printf("\nThe server is still provisioned and ready for new deployments.\n")
	fmt.Printf("To deploy again: tako deploy\n")

	return nil
}
