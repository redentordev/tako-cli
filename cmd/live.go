package cmd

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
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
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, targetServers, "live")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Printf("→ Acquired remote live leases: %s\n", leaseSet.Summary())
	}

	fmt.Printf("Disabling maintenance mode for %s on %d node(s)...\n\n", liveService, len(targetServers))

	socket := takodSocketFromConfig(cfg)
	networkName := fmt.Sprintf("tako_%s_%s", cfg.Project.Name, envName)
	request := takod.ReconcileServiceRequest{
		Project:     cfg.Project.Name,
		Environment: envName,
		Service:     maintenanceTakodServiceName(liveService),
		Image:       maintenanceImage,
		Network:     networkName,
	}

	results := runMaintenanceNodeActions(cfg.Servers, targetServers, func(_ string, server config.ServerConfig) error {
		return disableMaintenanceOnNode(cfg, server, socket, envName, liveService, request)
	})
	nodeErrors := printMaintenanceNodeResults("Disabling", "disabled", results)
	if len(nodeErrors) > 0 {
		return fmt.Errorf("maintenance disable failed on %d/%d node(s): %s", len(nodeErrors), len(targetServers), strings.Join(nodeErrors, "; "))
	}

	fmt.Printf("✓ Maintenance mode disabled for %s\n", liveService)
	fmt.Printf("\nService is now accepting normal traffic.\n")
	fmt.Printf("tako-proxy has removed the maintenance routing override.\n")

	return nil
}

func disableMaintenanceOnNode(cfg *config.Config, server config.ServerConfig, socket string, envName string, serviceName string, request takod.ReconcileServiceRequest) error {
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     server.Host,
		Port:     server.Port,
		User:     server.User,
		SSHKey:   server.SSHKey,
		Password: server.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to create SSH client: %w", err)
	}
	defer client.Close()

	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}
	if _, err := takodclient.RequestJSON(client, socket, "POST", "/v1/reconcile-service", request); err != nil {
		return fmt.Errorf("failed to remove maintenance container: %w", err)
	}
	name := maintenanceProxyConfigFileName(cfg.Project.Name, envName, serviceName)
	if _, err := takodclient.RequestJSON(client, socket, "DELETE", takodclient.ProxyFileEndpoint(name), nil); err != nil {
		return fmt.Errorf("failed to remove maintenance proxy config: %w", err)
	}
	return nil
}
