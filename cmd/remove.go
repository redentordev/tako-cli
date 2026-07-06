package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var (
	removeForce   bool
	removeServers []string
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
  tako remove --server node-b --force
  tako remove --force`,
	RunE: runRemove,
}

func init() {
	rootCmd.AddCommand(removeCmd)
	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Skip confirmation prompt")
	removeCmd.Flags().StringSliceVar(&removeServers, "server", nil, "Only remove services from the named environment server(s)")
}

func runRemove(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	request := engine.RemoveRequest{
		Config:      cfg,
		Environment: getEnvironmentName(cfg),
		Servers:     removeServers,
		Verbose:     verbose,
	}

	session, err := cliEngine().PlanRemove(cmd.Context(), request)
	if err != nil {
		return err
	}
	defer session.Close()

	// Confirmation unless --force
	if !removeForce {
		fmt.Printf("Type the project name '%s' to confirm: ", session.ProjectName())
		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read input: %w", err)
		}

		input = strings.TrimSpace(input)
		if input != session.ProjectName() {
			fmt.Println("❌ Confirmation failed. Operation cancelled.")
			return nil
		}
	}

	_, err = session.Apply(cmd.Context())
	return err
}

// The remove pipeline lives in pkg/engine; these aliases keep the helpers'
// previous cmd names working for tests and callers.

func resolveRemoveTargetServers(envName string, environmentServers []string, selected []string) ([]string, error) {
	return engine.ResolveRemoveTargetServers(envName, environmentServers, selected)
}

func removeCleanupRequest(cfg *config.Config, envName string, services map[string]config.ServiceConfig) takod.CleanupRequest {
	return engine.RemoveCleanupRequest(cfg, envName, services)
}
