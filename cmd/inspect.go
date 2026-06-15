package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var inspectServer string

type inspectRow struct {
	Node      string
	Service   string
	Slot      int
	Container string
	State     string
	Health    string
	Image     string
	Ports     string
	Mounts    string
}

var inspectCmd = &cobra.Command{
	Use:   "inspect [SERVICE]",
	Short: "Inspect app-owned service containers on takod nodes",
	Long: `Inspect app-owned service containers reported by takod on each selected node.

The command is scoped to the current project and environment. It shows safe
runtime metadata such as state, health, image, ports, mounts, and replica slot;
it does not print container environment variables.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceArg,
	RunE:              runInspect,
}

func init() {
	rootCmd.AddCommand(inspectCmd)
	inspectCmd.Flags().StringVarP(&inspectServer, "server", "s", "", "Node to query")
}

func runInspect(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	serviceName := ""
	if len(args) > 0 {
		serviceName = args[0]
		if _, ok := services[serviceName]; !ok {
			return fmt.Errorf("service %s not found in environment %s", serviceName, envName)
		}
	}

	serverNames, err := statePullServerNames(cfg, envName, inspectServer)
	if err != nil {
		return err
	}

	pool := ssh.NewPool()
	defer pool.CloseAll()

	rows, warnings, err := gatherInspectRows(pool, cfg, envName, serviceName, serverNames)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	}
	if len(rows) == 0 {
		fmt.Println("\nNo app-owned containers found")
		return nil
	}
	displayInspectRows(rows)
	return nil
}

func gatherInspectRows(pool *ssh.Pool, cfg *config.Config, envName string, serviceName string, serverNames []string) ([]inspectRow, []string, error) {
	if pool == nil {
		return nil, nil, fmt.Errorf("ssh pool not initialized")
	}
	return gatherInspectRowsWith(cfg.Servers, serverNames, func(serverName string, server config.ServerConfig) ([]inspectRow, error) {
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return nil, fmt.Errorf("connect failed: %w", err)
		}
		endpoint := takodclient.InspectEndpoint(cfg.Project.Name, envName, serviceName)
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", endpoint, nil)
		if err != nil {
			return nil, err
		}
		var response takod.InspectResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return nil, fmt.Errorf("failed to parse inspect response: %w", err)
		}
		return inspectRowsFromResponse(serverName, response, cfg.Project.Name, envName, serviceName)
	})
}

type inspectReadFunc func(serverName string, server config.ServerConfig) ([]inspectRow, error)

func gatherInspectRowsWith(servers map[string]config.ServerConfig, serverNames []string, read inspectReadFunc) ([]inspectRow, []string, error) {
	var rows []inspectRow
	var warnings []string
	successes := 0
	for _, serverName := range serverNames {
		server, ok := servers[serverName]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s: server not found in configuration", serverName))
			continue
		}
		nodeRows, err := read(serverName, server)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", serverName, err))
			continue
		}
		successes++
		rows = append(rows, nodeRows...)
	}
	sort.Strings(warnings)
	if successes == 0 && len(warnings) > 0 {
		return nil, nil, fmt.Errorf("failed to inspect selected nodes: %s", strings.Join(warnings, "; "))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Node == rows[j].Node {
			if rows[i].Service == rows[j].Service {
				if rows[i].Slot == rows[j].Slot {
					return rows[i].Container < rows[j].Container
				}
				return rows[i].Slot < rows[j].Slot
			}
			return rows[i].Service < rows[j].Service
		}
		return rows[i].Node < rows[j].Node
	})
	return rows, warnings, nil
}

func inspectRowsFromResponse(serverName string, response takod.InspectResponse, project string, environment string, serviceFilter string) ([]inspectRow, error) {
	if response.Project != project {
		return nil, fmt.Errorf("project mismatch")
	}
	if response.Environment != environment {
		return nil, fmt.Errorf("environment mismatch")
	}
	node := strings.TrimSpace(response.Node)
	if node == "" {
		node = serverName
	}

	var rows []inspectRow
	for _, container := range response.Containers {
		if serviceFilter != "" && container.Service != serviceFilter {
			return nil, fmt.Errorf("service mismatch")
		}
		if strings.TrimSpace(container.Name) == "" {
			return nil, fmt.Errorf("container name is required")
		}
		if strings.TrimSpace(container.ID) == "" {
			return nil, fmt.Errorf("container id is required")
		}
		if strings.TrimSpace(container.Service) == "" {
			return nil, fmt.Errorf("container service is required")
		}
		rows = append(rows, inspectRow{
			Node:      node,
			Service:   container.Service,
			Slot:      container.Slot,
			Container: displayContainerID(container),
			State:     displayContainerState(container),
			Health:    displayContainerHealth(container),
			Image:     container.Image,
			Ports:     formatInspectPorts(container.Ports),
			Mounts:    formatInspectMounts(container.Mounts),
		})
	}
	return rows, nil
}

func displayContainerID(container takod.InspectContainer) string {
	if container.ShortID != "" {
		return container.Name + "@" + container.ShortID
	}
	if len(container.ID) > 12 {
		return container.Name + "@" + container.ID[:12]
	}
	return container.Name + "@" + container.ID
}

func displayContainerState(container takod.InspectContainer) string {
	state := strings.TrimSpace(container.State)
	if state == "" {
		if container.Running {
			return "running"
		}
		return "unknown"
	}
	if state == "exited" && container.ExitCode != 0 {
		return fmt.Sprintf("exited(%d)", container.ExitCode)
	}
	return state
}

func displayContainerHealth(container takod.InspectContainer) string {
	if strings.TrimSpace(container.Health) != "" {
		return container.Health
	}
	if container.Running {
		return "-"
	}
	return "n/a"
}

func formatInspectPorts(ports []takod.InspectPort) string {
	if len(ports) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		protocol := port.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		private := strconv.Itoa(port.PrivatePort)
		if port.HostPort != "" {
			hostIP := port.HostIP
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}
			parts = append(parts, fmt.Sprintf("%s:%s->%s/%s", hostIP, port.HostPort, private, protocol))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s/%s", private, protocol))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func formatInspectMounts(mounts []takod.InspectMount) string {
	if len(mounts) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		source := mount.Name
		if source == "" {
			source = mount.Source
		}
		if source == "" {
			source = mount.Type
		}
		mode := "ro"
		if mount.RW {
			mode = "rw"
		}
		parts = append(parts, fmt.Sprintf("%s:%s:%s", source, mount.Destination, mode))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func displayInspectRows(rows []inspectRow) {
	fmt.Println()
	fmt.Printf("%-14s %-15s %-6s %-34s %-12s %-10s %-32s %-24s %s\n", "NODE", "SERVICE", "SLOT", "CONTAINER", "STATE", "HEALTH", "IMAGE", "PORTS", "MOUNTS")
	fmt.Println(strings.Repeat("-", 170))
	for _, row := range rows {
		slot := "-"
		if row.Slot > 0 {
			slot = strconv.Itoa(row.Slot)
		}
		fmt.Printf("%-14s %-15s %-6s %-34s %-12s %-10s %-32s %-24s %s\n",
			row.Node,
			row.Service,
			slot,
			truncateResource(row.Container, 34),
			row.State,
			row.Health,
			truncateResource(row.Image, 32),
			truncateResource(row.Ports, 24),
			truncateResource(row.Mounts, 40),
		)
	}
	fmt.Println()
}
