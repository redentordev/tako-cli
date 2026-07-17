package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	liveService string
)

var liveCmd = &cobra.Command{
	Use:   "live",
	Short: "Disable maintenance mode and restore normal service",
	Long: `Disable maintenance mode for a service and restore normal traffic routing.

This command removes the maintenance page container and restores
traffic to the main service.

Examples:
  tako live --service web`,
	RunE: runLive,
}

func init() {
	rootCmd.AddCommand(liveCmd)
	liveCmd.Flags().StringVar(&liveService, "service", "", "Service to restore (required)")
	liveCmd.MarkFlagRequired("service")
}

func runLive(cmd *cobra.Command, args []string) error {
	// Machine modes reserve stdout for parseable output.
	var out io.Writer = os.Stdout
	if machineOutputEnabled() {
		out = os.Stderr
	}

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

	targetServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return err
	}
	if len(targetServers) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}
	targetServers, err = config.ResolveSchedulableMutationTargets(cfg.Servers, targetServers, envName, true)
	if err != nil {
		return err
	}
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	runtimeFactory, err := nodeclient.NewFactory(cfg, sshPool, takodSocketFromConfig(cfg))
	if err != nil {
		return err
	}
	defer runtimeFactory.CloseIdleConnections()
	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, targetServers, "live")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Fprintf(out, "→ Acquired remote live leases: %s\n", leaseSet.Summary())
	}

	fmt.Fprintf(out, "Disabling maintenance mode for %s on %d node(s)...\n\n", liveService, len(targetServers))

	socket := takodSocketFromConfig(cfg)
	networkName := maintenanceNetworkName(cfg.Project.Name, envName)
	request := takod.ReconcileServiceRequest{
		Project:     cfg.Project.Name,
		Environment: envName,
		Service:     maintenanceTakodServiceName(liveService),
		Image:       maintenanceImage,
		Network:     networkName,
	}

	results := runMaintenanceNodeActions(cfg.Servers, targetServers, func(serverName string, server config.ServerConfig) error {
		return disableMaintenanceOnRuntimeNode(cmd.Context(), cfg, runtimeFactory, serverName, socket, envName, liveService, request)
	})
	nodeErrors := printMaintenanceNodeResults(out, "Disabling", "disabled", results)
	ack := maintenanceActionResult(cfg, envName, engine.ActionMaintenanceDisable, liveService, results)
	if len(nodeErrors) > 0 {
		err := fmt.Errorf("maintenance disable failed on %d/%d node(s): %s", len(nodeErrors), len(targetServers), strings.Join(nodeErrors, "; "))
		ack.Error = err.Error()
		if emitErr := emitResultDocument(ack); emitErr != nil {
			return emitErr
		}
		return err
	}

	fmt.Fprintf(out, "✓ Maintenance mode disabled for %s\n", liveService)
	fmt.Fprintf(out, "\nService is now accepting normal traffic.\n")
	fmt.Fprintf(out, "tako-proxy has removed the maintenance routing override.\n")

	return emitResultDocument(ack)
}

func disableMaintenanceOnNode(cfg *config.Config, pool sshClientProvider, server config.ServerConfig, socket string, envName string, serviceName string, request takod.ReconcileServiceRequest) error {
	return runMaintenanceWithClient(pool, server, func(client *ssh.Client) error {
		if _, err := takodclient.RequestJSON(client, socket, "POST", "/v1/reconcile-service", request); err != nil {
			return fmt.Errorf("failed to remove maintenance container: %w", err)
		}
		name := maintenanceProxyConfigFileName(cfg.Project.Name, envName, serviceName)
		if _, err := takodclient.RequestJSON(client, socket, "DELETE", takodclient.ProxyFileEndpoint(name), nil); err != nil {
			return fmt.Errorf("failed to remove maintenance proxy config: %w", err)
		}
		return nil
	})
}

func disableMaintenanceOnRuntimeNode(ctx context.Context, cfg *config.Config, factory *nodeclient.Factory, serverName string, socket string, envName string, serviceName string, request takod.ReconcileServiceRequest) error {
	client, _, err := factory.Client(ctx, serverName)
	if err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}
	if _, err := takodclient.RequestJSONWithContext(ctx, client, socket, "POST", "/v1/reconcile-service", request); err != nil {
		return fmt.Errorf("failed to remove maintenance container: %w", err)
	}
	name := maintenanceProxyConfigFileName(cfg.Project.Name, envName, serviceName)
	if _, err := takodclient.RequestJSONWithContext(ctx, client, socket, "DELETE", takodclient.ProxyFileEndpoint(name), nil); err != nil {
		return fmt.Errorf("failed to remove maintenance proxy config: %w", err)
	}
	return nil
}
