package cmd

import (
	"fmt"
	"strings"
	"sync"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

type remoteLeaseManager interface {
	ReleaseLease(*remotestate.LeaseInfo) error
}

type remoteOperationLease struct {
	serverName string
	manager    remoteLeaseManager
	lease      *remotestate.LeaseInfo
}

type remoteOperationLeaseSet struct {
	operation string
	leases    []remoteOperationLease
}

var acquireRemoteOperationLeasesFunc = acquireRemoteOperationLeases

func acquireRemoteOperationLeases(pool *ssh.Pool, cfg *config.Config, envName string, serverNames []string, operation string) (*remoteOperationLeaseSet, error) {
	if pool == nil {
		return nil, fmt.Errorf("ssh pool is not initialized")
	}
	if len(serverNames) == 0 {
		return nil, fmt.Errorf("no target nodes configured for %s", operation)
	}

	return acquireRemoteOperationLeasesWith(cfg.Servers, serverNames, operation, func(serverName string, server config.ServerConfig) (remoteOperationLease, error) {
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return remoteOperationLease{}, fmt.Errorf("failed to connect to lease node %s: %w", serverName, err)
		}

		manager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, takodSocketFromConfig(cfg))
		lease, err := manager.AcquireLease(operation, envName, remotestate.DefaultLeaseTTL)
		if err != nil {
			return remoteOperationLease{}, fmt.Errorf("cannot acquire remote %s lease on %s: %w", operation, serverName, err)
		}

		return remoteOperationLease{
			serverName: serverName,
			manager:    manager,
			lease:      lease,
		}, nil
	})
}

type remoteOperationLeaseAcquireFunc func(serverName string, server config.ServerConfig) (remoteOperationLease, error)

type remoteOperationLeaseResult struct {
	index int
	lease remoteOperationLease
	err   error
}

func acquireRemoteOperationLeasesWith(servers map[string]config.ServerConfig, serverNames []string, operation string, acquire remoteOperationLeaseAcquireFunc) (*remoteOperationLeaseSet, error) {
	set := &remoteOperationLeaseSet{
		operation: operation,
		leases:    make([]remoteOperationLease, 0, len(serverNames)),
	}

	resultCh := make(chan remoteOperationLeaseResult, len(serverNames))
	var wg sync.WaitGroup

	for index, serverName := range serverNames {
		server, ok := servers[serverName]
		if !ok {
			return nil, fmt.Errorf("server %s not found in configuration", serverName)
		}

		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			lease, err := acquire(serverName, server)
			resultCh <- remoteOperationLeaseResult{
				index: index,
				lease: lease,
				err:   err,
			}
		}(index, serverName, server)
	}

	wg.Wait()
	close(resultCh)

	ordered := make([]remoteOperationLease, len(serverNames))
	var errors []string
	for result := range resultCh {
		if result.err != nil {
			errors = append(errors, result.err.Error())
			continue
		}
		ordered[result.index] = result.lease
	}

	for _, lease := range ordered {
		if lease.lease == nil {
			continue
		}
		set.leases = append(set.leases, lease)
	}

	if len(errors) > 0 {
		set.Release(false)
		return nil, fmt.Errorf("failed to acquire remote %s leases: %s", operation, strings.Join(errors, "; "))
	}
	return set, nil
}

func (s *remoteOperationLeaseSet) Release(verbose bool) {
	if s == nil {
		return
	}
	for i := len(s.leases) - 1; i >= 0; i-- {
		lease := s.leases[i]
		if lease.manager == nil || lease.lease == nil {
			continue
		}
		if err := lease.manager.ReleaseLease(lease.lease); err != nil && verbose {
			fmt.Printf("Warning: failed to release remote %s lease on %s: %v\n", s.operation, lease.serverName, err)
		}
	}
}

func (s *remoteOperationLeaseSet) Summary() string {
	if s == nil || len(s.leases) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.leases))
	for _, lease := range s.leases {
		parts = append(parts, fmt.Sprintf("%s:%s", lease.serverName, lease.lease.ID))
	}
	return strings.Join(parts, ", ")
}
