package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	scaleServer string
)

var scaleCmd = &cobra.Command{
	Use:   "scale SERVICE=REPLICAS [SERVICE=REPLICAS...]",
	Short: "Scale services to specified number of replicas",
	Long: `Scale one or more services to a specified number of replicas.

This command updates the number of running replicas for a service
without rebuilding the image. It's perfect for horizontal scaling.

Examples:
  tako scale web=5              # Scale web service to 5 replicas
  tako scale api=3 web=2        # Scale multiple services
  tako scale worker=10          # Scale background workers
  tako scale web=0              # Scale down to 0 (pause service)

The command will:
  - Deploy new replicas if scaling up
  - Remove excess replicas if scaling down
  - Update load balancer configuration
  - Maintain zero-downtime during scaling

Note: This scales the running containers without rebuilding.
      To deploy new code, use 'tako deploy' instead.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runScale,
}

func init() {
	rootCmd.AddCommand(scaleCmd)
	scaleCmd.Flags().StringVarP(&scaleServer, "server", "s", "", "Scale on specific server (default: all servers)")
}

func runScale(cmd *cobra.Command, args []string) error {
	// Parse scale targets (format: service=replicas)
	scaleTargets := make(map[string]int)
	for _, arg := range args {
		parts := strings.Split(arg, "=")
		if len(parts) != 2 {
			return fmt.Errorf("invalid format '%s': expected SERVICE=REPLICAS", arg)
		}

		service := strings.TrimSpace(parts[0])
		replicas, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return fmt.Errorf("invalid replica count for %s: %v", service, err)
		}

		if replicas < 0 {
			return fmt.Errorf("replica count cannot be negative for %s", service)
		}

		scaleTargets[service] = replicas
	}

	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Get environment and services
	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	// Validate all services exist in environment
	for service := range scaleTargets {
		if _, exists := services[service]; !exists {
			return fmt.Errorf("service '%s' not found in environment %s", service, envName)
		}
	}

	// Determine which servers to scale on
	serversToScale := make(map[string]config.ServerConfig)
	if scaleServer != "" {
		server, ok := cfg.Servers[scaleServer]
		if !ok {
			return fmt.Errorf("server '%s' not found in config", scaleServer)
		}
		serversToScale[scaleServer] = server
	} else {
		serversToScale = cfg.Servers
	}

	fmt.Printf("⚖️  Scaling %d service(s) on %d server(s)...\n\n", len(scaleTargets), len(serversToScale))

	totalErrors := 0

	// Scale on each server
	for serverName, serverCfg := range serversToScale {
		fmt.Printf("=== Scaling on server: %s (%s) ===\n", serverName, serverCfg.Host)

		// Connect to server
		client, err := ssh.NewClient(serverCfg.Host, serverCfg.Port, serverCfg.User, serverCfg.SSHKey)
		if err != nil {
			fmt.Printf("❌ Failed to connect to %s: %v\n\n", serverName, err)
			totalErrors++
			continue
		}

		// Create deployer
		deploy := deployer.NewDeployer(client, cfg, envName, verbose)

		// Scale each service
		for serviceName, desiredReplicas := range scaleTargets {
			service := services[serviceName]

			fmt.Printf("\n→ Scaling %s: ", serviceName)

			// Get current replica count
			currentReplicas, err := getCurrentReplicaCount(client, cfg.Project.Name, serviceName, envName)
			if err != nil {
				fmt.Printf("❌ Failed to get current replicas: %v\n", err)
				totalErrors++
				continue
			}

			fmt.Printf("%d → %d replicas\n", currentReplicas, desiredReplicas)

			// Update service config with new replica count
			service.Replicas = desiredReplicas

			// Deploy with skip-build flag (just scale, don't rebuild)
			if err := deploy.DeployService(serviceName, &service, true); err != nil {
				fmt.Printf("❌ Failed to scale %s: %v\n", serviceName, err)
				totalErrors++
				continue
			}

			fmt.Printf("✓ Service %s scaled successfully\n", serviceName)
		}

		fmt.Printf("\n✓ Server %s scaling completed\n\n", serverName)
		client.Close()
	}

	// Summary
	if totalErrors > 0 {
		fmt.Printf("⚠️  Scaling completed with %d errors\n", totalErrors)
		return nil
	}

	fmt.Println("✨ All services scaled successfully!")
	return nil
}

// getCurrentReplicaCount returns the current number of replicas for a service
func getCurrentReplicaCount(client *ssh.Client, projectName, serviceName string, envName string) (int, error) {
	// Count containers matching pattern: {project}_{environment}_{service}_{number}
	cmd := fmt.Sprintf("docker ps -a --filter 'name=%s_%s_%s_' --format '{{.Names}}' | wc -l", projectName, envName, serviceName)
	output, err := client.Execute(cmd)
	if err != nil {
		return 0, err
	}

	count, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return 0, err
	}

	return count, nil
}
