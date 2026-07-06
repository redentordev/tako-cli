package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

const KindStatusResult = "StatusResult"

// StatusRequest describes one status/ps read operation. Config must be loaded
// and Environment must be resolved by the adapter.
type StatusRequest struct {
	Config      *config.Config
	Environment string
	Service     string
	Server      string
}

// StatusResult is the serializable outcome of Status.
type StatusResult struct {
	APIVersion  string          `json:"apiVersion"`
	Kind        string          `json:"kind"`
	Project     string          `json:"project"`
	Environment string          `json:"environment"`
	Server      string          `json:"server,omitempty"`
	Servers     []string        `json:"servers"`
	Service     string          `json:"service,omitempty"`
	Services    []StatusService `json:"services"`
}

// StatusService is one service row in the status/ps result.
type StatusService struct {
	Name     string `json:"name"`
	Desired  int    `json:"desired"`
	Running  int    `json:"running"`
	Status   string `json:"status"`
	Ports    string `json:"ports"`
	Revision string `json:"revision,omitempty"`
	Warming  int    `json:"warming,omitempty"`
	Internal bool   `json:"internal"`
}

// StatusActualStateReadFunc reads actual service state for one server.
type StatusActualStateReadFunc func(serverName string, server config.ServerConfig) (map[string]*takod.ActualService, error)

// StatusActualStateReadResult captures one node's actual-state read result.
type StatusActualStateReadResult struct {
	Index      int
	ServerName string
	Services   map[string]*takod.ActualService
	Err        error
}

// Status returns the takod mesh status for configured services.
func (e *Engine) Status(ctx context.Context, req StatusRequest) (*StatusResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Config == nil {
		return nil, invalidRequestf("status request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("status request requires an environment")
	}

	cfg := req.Config
	for _, server := range cfg.Servers {
		e.RegisterSecret(server.Password)
	}

	envName := req.Environment
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get services: %w", err)
	}
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	serverNames, err := ResolveStatusTargetServerNames(cfg, envName, req.Server)
	if err != nil {
		return nil, err
	}

	filterService := strings.TrimSpace(req.Service)
	if filterService != "" {
		if _, exists := services[filterService]; !exists {
			return nil, invalidRequestf("service '%s' not found in environment %s", filterService, envName)
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	actualServices, err := GatherStatusActualState(ctx, cfg, envName, serverNames)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	serviceInfos, err := BuildStatusServiceInfo(cfg.Servers, services, actualServices, envServers, serverNames, filterService)
	if err != nil {
		return nil, err
	}

	return &StatusResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindStatusResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Server:      strings.TrimSpace(req.Server),
		Servers:     append([]string(nil), serverNames...),
		Service:     filterService,
		Services:    serviceInfos,
	}, nil
}

// ResolveStatusTargetServerNames resolves the status --server selection
// against the configured environment nodes.
func ResolveStatusTargetServerNames(cfg *config.Config, envName string, requestedServer string) ([]string, error) {
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(envServers) == 0 {
		return nil, invalidRequestf("no servers configured for environment %s", envName)
	}
	if requestedServer == "" {
		return envServers, nil
	}
	if _, ok := cfg.Servers[requestedServer]; !ok {
		return nil, invalidRequestf("server %s not found in configuration", requestedServer)
	}
	for _, serverName := range envServers {
		if serverName == requestedServer {
			return []string{requestedServer}, nil
		}
	}
	return nil, invalidRequestf("server %s is not part of environment %s", requestedServer, envName)
}

// GatherStatusActualState reads and merges actual service state from selected
// nodes using takod over SSH.
func GatherStatusActualState(ctx context.Context, cfg *config.Config, envName string, serverNames []string) (map[string]*takod.ActualService, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	return GatherStatusActualStateWith(ctx, cfg.Servers, serverNames, func(serverName string, server config.ServerConfig) (map[string]*takod.ActualService, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to node %s: %w", serverName, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response, err := ActualStateViaTakod(client, cfg, envName)
		if err != nil {
			return nil, fmt.Errorf("failed to query takod on node %s: %w", serverName, err)
		}
		return response.Services, nil
	})
}

// GatherStatusActualStateWith fans out actual-state reads concurrently and
// merges successful node responses in selected server order.
func GatherStatusActualStateWith(ctx context.Context, servers map[string]config.ServerConfig, serverNames []string, read StatusActualStateReadFunc) (map[string]*takod.ActualService, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resultCh := make(chan StatusActualStateReadResult, len(serverNames))
	var wg sync.WaitGroup

	for index, serverName := range serverNames {
		server, ok := servers[serverName]
		if !ok {
			return nil, invalidRequestf("server %s not found in configuration", serverName)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			var services map[string]*takod.ActualService
			var err error
			if ctxErr := ctx.Err(); ctxErr != nil {
				err = ctxErr
			} else {
				services, err = read(serverName, server)
				if ctxErr := ctx.Err(); ctxErr != nil && err == nil {
					err = ctxErr
				}
			}
			resultCh <- StatusActualStateReadResult{
				Index:      index,
				ServerName: serverName,
				Services:   services,
				Err:        err,
			}
		}(index, serverName, server)
	}

	wg.Wait()
	close(resultCh)

	results := make([]StatusActualStateReadResult, len(serverNames))
	var nodeErrors []string
	for result := range resultCh {
		if result.Err != nil {
			nodeErrors = append(nodeErrors, fmt.Sprintf("%s: %v", result.ServerName, result.Err))
			continue
		}
		results[result.Index] = result
	}
	if len(nodeErrors) > 0 {
		sort.Strings(nodeErrors)
		return nil, fmt.Errorf("failed to gather ps actual state: %s", strings.Join(nodeErrors, "; "))
	}

	merged := make(map[string]*takod.ActualService)
	for _, result := range results {
		serviceNames := make([]string, 0, len(result.Services))
		for serviceName := range result.Services {
			serviceNames = append(serviceNames, serviceName)
		}
		sort.Strings(serviceNames)
		for _, serviceName := range serviceNames {
			service := result.Services[serviceName]
			if service == nil {
				continue
			}
			if existing, ok := merged[serviceName]; ok {
				existing.Replicas += service.Replicas
				existing.Containers = append(existing.Containers, service.Containers...)
				if existing.Image == "" {
					existing.Image = service.Image
				}
				existing.RevisionImages = MergeStatusRevisionImageMaps(existing.RevisionImages, service.RevisionImages)
				existing.CurrentRevision = MergeStatusOptionalLabel(existing.CurrentRevision, service.CurrentRevision)
				existing.PreviousRevision = MergeStatusOptionalLabel(existing.PreviousRevision, service.PreviousRevision)
				existing.WarmingRevisions = MergeStatusRevisionLists(existing.WarmingRevisions, service.WarmingRevisions)
				existing.DeployStrategy = MergeStatusOptionalLabel(existing.DeployStrategy, service.DeployStrategy)
				existing.ActiveContainers = append(existing.ActiveContainers, service.ActiveContainers...)
				existing.WarmingContainers = append(existing.WarmingContainers, service.WarmingContainers...)
				continue
			}
			merged[serviceName] = &takod.ActualService{
				Name:              service.Name,
				Image:             service.Image,
				RevisionImages:    CloneStatusStringMap(service.RevisionImages),
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

func MergeStatusOptionalLabel(existing string, incoming string) string {
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

func MergeStatusRevisionLists(existing []string, incoming []string) []string {
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

func MergeStatusRevisionImageMaps(existing map[string]string, incoming map[string]string) map[string]string {
	if len(incoming) == 0 {
		return existing
	}
	out := CloneStatusStringMap(existing)
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

func CloneStatusStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func BuildStatusServiceInfo(
	servers map[string]config.ServerConfig,
	services map[string]config.ServiceConfig,
	actualServices map[string]*takod.ActualService,
	envServers []string,
	selectedServers []string,
	filterService string,
) ([]StatusService, error) {
	serviceInfos := make([]StatusService, 0, len(services))
	for serviceName, serviceConfig := range services {
		if filterService != "" && serviceName != filterService {
			continue
		}

		running := 0
		if actual, ok := actualServices[serviceName]; ok && actual != nil {
			running = actual.Replicas
		}

		desired, err := DesiredReplicasForSelection(servers, serviceConfig, envServers, selectedServers)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve desired placement for %s: %w", serviceName, err)
		}
		info := StatusService{
			Name:     serviceName,
			Desired:  desired,
			Running:  running,
			Internal: serviceConfig.IsInternal() || serviceConfig.IsWorker(),
		}
		if actual, ok := actualServices[serviceName]; ok && actual != nil {
			info.Revision = ShortStatusRevision(actual.CurrentRevision)
			info.Warming = len(actual.WarmingContainers)
		}
		info.Status = ServiceStatus(running, desired)
		info.Ports = ServicePorts(serviceConfig, info.Internal, running)
		serviceInfos = append(serviceInfos, info)
	}

	sort.Slice(serviceInfos, func(i, j int) bool {
		return serviceInfos[i].Name < serviceInfos[j].Name
	})
	return serviceInfos, nil
}

func DesiredReplicasForSelection(servers map[string]config.ServerConfig, service config.ServiceConfig, envServers []string, selectedServers []string) (int, error) {
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

func ServiceStatus(running int, desired int) string {
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

func ServicePorts(service config.ServiceConfig, internal bool, running int) string {
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

func ShortStatusRevision(revision string) string {
	if len(revision) <= 12 {
		return revision
	}
	return revision[:12]
}

// ActualStateViaTakod reads the aggregate actual-state endpoint through takod.
func ActualStateViaTakod(client takodclient.RequestExecutor, cfg *config.Config, environment string) (*takod.ActualStateResponse, error) {
	var response takod.ActualStateResponse
	output, err := takodclient.RequestJSON(
		client,
		TakodSocketFromConfig(cfg),
		"GET",
		takodclient.ActualStateEndpoint(cfg.Project.Name, environment),
		nil,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse takod response: %w", err)
	}
	return &response, nil
}
