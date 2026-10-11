package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var meshPruneServers []string

var meshCmd = &cobra.Command{
	Use:   "mesh",
	Short: "Manage the environment's WireGuard mesh on its servers",
}

var meshPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove the environment's mesh peer set from servers that left it",
	Long: `Remove the selected environment's mesh peer set from declared servers
that are no longer members of it.

Each server keeps one mesh peer set per project and environment and renders
their union. A deploy only contacts the environment's members, so when a
server is removed from an environment's servers list, the set that
environment stored there stays behind. It can later conflict with another
environment's set on that server (409 "mesh address ... is held by ...").

mesh prune reads every declared, non-enrolled server outside the environment
(or the servers named with --server), takes the environment's lease on each
server that still stores the set, deletes the set, and releases the lease.
An unreachable server is reported and skipped; run the command again once it
is reachable. A server that is no longer declared in servers cannot be
reached; declare it again to prune it.

Examples:
  tako mesh prune -e preview
  tako mesh prune -e preview --server old-1`,
	RunE: runMeshPrune,
}

func init() {
	rootCmd.AddCommand(meshCmd)
	meshCmd.AddCommand(meshPruneCmd)
	meshPruneCmd.Flags().StringSliceVar(&meshPruneServers, "server", nil, "Prune only these servers (repeatable)")
	markHumanOnly(meshPruneCmd)
}

func runMeshPrune(cmd *cobra.Command, args []string) error {
	var out io.Writer = os.Stdout
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}
	envName := getEnvironmentName(cfg)
	servers, err := engine.MeshPruneCandidates(cfg, envName, meshPruneServers)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		fmt.Fprintf(out, "No servers outside environment %s to prune.\n", envName)
		return nil
	}
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	ops, closeIdle, err := engine.SSHMeshPruneOps(cfg, sshPool, envName)
	if err != nil {
		return err
	}
	defer closeIdle()
	outcomes, err := engine.PruneMeshPeerSets(cmd.Context(), cfg.Project.Name, envName, servers, ops, func(message string) {
		fmt.Fprintf(out, "  ⚠ %s\n", message)
	})
	printMeshPruneOutcomes(out, cfg.Project.Name, envName, outcomes)
	return err
}

func printMeshPruneOutcomes(out io.Writer, project, environment string, outcomes []engine.MeshPruneNodeOutcome) {
	for _, outcome := range outcomes {
		switch outcome.Outcome {
		case engine.MeshPruneOutcomePruned:
			fmt.Fprintf(out, "✓ %s: removed the %s/%s mesh peer set\n", outcome.Server, project, environment)
		case engine.MeshPruneOutcomeAbsent:
			fmt.Fprintf(out, "· %s: no %s/%s mesh peer set\n", outcome.Server, project, environment)
		case engine.MeshPruneOutcomeUnsupported:
			fmt.Fprintf(out, "· %s: takod keeps no per-environment peer sets\n", outcome.Server)
		case engine.MeshPruneOutcomeUnreachable:
			fmt.Fprintf(out, "⚠ %s: unreachable, skipped\n", outcome.Server)
		default:
			fmt.Fprintf(out, "✗ %s\n", outcome.Error)
		}
	}
}
