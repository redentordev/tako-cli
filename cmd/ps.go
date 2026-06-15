package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodstate"
	"github.com/spf13/cobra"
)

var psServer string

type ServiceInfo struct {
	Name                  string
	Desired               int
	Running               int
	Status                string
	Health                string
	Ports                 string
	Internal              bool
	HealthyReplicas       int
	UnhealthyReplicas     int
	StartingReplicas      int
	NoHealthcheckReplicas int
	UnknownHealthReplicas int
}

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

	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}
	serverNames, err := psTargetServers(cfg, envName)
	if err != nil {
		return err
	}

	filterService := ""
	if len(args) > 0 {
		filterService = args[0]
		if _, exists := services[filterService]; !exists {
			return fmt.Errorf("service '%s' not found in environment %s", filterService, envName)
		}
	}

	actualServices, err := gatherPSActualState(cfg, envName, serverNames)
	if err != nil {
		return err
	}
	desiredRevision, err := gatherPSDesiredRevision(cfg, envName, serverNames)
	if err != nil {
		return err
	}

	serviceInfos, err := buildPSServiceInfo(cfg.Servers, services, actualServices, desiredRevision, envServers, serverNames, filterService)
	if err != nil {
		return err
	}
	if len(serviceInfos) == 0 {
		fmt.Println("\nNo services configured")
		return nil
	}

	displayServices(serviceInfos)
	return nil
}

func psTargetServers(cfg *config.Config, envName string) ([]string, error) {
	return statePullServerNames(cfg, envName, psServer)
}

func gatherPSActualState(cfg *config.Config, envName string, serverNames []string) (map[string]*takod.ActualService, error) {
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	return gatherPSActualStateWith(cfg.Servers, serverNames, func(serverName string, server config.ServerConfig) (map[string]*takod.ActualService, error) {
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to node %s: %w", serverName, err)
		}

		response, err := actualStateViaTakod(client, cfg, envName)
		if err != nil {
			return nil, fmt.Errorf("failed to query takod on node %s: %w", serverName, err)
		}
		return response.Services, nil
	})
}

type psActualStateReadFunc func(serverName string, server config.ServerConfig) (map[string]*takod.ActualService, error)

type psActualStateReadResult struct {
	index      int
	serverName string
	services   map[string]*takod.ActualService
	err        error
}

func gatherPSActualStateWith(servers map[string]config.ServerConfig, serverNames []string, read psActualStateReadFunc) (map[string]*takod.ActualService, error) {
	resultCh := make(chan psActualStateReadResult, len(serverNames))
	var wg sync.WaitGroup

	for index, serverName := range serverNames {
		server, ok := servers[serverName]
		if !ok {
			return nil, fmt.Errorf("server %s not found in configuration", serverName)
		}

		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			services, err := read(serverName, server)
			resultCh <- psActualStateReadResult{
				index:      index,
				serverName: serverName,
				services:   services,
				err:        err,
			}
		}(index, serverName, server)
	}

	wg.Wait()
	close(resultCh)

	results := make([]psActualStateReadResult, len(serverNames))
	var nodeErrors []string
	for result := range resultCh {
		if result.err != nil {
			nodeErrors = append(nodeErrors, fmt.Sprintf("%s: %v", result.serverName, result.err))
			continue
		}
		results[result.index] = result
	}
	if len(nodeErrors) > 0 {
		sort.Strings(nodeErrors)
		return nil, fmt.Errorf("failed to gather ps actual state: %s", strings.Join(nodeErrors, "; "))
	}

	merged := make(map[string]*takod.ActualService)
	for _, result := range results {
		serviceNames := make([]string, 0, len(result.services))
		for serviceName := range result.services {
			serviceNames = append(serviceNames, serviceName)
		}
		sort.Strings(serviceNames)
		for _, serviceName := range serviceNames {
			service := result.services[serviceName]
			if service == nil {
				continue
			}
			if existing, ok := merged[serviceName]; ok {
				existing.Replicas += service.Replicas
				existing.Containers = append(existing.Containers, service.Containers...)
				if existing.Image == "" {
					existing.Image = service.Image
				}
				existing.HealthyReplicas += service.HealthyReplicas
				existing.UnhealthyReplicas += service.UnhealthyReplicas
				existing.StartingReplicas += service.StartingReplicas
				existing.NoHealthcheckReplicas += service.NoHealthcheckReplicas
				existing.UnknownHealthReplicas += service.UnknownHealthReplicas
				continue
			}
			merged[serviceName] = &takod.ActualService{
				Name:                  service.Name,
				Image:                 service.Image,
				Replicas:              service.Replicas,
				Containers:            append([]string(nil), service.Containers...),
				HealthyReplicas:       service.HealthyReplicas,
				UnhealthyReplicas:     service.UnhealthyReplicas,
				StartingReplicas:      service.StartingReplicas,
				NoHealthcheckReplicas: service.NoHealthcheckReplicas,
				UnknownHealthReplicas: service.UnknownHealthReplicas,
			}
		}
	}
	return merged, nil
}

func gatherPSDesiredRevision(cfg *config.Config, envName string, serverNames []string) (*takodstate.DesiredRevision, error) {
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	failures := make([]string, 0)
	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			failures = append(failures, fmt.Sprintf("%s: server not found in configuration", serverName))
			continue
		}

		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: connect failed: %v", serverName, err))
			continue
		}

		manager := takodstate.NewManager(client, cfg, envName)
		revision, err := manager.ReadDesired()
		if errors.Is(err, takodstate.ErrNotFound) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", serverName, err))
			continue
		}
		if revision.Project != cfg.Project.Name || revision.Environment != envName {
			failures = append(failures, fmt.Sprintf("%s: desired state project/environment mismatch", serverName))
			continue
		}
		return revision, nil
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		return nil, fmt.Errorf("failed to read ps desired state: %s", strings.Join(failures, "; "))
	}
	return nil, nil
}

func buildPSServiceInfo(
	servers map[string]config.ServerConfig,
	services map[string]config.ServiceConfig,
	actualServices map[string]*takod.ActualService,
	desiredRevision *takodstate.DesiredRevision,
	envServers []string,
	selectedServers []string,
	filterService string,
) ([]ServiceInfo, error) {
	serviceInfos := make([]ServiceInfo, 0, len(services))
	for serviceName, serviceConfig := range services {
		if filterService != "" && serviceName != filterService {
			continue
		}

		running := 0
		var actual *takod.ActualService
		if value, ok := actualServices[serviceName]; ok && value != nil {
			actual = value
			running = actual.Replicas
		}

		desired, err := desiredReplicasForService(servers, serviceName, serviceConfig, desiredRevision, envServers, selectedServers)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve desired placement for %s: %w", serviceName, err)
		}
		info := ServiceInfo{
			Name:                  serviceName,
			Desired:               desired,
			Running:               running,
			Internal:              serviceConfig.IsInternal() || serviceConfig.IsWorker(),
			Health:                serviceHealthSummary(actual, running),
			HealthyReplicas:       actualHealthCount(actual, "healthy"),
			UnhealthyReplicas:     actualHealthCount(actual, "unhealthy"),
			StartingReplicas:      actualHealthCount(actual, "starting"),
			NoHealthcheckReplicas: actualHealthCount(actual, "none"),
			UnknownHealthReplicas: actualHealthCount(actual, "unknown"),
		}
		info.Status = serviceStatus(running, desired, actual)
		info.Ports = servicePorts(serviceConfig, info.Internal, running)
		serviceInfos = append(serviceInfos, info)
	}

	sort.Slice(serviceInfos, func(i, j int) bool {
		return serviceInfos[i].Name < serviceInfos[j].Name
	})
	return serviceInfos, nil
}

func desiredReplicasForService(
	servers map[string]config.ServerConfig,
	serviceName string,
	service config.ServiceConfig,
	desiredRevision *takodstate.DesiredRevision,
	envServers []string,
	selectedServers []string,
) (int, error) {
	targetServers := envServers
	desiredService := service
	if desiredRevision != nil {
		if runtimeService, ok := desiredRevision.Services[serviceName]; ok {
			desiredService.Replicas = runtimeService.Replicas
			desiredService.Placement = runtimeService.Placement
			if len(desiredRevision.TargetNodes) > 0 {
				targetServers = desiredRevision.TargetNodes
			}
		}
	}
	return desiredReplicasForSelection(servers, desiredService, targetServers, selectedServers)
}

func desiredReplicasForSelection(servers map[string]config.ServerConfig, service config.ServiceConfig, envServers []string, selectedServers []string) (int, error) {
	replicas := service.Replicas
	if replicas <= 0 {
		replicas = 1
	}

	targets, err := config.ResolvePlacementTargets(service.Placement, servers, envServers, "selected environment")
	if err != nil {
		return 0, err
	}
	if service.Placement != nil && strings.TrimSpace(service.Placement.Strategy) == "global" {
		replicas = len(targets)
	}

	if len(targets) == 0 || replicas <= 0 {
		return 0, nil
	}

	selected := make(map[string]bool, len(selectedServers))
	for _, serverName := range selectedServers {
		selected[serverName] = true
	}

	count := 0
	for slot := 1; slot <= replicas; slot++ {
		serverName := targets[(slot-1)%len(targets)]
		if selected[serverName] {
			count++
		}
	}
	return count, nil
}

func serviceStatus(running int, desired int, actual *takod.ActualService) string {
	switch {
	case actual != nil && actual.UnhealthyReplicas > 0:
		return "unhealthy"
	case running == 0:
		return "stopped"
	case actual != nil && actual.StartingReplicas > 0:
		return "starting"
	case desired == 0:
		return "running"
	case running < desired:
		return "degraded"
	case running == desired:
		return "running"
	default:
		return "scaling"
	}
}

func actualHealthCount(actual *takod.ActualService, kind string) int {
	if actual == nil {
		return 0
	}
	switch kind {
	case "healthy":
		return actual.HealthyReplicas
	case "unhealthy":
		return actual.UnhealthyReplicas
	case "starting":
		return actual.StartingReplicas
	case "none":
		return actual.NoHealthcheckReplicas
	case "unknown":
		return actual.UnknownHealthReplicas
	default:
		return 0
	}
}

func serviceHealthSummary(actual *takod.ActualService, running int) string {
	if actual == nil || running == 0 {
		return "-"
	}
	counts := []struct {
		label string
		count int
	}{
		{label: "healthy", count: actual.HealthyReplicas},
		{label: "unhealthy", count: actual.UnhealthyReplicas},
		{label: "starting", count: actual.StartingReplicas},
		{label: "unknown", count: actual.UnknownHealthReplicas},
	}
	var parts []string
	for _, item := range counts {
		if item.count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", item.count, item.label))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func servicePorts(service config.ServiceConfig, internal bool, running int) string {
	ports := service.EffectivePorts()
	if len(ports) == 0 || running == 0 {
		return "-"
	}

	var parts []string
	for _, port := range ports {
		switch port.Mode {
		case "proxy":
			label := port.Name
			if label == "" {
				label = "http"
			}
			if len(ports) == 1 && port.Name == "http" && service.Port > 0 {
				if running > 1 {
					return fmt.Sprintf("%d-%d", service.Port, service.Port+running-1)
				}
				return fmt.Sprintf("%d", service.Port)
			}
			parts = append(parts, fmt.Sprintf("%s:%d", label, port.Target))
		case "host":
			label := port.Name
			if label == "" {
				label = "host"
			}
			published := port.Published
			if published <= 0 {
				published = port.Target
			}
			spec := fmt.Sprintf("%s:%d->%d", label, published, port.Target)
			if port.Protocol == "udp" {
				spec += "/udp"
			}
			parts = append(parts, spec)
		default:
			label := port.Name
			if label == "" {
				label = "internal"
			}
			parts = append(parts, fmt.Sprintf("%s:%d/internal", label, port.Target))
		}
	}
	if len(parts) == 0 {
		return "internal"
	}
	if internal && len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts, ",")
}

func displayServices(services []ServiceInfo) {
	fmt.Println()
	fmt.Printf("%-15s %-12s %-13s %-24s %-15s\n", "SERVICE", "REPLICAS", "STATUS", "HEALTH", "PORTS")
	fmt.Println(strings.Repeat("─", 85))

	for _, svc := range services {
		replicaStr := fmt.Sprintf("%d/%d", svc.Running, svc.Desired)
		if svc.Desired == 0 {
			replicaStr = fmt.Sprintf("%d", svc.Running)
		}

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
		case "starting":
			statusStr = "... starting"
		case "unhealthy":
			statusStr = "x unhealthy"
		}

		fmt.Printf("%-15s %-12s %-13s %-24s %-15s\n",
			svc.Name,
			replicaStr,
			statusStr,
			truncateResource(svc.Health, 24),
			svc.Ports,
		)
	}

	fmt.Println()
}
