package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/swarm"
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

	// For Swarm, we only need to connect to the manager node (first server)
	// Swarm handles distribution across all nodes
	var managerName string
	var managerCfg config.ServerConfig
	for name, server := range serversToScale {
		managerName = name
		managerCfg = server
		break
	}

	fmt.Printf("=== Scaling via Swarm manager: %s (%s) ===\n", managerName, managerCfg.Host)

	// Connect to manager (supports both key and password auth)
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     managerCfg.Host,
		Port:     managerCfg.Port,
		User:     managerCfg.User,
		SSHKey:   managerCfg.SSHKey,
		Password: managerCfg.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to connect to manager %s: %w", managerName, err)
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to manager %s: %w", managerName, err)
	}
	defer client.Close()

	// Create swarm manager
	swarmMgr := swarm.NewManager(verbose)

	// Scale each service
	for serviceName, desiredReplicas := range scaleTargets {
		// Build full service name: {project}_{env}_{service}
		fullServiceName := fmt.Sprintf("%s_%s_%s", cfg.Project.Name, envName, serviceName)

		fmt.Printf("\n→ Scaling %s: ", serviceName)

		// Get current replica count from Swarm
		currentReplicas, err := getSwarmReplicaCount(client, fullServiceName)
		if err != nil {
			fmt.Printf("❌ Failed to get current replicas: %v\n", err)
			totalErrors++
			continue
		}

		fmt.Printf("%d → %d replicas\n", currentReplicas, desiredReplicas)

		// Scale the Swarm service directly
		if err := swarmMgr.ScaleService(client, fullServiceName, desiredReplicas); err != nil {
			fmt.Printf("❌ Failed to scale %s: %v\n", serviceName, err)
			totalErrors++
			continue
		}

		fmt.Printf("✓ Service %s scaled successfully\n", serviceName)
	}

	fmt.Println()

	// Summary
	if totalErrors > 0 {
		fmt.Printf("⚠️  Scaling completed with %d errors\n", totalErrors)
		return nil
	}

	fmt.Println("✨ All services scaled successfully!")
	return nil
}

// getSwarmReplicaCount returns the current number of replicas for a Swarm service
func getSwarmReplicaCount(client *ssh.Client, fullServiceName string) (int, error) {
	// Query Swarm service for replica count
	// Format: "3/3" (running/desired) - we want the desired count
	cmd := fmt.Sprintf("docker service inspect %s --format '{{.Spec.Mode.Replicated.Replicas}}' 2>/dev/null || echo 0", fullServiceName)
	output, err := client.Execute(cmd)
	if err != nil {
		return 0, err
	}

	output = strings.TrimSpace(output)
	if output == "" || output == "<no value>" {
		return 0, nil // Service doesn't exist or is in global mode
	}

	count, err := strconv.Atoi(output)
	if err != nil {
		return 0, err
	}

	return count, nil
}
