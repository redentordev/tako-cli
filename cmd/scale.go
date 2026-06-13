package cmd

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/notification"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodstate"
	"github.com/spf13/cobra"
)

var (
	scaleServer string
)

var scaleCmd = &cobra.Command{
	Use:   "scale SERVICE=REPLICAS [SERVICE=REPLICAS...]",
	Short: "Scale takod services to specified replicas",
	Long: `Scale one or more services to a specified number of replicas.

This command reconciles running takod containers without rebuilding images.

Examples:
  tako scale web=5
  tako scale api=3 web=2
  tako scale worker=10
  tako scale web=0

Note: this changes runtime state immediately. Update replicas in tako.yaml if
you want the next full deploy to preserve the same count.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runScale,
}

func init() {
	rootCmd.AddCommand(scaleCmd)
	scaleCmd.Flags().StringVarP(&scaleServer, "server", "s", "", "Scale on specific server")
}

func runScale(cmd *cobra.Command, args []string) error {
	scaleTargets := make(map[string]int)
	for _, arg := range args {
		parts := strings.Split(arg, "=")
		if len(parts) != 2 {
			return fmt.Errorf("invalid format '%s': expected SERVICE=REPLICAS", arg)
		}

		service := strings.TrimSpace(parts[0])
		replicas, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return fmt.Errorf("invalid replica count for %s: %w", service, err)
		}
		if replicas < 0 {
			return fmt.Errorf("replica count cannot be negative for %s", service)
		}

		scaleTargets[service] = replicas
	}

	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if !cfg.IsTakodRuntime() {
		return fmt.Errorf("runtime.mode=%s is not supported; Tako now uses runtime.mode=takod", cfg.GetRuntimeMode())
	}

	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}
	for serviceName := range scaleTargets {
		if _, exists := services[serviceName]; !exists {
			return fmt.Errorf("service '%s' not found in environment %s", serviceName, envName)
		}
	}

	serverNames, err := scaleTargetServers(cfg, envName)
	if err != nil {
		return err
	}
	if len(serverNames) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}

	fmt.Printf("Scaling %d service(s) on %d takod node(s)...\n\n", len(scaleTargets), len(serverNames))

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, serverNames, "scale")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Printf("→ Acquired remote scale leases: %s\n", leaseSet.Summary())
	}

	firstServerName := serverNames[0]
	firstServer := cfg.Servers[firstServerName]
	firstClient, err := sshPool.GetOrCreateWithAuth(firstServer.Host, firstServer.Port, firstServer.User, firstServer.SSHKey, firstServer.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to node %s: %w", firstServerName, err)
	}

	deploy := deployer.NewDeployerWithPool(firstClient, cfg, envName, sshPool, verbose)
	if err := deploy.SetTargetServers(serverNames); err != nil {
		return err
	}
	if err := deploy.SetupTakodRuntime(); err != nil {
		return fmt.Errorf("failed to setup takod runtime: %w", err)
	}

	actualState, err := reconcile.GatherActualStateFromServers(sshPool, cfg, envName, serverNames, nil)
	if err != nil {
		return fmt.Errorf("failed to gather current replica state: %w", err)
	}

	notifier := scaleNotifier(cfg)
	desiredServices := cloneServiceMap(services)
	imageRefs := defaultImageRefs(cfg, envName, desiredServices)
	totalErrors := 0
	for serviceName, desiredReplicas := range scaleTargets {
		currentReplicas := 0
		if actual, ok := actualState[serviceName]; ok {
			currentReplicas = actual.Replicas
		}

		fmt.Printf("-> Scaling %s: %d -> %d replicas\n", serviceName, currentReplicas, desiredReplicas)

		service := services[serviceName]
		service.Replicas = desiredReplicas
		if scaleServer != "" {
			service.Placement = &config.PlacementConfig{
				Strategy: "pinned",
				Servers:  []string{scaleServer},
			}
		}

		imageRef := service.Image
		if imageRef == "" {
			imageRef = cfg.GetFullImageName(serviceName, envName)
		}

		if err := deploy.DeployServiceTakod(serviceName, &service, imageRef); err != nil {
			fmt.Printf("  Failed: %v\n", err)
			totalErrors++
			continue
		}
		desiredServices[serviceName] = service
		imageRefs[serviceName] = imageRef

		fmt.Printf("  ✓ Service %s scaled\n", serviceName)
		if notifier != nil && currentReplicas != desiredReplicas {
			notifier.Notify(notification.ScaleEvent(cfg.Project.Name, envName, serviceName, currentReplicas, desiredReplicas))
		}
	}

	if totalErrors > 0 {
		return fmt.Errorf("scaling completed with %d error(s)", totalErrors)
	}

	if err := deploy.ReconcileTakodProxy(desiredServices); err != nil {
		return fmt.Errorf("scale succeeded but failed to reconcile proxy: %w", err)
	}

	postScaleActualState, err := reconcile.GatherActualStateFromServers(sshPool, cfg, envName, serverNames, nil)
	if err != nil {
		return fmt.Errorf("scale succeeded but failed to gather post-scale actual state: %w", err)
	}
	if err := persistTakodRuntimeState(
		sshPool,
		cfg,
		envName,
		serverNames,
		"scale",
		desiredServices,
		imageRefs,
		postScaleActualState,
		takodstate.GitInfo{},
		"scale.succeeded",
		fmt.Sprintf("scaled %d service(s)", len(scaleTargets)),
		scaleEventDetails(scaleTargets),
	); err != nil {
		return fmt.Errorf("scale succeeded but failed to persist takod state: %w", err)
	}

	fmt.Println("\nAll services scaled successfully.")
	return nil
}

func scaleEventDetails(targets map[string]int) map[string]string {
	details := make(map[string]string, len(targets))
	for serviceName, replicas := range targets {
		details[serviceName] = fmt.Sprintf("%d", replicas)
	}
	return details
}

func scaleTargetServers(cfg *config.Config, envName string) ([]string, error) {
	environmentServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}

	if scaleServer == "" {
		serverNames := append([]string(nil), environmentServers...)
		sort.Strings(serverNames)
		return serverNames, nil
	}

	if _, ok := cfg.Servers[scaleServer]; !ok {
		return nil, fmt.Errorf("server '%s' not found in config", scaleServer)
	}
	for _, serverName := range environmentServers {
		if serverName == scaleServer {
			return []string{scaleServer}, nil
		}
	}

	return nil, fmt.Errorf("server '%s' is not part of environment %s", scaleServer, envName)
}

func scaleNotifier(cfg *config.Config) *notification.Notifier {
	if cfg.Notifications == nil {
		return nil
	}
	if cfg.Notifications.Slack == "" && cfg.Notifications.Discord == "" && cfg.Notifications.Webhook == "" {
		return nil
	}
	return notification.NewNotifier(notification.NotifierConfig{
		SlackWebhook:   cfg.Notifications.Slack,
		DiscordWebhook: cfg.Notifications.Discord,
		Webhook:        cfg.Notifications.Webhook,
	}, verbose)
}
