package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
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
	// Image is the running image reference; empty when the selected nodes
	// disagree. Strategy is the recorded deploy strategy label.
	Image    string `json:"image,omitempty"`
	Strategy string `json:"strategy,omitempty"`
	// Health aggregates docker health-check state across the service's
	// active containers (worst wins: unhealthy > starting > healthy).
	// Empty when no container defines a health check or the node agent
	// predates health capture.
	Health string `json:"health,omitempty"`
	// Nodes breaks the replica placement down per selected node; nodes not
	// running the service are omitted.
	Nodes []StatusServiceNode `json:"nodes,omitempty"`
	// Job fields: kind is "job" for scheduled jobs, lastRun carries the most
	// recent run's status, nextRun the owning node's next fire time.
	Kind     string     `json:"kind,omitempty"`
	Schedule string     `json:"schedule,omitempty"`
	LastRun  string     `json:"lastRun,omitempty"`
	NextRun  *time.Time `json:"nextRun,omitempty"`
}

// StatusServiceNode is one node's share of a service row.
type StatusServiceNode struct {
	Name    string `json:"name"`
	Running int    `json:"running"`
	Warming int    `json:"warming,omitempty"`
	Health  string `json:"health,omitempty"`
}

// StatusNodeActualState is one node's actual-state read, used for per-node
// status breakdowns before the mesh-wide merge.
type StatusNodeActualState struct {
	Server   string
	Services map[string]*takod.ActualService
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
	nodeStates, err := GatherStatusActualStateByNode(ctx, cfg, envName, serverNames)
	if err != nil {
		return nil, err
	}
	actualServices := MergeStatusActualStates(nodeStates)

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var jobStatuses map[string]*takod.JobStatus
	if HasJobServices(services) {
		jobStatuses = make(map[string]*takod.JobStatus)
		for _, serverName := range serverNames {
			statuses, err := e.queryNodeJobs(ctx, cfg, envName, serverName)
			if err != nil {
				return nil, err
			}
			for i := range statuses {
				if _, ok := jobStatuses[statuses[i].Name]; !ok {
					jobStatuses[statuses[i].Name] = &statuses[i]
				}
			}
		}
	}

	serviceInfos, err := BuildStatusServiceInfo(cfg.Servers, services, actualServices, jobStatuses, envServers, serverNames, filterService)
	if err != nil {
		return nil, err
	}
	AttachStatusServiceNodes(serviceInfos, nodeStates)

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
	nodeStates, err := GatherStatusActualStateByNode(ctx, cfg, envName, serverNames)
	if err != nil {
		return nil, err
	}
	return MergeStatusActualStates(nodeStates), nil
}

// GatherStatusActualStateByNode reads actual service state from the selected
// nodes using takod over SSH, preserving per-node attribution.
func GatherStatusActualStateByNode(ctx context.Context, cfg *config.Config, envName string, serverNames []string) ([]StatusNodeActualState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	factory, err := nodeclient.NewFactory(cfg, sshPool, TakodSocketFromConfig(cfg))
	if err != nil {
		return nil, err
	}
	defer factory.CloseIdleConnections()

	return GatherStatusActualStateByNodeWith(ctx, cfg.Servers, serverNames, func(serverName string, server config.ServerConfig) (map[string]*takod.ActualService, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		client, _, err := factory.Client(ctx, serverName)
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
	nodeStates, err := GatherStatusActualStateByNodeWith(ctx, servers, serverNames, read)
	if err != nil {
		return nil, err
	}
	return MergeStatusActualStates(nodeStates), nil
}

// GatherStatusActualStateByNodeWith fans out actual-state reads concurrently
// and returns per-node responses in selected server order.
func GatherStatusActualStateByNodeWith(ctx context.Context, servers map[string]config.ServerConfig, serverNames []string, read StatusActualStateReadFunc) ([]StatusNodeActualState, error) {
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

	nodeStates := make([]StatusNodeActualState, len(results))
	for index, result := range results {
		nodeStates[index] = StatusNodeActualState{Server: result.ServerName, Services: result.Services}
	}
	return nodeStates, nil
}

// MergeStatusActualStates merges per-node actual state into the mesh-wide
// view in node order.
func MergeStatusActualStates(nodeStates []StatusNodeActualState) map[string]*takod.ActualService {
	merged := make(map[string]*takod.ActualService)
	for _, nodeState := range nodeStates {
		serviceNames := make([]string, 0, len(nodeState.Services))
		for serviceName := range nodeState.Services {
			serviceNames = append(serviceNames, serviceName)
		}
		sort.Strings(serviceNames)
		for _, serviceName := range serviceNames {
			service := nodeState.Services[serviceName]
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
				existing.Health = takod.MergeHealthStates(existing.Health, service.Health)
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
				Health:            service.Health,
			}
		}
	}
	return merged
}

// AttachStatusServiceNodes fills each container-service row's per-node
// placement breakdown from the per-node actual state. Job and run rows have
// no long-running containers and are left untouched.
func AttachStatusServiceNodes(serviceInfos []StatusService, nodeStates []StatusNodeActualState) {
	for index := range serviceInfos {
		info := &serviceInfos[index]
		if info.Kind != "" {
			continue
		}
		for _, nodeState := range nodeStates {
			actual, ok := nodeState.Services[info.Name]
			if !ok || actual == nil {
				continue
			}
			info.Nodes = append(info.Nodes, StatusServiceNode{
				Name:    nodeState.Server,
				Running: actual.Replicas,
				Warming: len(actual.WarmingContainers),
				Health:  actual.Health,
			})
		}
	}
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
	jobStatuses map[string]*takod.JobStatus,
	envServers []string,
	selectedServers []string,
	filterService string,
) ([]StatusService, error) {
	serviceInfos := make([]StatusService, 0, len(services))
	for serviceName, serviceConfig := range services {
		if filterService != "" && serviceName != filterService {
			continue
		}

		if serviceConfig.IsJob() {
			serviceInfos = append(serviceInfos, buildStatusJobInfo(serviceName, serviceConfig, jobStatuses[serviceName]))
			continue
		}
		if serviceConfig.IsRun() {
			serviceInfos = append(serviceInfos, StatusService{
				Name: serviceName, Kind: config.ServiceKindRun, Status: "deploy-step", Ports: "-", Internal: true,
			})
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
			info.Image = actual.Image
			info.Strategy = actual.DeployStrategy
			info.Health = actual.Health
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

// buildStatusJobInfo renders a kind:job service row: jobs have no replica
// counts, so status reflects whether the cron schedule is registered.
func buildStatusJobInfo(serviceName string, serviceConfig config.ServiceConfig, job *takod.JobStatus) StatusService {
	info := StatusService{
		Name:     serviceName,
		Internal: true,
		Kind:     config.ServiceKindJob,
		Schedule: serviceConfig.Schedule,
		Ports:    "-",
		Status:   "unscheduled",
	}
	if job != nil {
		info.Status = "scheduled"
		info.Schedule = job.Schedule
		if job.NextRun != nil {
			nextRun := job.NextRun.UTC()
			info.NextRun = &nextRun
		}
		if job.LastRun != nil {
			info.LastRun = job.LastRun.Status
		}
	}
	return info
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
func ActualStateViaTakod(client any, cfg *config.Config, environment string) (*takod.ActualStateResponse, error) {
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
