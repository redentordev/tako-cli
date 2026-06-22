package cmd

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var psServer string

type ServiceInfo struct {
	Name     string
	Desired  int
	Running  int
	Status   string
	Ports    string
	Revision string
	Warming  int
	Internal bool
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

	serviceInfos, err := buildPSServiceInfo(cfg.Servers, services, actualServices, envServers, serverNames, filterService)
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
				existing.RevisionImages = mergePSRevisionImageMaps(existing.RevisionImages, service.RevisionImages)
				existing.CurrentRevision = mergePSOptionalLabel(existing.CurrentRevision, service.CurrentRevision)
				existing.PreviousRevision = mergePSOptionalLabel(existing.PreviousRevision, service.PreviousRevision)
				existing.WarmingRevisions = mergePSRevisionLists(existing.WarmingRevisions, service.WarmingRevisions)
				existing.DeployStrategy = mergePSOptionalLabel(existing.DeployStrategy, service.DeployStrategy)
				existing.ActiveContainers = append(existing.ActiveContainers, service.ActiveContainers...)
				existing.WarmingContainers = append(existing.WarmingContainers, service.WarmingContainers...)
				continue
			}
			merged[serviceName] = &takod.ActualService{
				Name:              service.Name,
				Image:             service.Image,
				RevisionImages:    clonePSStringMap(service.RevisionImages),
				Replicas:          service.Replicas,
				Containers:        append([]string(nil), service.Containers...),
				CurrentRevision:   service.CurrentRevision,
				PreviousRevision:  service.PreviousRevision,
				WarmingRevisions:  append([]string(nil), service.WarmingRevisions...),
				DeployStrategy:    service.DeployStrategy,
				ActiveContainers:  append([]string(nil), service.ActiveContainers...),
				WarmingContainers: append([]string(nil), service.WarmingContainers...),
			}
		}
	}
	return merged, nil
}

func mergePSOptionalLabel(existing string, incoming string) string {
	if incoming == "" {
		return existing
	}
	if existing == "" {
		return incoming
	}
	if existing == incoming {
		return existing
	}
	return ""
}

func mergePSRevisionLists(existing []string, incoming []string) []string {
	if len(incoming) == 0 {
		return existing
	}
	out := append([]string(nil), existing...)
	for _, revision := range incoming {
		revision = strings.TrimSpace(revision)
		if revision == "" {
			continue
		}
		found := false
		for _, current := range out {
			if current == revision {
				found = true
				break
			}
		}
		if !found {
			out = append(out, revision)
		}
	}
	sort.Strings(out)
	return out
}

func mergePSRevisionImageMaps(existing map[string]string, incoming map[string]string) map[string]string {
	if len(incoming) == 0 {
		return existing
	}
	out := clonePSStringMap(existing)
	if out == nil {
		out = make(map[string]string)
	}
	for revision, image := range incoming {
		revision = strings.TrimSpace(revision)
		image = strings.TrimSpace(image)
		if revision == "" || image == "" {
			continue
		}
		if current := out[revision]; current != "" && current != image {
			out[revision] = ""
			continue
		}
		out[revision] = image
	}
	return out
}

func clonePSStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func buildPSServiceInfo(
	servers map[string]config.ServerConfig,
	services map[string]config.ServiceConfig,
	actualServices map[string]*takod.ActualService,
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
		if actual, ok := actualServices[serviceName]; ok && actual != nil {
			running = actual.Replicas
		}

		desired, err := desiredReplicasForSelection(servers, serviceConfig, envServers, selectedServers)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve desired placement for %s: %w", serviceName, err)
		}
		info := ServiceInfo{
			Name:     serviceName,
			Desired:  desired,
			Running:  running,
			Internal: serviceConfig.IsInternal() || serviceConfig.IsWorker(),
		}
		if actual, ok := actualServices[serviceName]; ok && actual != nil {
			info.Revision = shortRevision(actual.CurrentRevision)
			info.Warming = len(actual.WarmingContainers)
		}
		info.Status = serviceStatus(running, desired)
		info.Ports = servicePorts(serviceConfig, info.Internal, running)
		serviceInfos = append(serviceInfos, info)
	}

	sort.Slice(serviceInfos, func(i, j int) bool {
		return serviceInfos[i].Name < serviceInfos[j].Name
	})
	return serviceInfos, nil
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

func serviceStatus(running int, desired int) string {
	switch {
	case running == 0:
		return "stopped"
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

func servicePorts(service config.ServiceConfig, internal bool, running int) string {
	if internal {
		return "internal"
	}
	if service.Port <= 0 || running == 0 {
		return "-"
	}
	if running > 1 {
		return fmt.Sprintf("%d-%d", service.Port, service.Port+running-1)
	}
	return fmt.Sprintf("%d", service.Port)
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
	if len(revision) <= 12 {
		return revision
	}
	return revision[:12]
}
