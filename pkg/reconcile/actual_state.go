package reconcile

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takod"
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
	actualByServer := make(map[string]map[string]*ActualService, len(serverNames))
	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, fmt.Errorf("server %s not found", serverName)
		}

		client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}

		nodeState, err := gatherActualStateFromTakod(client, cfg, environment)
		if err != nil {
			return nil, fmt.Errorf("failed to gather actual state from %s through takod: %w", serverName, err)
		}

		actualByServer[serverName] = nodeState
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
	return &clone
}

func gatherActualStateFromTakod(client *ssh.Client, cfg *config.Config, environment string) (map[string]*ActualService, error) {
	socket := "/run/tako/takod.sock"
	if cfg.Runtime != nil && cfg.Runtime.Agent != nil && cfg.Runtime.Agent.Socket != "" {
		socket = cfg.Runtime.Agent.Socket
	}

	requestURL := fmt.Sprintf(
		"http://takod/v1/actual?project=%s&environment=%s",
		queryEscape(cfg.Project.Name),
		queryEscape(environment),
	)
	cmd := fmt.Sprintf(
		"if test -S %[1]s && command -v curl >/dev/null 2>&1; then curl --fail --silent --show-error --unix-socket %[1]s %[2]s; else exit 42; fi",
		shellQuote(socket),
		shellQuote(requestURL),
	)
	output, err := client.Execute(cmd)
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
			Name:       service.Name,
			Image:      service.Image,
			Replicas:   service.Replicas,
			Containers: append([]string(nil), service.Containers...),
			ConfigSnapshot: &config.ServiceConfig{
				Image: service.Image,
			},
		}
	}
	return actualServices, nil
}

func queryEscape(value string) string {
	return url.QueryEscape(value)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
