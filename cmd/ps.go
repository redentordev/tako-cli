package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	psServer string
	psAll    bool
)

type ServiceInfo struct {
	Name     string
	Replicas int
	Desired  int
	Running  int
	Status   string
	Ports    string
	Internal bool
}

var psCmd = &cobra.Command{
	Use:   "ps [SERVICE]",
	Short: "List running services and their replicas",
	Long: `Show the status of deployed services and their replicas.

This command displays:
  - Number of running vs desired replicas
  - Container status (running, stopped, unhealthy)
  - Port mappings
  - Uptime information
  - Internal/external service designation

Examples:
  tako ps                    # Show all services
  tako ps web                # Show specific service details
  tako ps --server prod      # Show services on specific server
  tako ps --all             # Include stopped containers

Output columns:
  SERVICE   - Service name
  REPLICAS  - Running/Desired replica count
  STATUS    - Overall service status
  PORTS     - Port mappings or "internal"
  `,
	Args: cobra.MaximumNArgs(1),
	RunE: runPS,
}

func init() {
	rootCmd.AddCommand(psCmd)
	psCmd.Flags().StringVarP(&psServer, "server", "s", "", "Show services on specific server")
	psCmd.Flags().BoolVarP(&psAll, "all", "a", false, "Show all containers including stopped")
}

func runPS(cmd *cobra.Command, args []string) error {
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

	// Determine which servers to query
	serversToQuery := make(map[string]config.ServerConfig)
	if psServer != "" {
		server, ok := cfg.Servers[psServer]
		if !ok {
			return fmt.Errorf("server '%s' not found in config", psServer)
		}
		serversToQuery[psServer] = server
	} else {
		serversToQuery = cfg.Servers
	}

	// Filter by specific service if provided
	var filterService string
	if len(args) > 0 {
		filterService = args[0]
		if _, exists := services[filterService]; !exists {
			return fmt.Errorf("service '%s' not found in environment %s", filterService, envName)
		}
	}

	// Query each server
	for serverName, serverCfg := range serversToQuery {
		if len(serversToQuery) > 1 {
			fmt.Printf("\n=== Server: %s (%s) ===\n", serverName, serverCfg.Host)
		}

		// Connect to server (supports both key and password auth)
		client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
			Host:     serverCfg.Host,
			Port:     serverCfg.Port,
			User:     serverCfg.User,
			SSHKey:   serverCfg.SSHKey,
			Password: serverCfg.Password,
		})
		if err != nil {
			fmt.Printf("❌ Failed to connect to %s: %v\n", serverName, err)
			continue
		}
		if err := client.Connect(); err != nil {
			fmt.Printf("❌ Failed to connect to %s: %v\n", serverName, err)
			continue
		}

		// Get service information
		serviceInfos, err := getServiceInfo(client, cfg, envName, filterService, services)
		if err != nil {
			fmt.Printf("❌ Failed to get service info: %v\n", err)
			client.Close()
			continue
		}

		// Display results
		if len(serviceInfos) == 0 {
			fmt.Println("\nNo services running")
		} else {
			displayServices(serviceInfos, filterService != "")
		}

		client.Close()
	}

	return nil
}

// getServiceInfo retrieves information about running services
func getServiceInfo(client *ssh.Client, cfg *config.Config, envName string, filterService string, services map[string]config.ServiceConfig) ([]ServiceInfo, error) {
	serviceInfos := []ServiceInfo{}

	for serviceName, serviceConfig := range services {
		// Skip if filtering and doesn't match
		if filterService != "" && serviceName != filterService {
			continue
		}

		info := ServiceInfo{
			Name:     serviceName,
			Desired:  0, // Will be set based on actual running containers
			Internal: serviceConfig.IsInternal() || serviceConfig.IsWorker(),
		}

		// Get running containers for this service
		filter := "running"
		if psAll {
			filter = ""
		}

		var cmd string
		if filter != "" {
			cmd = fmt.Sprintf(
				"docker ps --filter 'name=%s_%s_%s_' --filter 'status=%s' --format '{{.Names}}|{{.Status}}|{{.Ports}}'",
				cfg.Project.Name,
				envName,
				serviceName,
				filter,
			)
		} else {
			cmd = fmt.Sprintf(
				"docker ps -a --filter 'name=%s_%s_%s_' --format '{{.Names}}|{{.Status}}|{{.Ports}}'",
				cfg.Project.Name,
				envName,
				serviceName,
			)
		}

		output, err := client.Execute(cmd)
		if err != nil {
			return nil, err
		}

		// Parse container information
		lines := strings.Split(strings.TrimSpace(output), "\n")
		runningCount := 0

		for _, line := range lines {
			if line == "" {
				continue
			}

			parts := strings.Split(line, "|")
			if len(parts) >= 2 {
				status := parts[1]
				if strings.HasPrefix(status, "Up") {
					runningCount++
				}

				// Extract port info from first container
				if info.Ports == "" && len(parts) >= 3 {
					ports := parts[2]
					if ports != "" {
						info.Ports = formatPorts(ports)
					}
				}
			}
		}

		info.Running = runningCount
		info.Replicas = runningCount

		// Set desired based on config, but if we have more running than config, use running count
		configReplicas := serviceConfig.Replicas
		if configReplicas == 0 {
			configReplicas = 1 // Default to 1 if not specified
		}

		// Desired count should be the max of config and running
		// This handles manual scaling where running > config
		if runningCount > configReplicas {
			info.Desired = runningCount
		} else {
			info.Desired = configReplicas
		}

		// Determine status
		if runningCount == 0 {
			info.Status = "stopped"
		} else if runningCount < info.Desired {
			info.Status = "degraded"
		} else if runningCount == info.Desired {
			info.Status = "running"
		} else {
			info.Status = "scaling"
		}

		// Set ports display
		if info.Ports == "" {
			if info.Internal {
				info.Ports = "internal"
			} else if serviceConfig.Port > 0 {
				if info.Running > 1 {
					info.Ports = fmt.Sprintf("%d-%d", serviceConfig.Port, serviceConfig.Port+info.Running-1)
				} else if info.Running == 1 {
					info.Ports = fmt.Sprintf("%d", serviceConfig.Port)
				} else {
					info.Ports = "-"
				}
			} else {
				info.Ports = "-"
			}
		}

		serviceInfos = append(serviceInfos, info)
	}

	// Sort by service name
	sort.Slice(serviceInfos, func(i, j int) bool {
		return serviceInfos[i].Name < serviceInfos[j].Name
	})

	return serviceInfos, nil
}

// getPortDisplay returns the appropriate port display string
func getPortDisplay(serviceConfig config.ServiceConfig, isInternal bool, runningCount int) string {
	if isInternal {
		return "internal"
	}
	if serviceConfig.Port > 0 {
		return fmt.Sprintf("%d/tcp", serviceConfig.Port)
	}
	return "-"
}

// displayServices prints service information in a formatted table
func displayServices(services []ServiceInfo, detailed bool) {
	fmt.Println()
	fmt.Printf("%-15s %-12s %-10s %-15s\n", "SERVICE", "REPLICAS", "STATUS", "PORTS")
	fmt.Println(strings.Repeat("─", 60))

	for _, svc := range services {
		replicaStr := fmt.Sprintf("%d/%d", svc.Running, svc.Desired)
		if svc.Desired == 0 {
			replicaStr = fmt.Sprintf("%d", svc.Running)
		}

		// Status with color indicator
		statusStr := svc.Status
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

		fmt.Printf("%-15s %-12s %-10s %-15s\n",
			svc.Name,
			replicaStr,
			statusStr,
			svc.Ports,
		)
	}

	fmt.Println()
}

// formatPorts formats Docker port mappings for display
func formatPorts(ports string) string {
	if ports == "" {
		return "-"
	}

	// Extract just the host port
	// Example: "0.0.0.0:3000->3000/tcp" -> "3000"
	parts := strings.Split(ports, "->")
	if len(parts) > 0 {
		hostPart := parts[0]
		if strings.Contains(hostPart, ":") {
			portParts := strings.Split(hostPart, ":")
			if len(portParts) > 1 {
				return portParts[len(portParts)-1]
			}
		}
	}

	return ports
}
