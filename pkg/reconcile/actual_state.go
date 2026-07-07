package reconcile

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// GatherActualStateFromServers collects takod container state from every
// requested node and aggregates replicas by service.
func GatherActualStateFromServers(
	sshPool *ssh.Pool,
	cfg *config.Config,
	environment string,
	serverNames []string,
	_ *localstate.Manager,
) (map[string]*ActualService, error) {
	actualByServer, err := GatherActualStateByServer(sshPool, cfg, environment, serverNames)
	if err != nil {
		return nil, err
	}
	return AggregateActualStateByServer(actualByServer), nil
}

func GatherActualStateByServer(
	sshPool *ssh.Pool,
	cfg *config.Config,
	environment string,
	serverNames []string,
) (map[string]map[string]*ActualService, error) {
	return gatherActualStateByServerWith(cfg.Servers, serverNames, func(serverName string, server config.ServerConfig) (map[string]*ActualService, error) {
		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}

		nodeState, err := gatherActualStateFromTakod(client, cfg, environment)
		if err != nil {
			return nil, fmt.Errorf("failed to gather actual state from %s through takod: %w", serverName, err)
		}
		return nodeState, nil
	})
}

type actualStateGatherFunc func(serverName string, server config.ServerConfig) (map[string]*ActualService, error)

type actualStateGatherResult struct {
	serverName string
	actual     map[string]*ActualService
	err        error
}

func gatherActualStateByServerWith(servers map[string]config.ServerConfig, serverNames []string, gather actualStateGatherFunc) (map[string]map[string]*ActualService, error) {
	actualByServer := make(map[string]map[string]*ActualService, len(serverNames))
	resultCh := make(chan actualStateGatherResult, len(serverNames))
	var wg sync.WaitGroup

	for _, serverName := range serverNames {
		server, ok := servers[serverName]
		if !ok {
			return nil, fmt.Errorf("server %s not found", serverName)
		}

		wg.Add(1)
		go func(serverName string, server config.ServerConfig) {
			defer wg.Done()
			actual, err := gather(serverName, server)
			resultCh <- actualStateGatherResult{
				serverName: serverName,
				actual:     actual,
				err:        err,
			}
		}(serverName, server)
	}

	wg.Wait()
	close(resultCh)

	var errors []string
	for result := range resultCh {
		if result.err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", result.serverName, result.err))
			continue
		}
		actualByServer[result.serverName] = result.actual
	}

	if len(errors) > 0 {
		sort.Strings(errors)
		return nil, fmt.Errorf("failed to gather actual state: %s", strings.Join(errors, "; "))
	}
	return actualByServer, nil
}

func AggregateActualStateByServer(actualByServer map[string]map[string]*ActualService) map[string]*ActualService {
	actualServices := make(map[string]*ActualService)
	serverNames := make([]string, 0, len(actualByServer))
	for serverName := range actualByServer {
		serverNames = append(serverNames, serverName)
	}
	sort.Strings(serverNames)
	for _, serverName := range serverNames {
		nodeState := actualByServer[serverName]
		for serviceName, serviceState := range nodeState {
			if serviceState == nil {
				continue
			}
			if existing, ok := actualServices[serviceName]; ok {
				existing.Replicas += serviceState.Replicas
				existing.Containers = append(existing.Containers, serviceState.Containers...)
				if existing.Image == "" {
					existing.Image = serviceState.Image
				}
				existing.RevisionImages = mergeRevisionImageMaps(existing.RevisionImages, serviceState.RevisionImages)
				if existing.ConfigHash == "" {
					existing.ConfigHash = serviceState.ConfigHash
				} else if serviceState.ConfigHash != "" && existing.ConfigHash != serviceState.ConfigHash {
					existing.ConfigHash = ""
				}
				existing.RuntimeID = mergeRuntimeID(existing.RuntimeID, serviceState.RuntimeID)
				existing.Persistent = existing.Persistent || serviceState.Persistent
				existing.CurrentRevision = mergeOptionalLabel(existing.CurrentRevision, serviceState.CurrentRevision)
				existing.PreviousRevision = mergeOptionalLabel(existing.PreviousRevision, serviceState.PreviousRevision)
				existing.WarmingRevisions = mergeRevisionLists(existing.WarmingRevisions, serviceState.WarmingRevisions)
				existing.DeployStrategy = mergeOptionalLabel(existing.DeployStrategy, serviceState.DeployStrategy)
				existing.ActiveContainers = append(existing.ActiveContainers, serviceState.ActiveContainers...)
				existing.WarmingContainers = append(existing.WarmingContainers, serviceState.WarmingContainers...)
				continue
			}
			actualServices[serviceName] = cloneActualService(serviceState)
		}
	}
	return actualServices
}

func cloneActualService(service *ActualService) *ActualService {
	if service == nil {
		return nil
	}
	clone := *service
	clone.Containers = append([]string(nil), service.Containers...)
	clone.ActiveContainers = append([]string(nil), service.ActiveContainers...)
	clone.WarmingContainers = append([]string(nil), service.WarmingContainers...)
	clone.WarmingRevisions = append([]string(nil), service.WarmingRevisions...)
	clone.RevisionImages = cloneStringMap(service.RevisionImages)
	return &clone
}

func gatherActualStateFromTakod(client *ssh.Client, cfg *config.Config, environment string) (map[string]*ActualService, error) {
	socket := "/run/tako/takod.sock"
	if cfg.Runtime != nil && cfg.Runtime.Agent != nil && cfg.Runtime.Agent.Socket != "" {
		socket = cfg.Runtime.Agent.Socket
	}

	return gatherActualStateFromTakodWith(client, socket, cfg.Project.Name, environment)
}

func gatherActualStateFromTakodWith(client takodclient.RequestExecutor, socket string, project string, environment string) (map[string]*ActualService, error) {
	output, err := takodclient.RequestJSON(client, socket, "GET", takodclient.ActualStateEndpoint(project, environment), nil)
	if err != nil {
		return nil, err
	}

	var response takod.ActualStateResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse takod actual state: %w", err)
	}

	actualServices := make(map[string]*ActualService, len(response.Services))
	for serviceName, service := range response.Services {
		if service == nil {
			continue
		}
		actualServices[serviceName] = &ActualService{
			Name:              service.Name,
			Image:             service.Image,
			RevisionImages:    cloneStringMap(service.RevisionImages),
			Replicas:          service.Replicas,
			Containers:        append([]string(nil), service.Containers...),
			ConfigHash:        service.ConfigHash,
			RuntimeID:         service.RuntimeID,
			Persistent:        service.Persistent,
			CurrentRevision:   service.CurrentRevision,
			PreviousRevision:  service.PreviousRevision,
			WarmingRevisions:  append([]string(nil), service.WarmingRevisions...),
			DeployStrategy:    service.DeployStrategy,
			ActiveContainers:  append([]string(nil), service.ActiveContainers...),
			WarmingContainers: append([]string(nil), service.WarmingContainers...),
			ConfigSnapshot: &config.ServiceConfig{
				Image:      service.Image,
				Persistent: service.Persistent,
			},
		}
	}
	// Scheduled jobs have no long-running containers; surface each as a
	// zero-replica service whose identity is its cron schedule. A job's
	// transient run container (same service labels) is superseded here.
	for jobName, job := range response.Jobs {
		if job == nil {
			continue
		}
		actualServices[jobName] = &ActualService{
			Name:       jobName,
			Image:      job.Image,
			ConfigHash: job.ConfigHash,
			ConfigSnapshot: &config.ServiceConfig{
				Kind:     config.ServiceKindJob,
				Schedule: job.Schedule,
				Timezone: job.Timezone,
				Image:    job.Image,
			},
		}
	}
	return actualServices, nil
}

func mergeRuntimeID(existing string, incoming string) string {
	if existing == incoming {
		return existing
	}
	return ""
}

func mergeOptionalLabel(existing string, incoming string) string {
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

func mergeRevisionLists(existing []string, incoming []string) []string {
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

func mergeRevisionImageMaps(existing map[string]string, incoming map[string]string) map[string]string {
	if len(incoming) == 0 {
		return existing
	}
	out := cloneStringMap(existing)
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

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
