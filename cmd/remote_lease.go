package cmd

import (
	"fmt"
	"strings"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

type remoteOperationLease struct {
	serverName string
	manager    *remotestate.StateManager
	lease      *remotestate.LeaseInfo
}

type remoteOperationLeaseSet struct {
	operation string
	leases    []remoteOperationLease
}

func acquireRemoteOperationLeases(pool *ssh.Pool, cfg *config.Config, envName string, serverNames []string, operation string) (*remoteOperationLeaseSet, error) {
	if pool == nil {
		return nil, fmt.Errorf("ssh pool is not initialized")
	}
	if len(serverNames) == 0 {
		return nil, fmt.Errorf("no target nodes configured for %s", operation)
	}

	set := &remoteOperationLeaseSet{
		operation: operation,
		leases:    make([]remoteOperationLease, 0, len(serverNames)),
	}

	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			set.Release(false)
			return nil, fmt.Errorf("server %s not found in configuration", serverName)
		}

		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			set.Release(false)
			return nil, fmt.Errorf("failed to connect to lease node %s: %w", serverName, err)
		}

		manager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, takodSocketFromConfig(cfg))
		lease, err := manager.AcquireLease(operation, envName, remotestate.DefaultLeaseTTL)
		if err != nil {
			set.Release(false)
			return nil, fmt.Errorf("cannot acquire remote %s lease on %s: %w", operation, serverName, err)
		}

		set.leases = append(set.leases, remoteOperationLease{
			serverName: serverName,
			manager:    manager,
			lease:      lease,
		})
	}

	return set, nil
}

func (s *remoteOperationLeaseSet) Release(verbose bool) {
	if s == nil {
		return
	}
	for i := len(s.leases) - 1; i >= 0; i-- {
		lease := s.leases[i]
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
