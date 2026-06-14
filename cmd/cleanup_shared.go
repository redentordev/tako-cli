package cmd

import (
	"fmt"
	"sort"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
)

type cleanupNodeAction func(serverName string, serverCfg config.ServerConfig) (*takod.CleanupResponse, error)

type sshClientProvider interface {
	GetOrCreateWithAuth(host string, port int, user string, keyPath string, password string) (*ssh.Client, error)
}

type cleanupNodeResult struct {
	index      int
	serverName string
	host       string
	response   *takod.CleanupResponse
	err        error
}

func collectCleanupNodes(servers map[string]config.ServerConfig, action cleanupNodeAction) []cleanupNodeResult {
	names := sortedCleanupServerNames(servers)

	resultCh := make(chan cleanupNodeResult, len(names))
	var wg sync.WaitGroup
	for index, serverName := range names {
		serverCfg := servers[serverName]
		wg.Add(1)
		go func(index int, serverName string, serverCfg config.ServerConfig) {
			defer wg.Done()
			response, err := action(serverName, serverCfg)
			resultCh <- cleanupNodeResult{
				index:      index,
				serverName: serverName,
				host:       serverCfg.Host,
				response:   response,
				err:        err,
			}
		}(index, serverName, serverCfg)
	}
	wg.Wait()
	close(resultCh)

	results := make([]cleanupNodeResult, len(names))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

func sortedCleanupServerNames(servers map[string]config.ServerConfig) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cleanupSingleNode(cfg *config.Config, pool sshClientProvider, serverCfg config.ServerConfig, request takod.CleanupRequest) (*takod.CleanupResponse, error) {
	return cleanupSingleNodeWithExecutor(cfg, pool, serverCfg, request, cleanupViaTakod)
}

func cleanupSingleNodeWithExecutor(cfg *config.Config, pool sshClientProvider, serverCfg config.ServerConfig, request takod.CleanupRequest, execute func(*ssh.Client, *config.Config, takod.CleanupRequest) (*takod.CleanupResponse, error)) (*takod.CleanupResponse, error) {
	if pool == nil {
		return nil, fmt.Errorf("ssh pool is not initialized")
	}
	client, err := pool.GetOrCreateWithAuth(serverCfg.Host, serverCfg.Port, serverCfg.User, serverCfg.SSHKey, serverCfg.Password)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}

	response, err := execute(client, cfg, request)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func maintenanceProxyConfigFileName(project string, environment string, serviceName string) string {
	return runtimeid.MaintenanceProxyConfigFileName(project, environment, serviceName)
}
