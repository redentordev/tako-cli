package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	destroyPurgeAll bool
	destroyForce    bool
)

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Remove deployed application and optionally clean up app-owned leftovers",
	Long: `Destroy the deployed application runtime and clean up app-owned resources.

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

	request := engine.DestroyRequest{
		Config:      cfg,
		Environment: getEnvironmentName(cfg),
		PurgeAll:    destroyPurgeAll,
		Force:       destroyForce,
		Verbose:     verbose,
	}

	session, err := cliEngine().PlanDestroy(cmd.Context(), request)
	if err != nil {
		return err
	}
	defer session.Close()

	// Get confirmation
	if !destroyForce {
		fmt.Printf("\nType the project name '%s' to confirm: ", session.ProjectName())
		reader := bufio.NewReader(os.Stdin)
		confirmation, _ := reader.ReadString('\n')
		confirmation = strings.TrimSpace(confirmation)

		if confirmation != session.ProjectName() {
			fmt.Println("\n❌ Confirmation failed. Operation cancelled.")
			return nil
		}
	}

	_, err = session.Apply(cmd.Context())
	return err
}

// The destroy pipeline lives in pkg/engine; these aliases keep the helpers'
// previous cmd names working for tests and callers.

func destroyEnvironmentTargets(cfg *config.Config, envName string) (map[string]config.ServerConfig, []string, error) {
	return engine.DestroyEnvironmentTargets(cfg, envName)
}

func destroySingleServerWithHooks(pool sshClientProvider, serverName string, serverCfg config.ServerConfig, cfg *config.Config, envName string, verbose bool, purgeAll bool, decommission func(*ssh.Client, *config.Config, string, bool) error, purge func(*ssh.Client, *config.Config, string, bool) error) error {
	return engine.DestroySingleServerWithHooks(pool, serverName, serverCfg, cfg, envName, verbose, purgeAll, decommission, purge)
}
