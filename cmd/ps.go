package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var psServer string

type ServiceInfo = engine.StatusService

var psCmd = &cobra.Command{
	Use:   "ps [SERVICE]",
	Short: "List running services and their replicas",
	Long: `Show deployed service status from the takod mesh.

This command displays:
  - Running vs desired replica count
  - Service status
  - Configured port or internal service designation

Examples:
  tako ps                    # Show all services in the environment
  tako ps web                # Show a specific service
  tako ps --server prod      # Show the selected node only

Output columns:
  SERVICE   - Service name
  REPLICAS  - Running/desired replica count
  STATUS    - Overall service status
  PORTS     - Configured port or "internal"
  `,
	Args: cobra.MaximumNArgs(1),
	RunE: runPS,
}

func init() {
	rootCmd.AddCommand(psCmd)
	psCmd.Flags().StringVarP(&psServer, "server", "s", "", "Show services on a specific node")
}

func runPS(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	filterService := ""
	if len(args) > 0 {
		filterService = args[0]
	}

	result, err := cliEngine().Status(cmd.Context(), engine.StatusRequest{
		Config:      cfg,
		Environment: getEnvironmentName(cfg),
		Service:     filterService,
		Server:      psServer,
	})
	if result != nil {
		if emitErr := renderPSResult(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	return err
}

func renderPSResult(result *engine.StatusResult) error {
	if result == nil {
		return nil
	}
	if machineOutputEnabled() {
		return emitResultDocument(result)
	}
	if len(result.Services) == 0 {
		fmt.Println("\nNo services configured")
		return nil
	}
	displayServices(result.Services)
	return nil
}

// The status pipeline lives in pkg/engine; the aliases below keep the
// historical cmd-level names for tests that still reference them.

func psTargetServers(cfg *config.Config, envName string) ([]string, error) {
	return engine.ResolveStatusTargetServerNames(cfg, envName, psServer)
}

func gatherPSActualState(cfg *config.Config, envName string, serverNames []string) (map[string]*takod.ActualService, error) {
	return engine.GatherStatusActualState(context.Background(), cfg, envName, serverNames)
}

type psActualStateReadFunc = engine.StatusActualStateReadFunc

type psActualStateReadResult struct {
	index      int
	serverName string
	services   map[string]*takod.ActualService
	err        error
}

func gatherPSActualStateWith(servers map[string]config.ServerConfig, serverNames []string, read psActualStateReadFunc) (map[string]*takod.ActualService, error) {
	return engine.GatherStatusActualStateWith(context.Background(), servers, serverNames, read)
}

func mergePSOptionalLabel(existing string, incoming string) string {
	return engine.MergeStatusOptionalLabel(existing, incoming)
}

func mergePSRevisionLists(existing []string, incoming []string) []string {
	return engine.MergeStatusRevisionLists(existing, incoming)
}

func mergePSRevisionImageMaps(existing map[string]string, incoming map[string]string) map[string]string {
	return engine.MergeStatusRevisionImageMaps(existing, incoming)
}

func clonePSStringMap(values map[string]string) map[string]string {
	return engine.CloneStatusStringMap(values)
}

func buildPSServiceInfo(
	servers map[string]config.ServerConfig,
	services map[string]config.ServiceConfig,
	actualServices map[string]*takod.ActualService,
	envServers []string,
	selectedServers []string,
	filterService string,
) ([]ServiceInfo, error) {
	return engine.BuildStatusServiceInfo(servers, services, actualServices, nil, envServers, selectedServers, filterService)
}

func desiredReplicasForSelection(servers map[string]config.ServerConfig, service config.ServiceConfig, envServers []string, selectedServers []string) (int, error) {
	return engine.DesiredReplicasForSelection(servers, service, envServers, selectedServers)
}

func serviceStatus(running int, desired int) string {
	return engine.ServiceStatus(running, desired)
}

func servicePorts(service config.ServiceConfig, internal bool, running int) string {
	return engine.ServicePorts(service, internal, running)
}

func displayServices(services []ServiceInfo) {
	fmt.Println()
	fmt.Printf("%-15s %-12s %-10s %-15s %-14s %-8s\n", "SERVICE", "REPLICAS", "STATUS", "PORTS", "REVISION", "WARMING")
	fmt.Println(strings.Repeat("─", 90))

	for _, svc := range services {
		replicaStr := fmt.Sprintf("%d/%d", svc.Running, svc.Desired)
		if svc.Desired == 0 {
			replicaStr = fmt.Sprintf("%d", svc.Running)
		}
		if svc.Kind == config.ServiceKindJob {
			replicaStr = "cron"
		}

		statusStr := svc.Status
		if svc.Kind == config.ServiceKindJob && svc.LastRun != "" {
			statusStr = fmt.Sprintf("%s (%s)", svc.Status, svc.LastRun)
		}
		switch svc.Status {
		case "running":
			statusStr = "✓ running"
		case "degraded":
			statusStr = "⚠ degraded"
		case "stopped":
			statusStr = "✗ stopped"
		case "scaling":
			statusStr = "↻ scaling"
		}

		revision := svc.Revision
		if revision == "" {
			revision = "-"
		}
		warming := "-"
		if svc.Warming > 0 {
			warming = fmt.Sprintf("%d", svc.Warming)
		}

		fmt.Printf("%-15s %-12s %-10s %-15s %-14s %-8s\n",
			svc.Name,
			replicaStr,
			statusStr,
			svc.Ports,
			revision,
			warming,
		)
	}

	fmt.Println()
}

func shortRevision(revision string) string {
	return engine.ShortStatusRevision(revision)
}
