package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// RemoteLeaseManager releases remote operation leases.
type RemoteLeaseManager interface {
	ReleaseLease(*remotestate.LeaseInfo) error
}

// RemoteLease is one acquired lease on one node.
type RemoteLease struct {
	ServerName string
	Manager    RemoteLeaseManager
	Lease      *remotestate.LeaseInfo
}

// RemoteLeaseSet holds the leases acquired for one operation across nodes.
type RemoteLeaseSet struct {
	operation string
	leases    []RemoteLease
	// warn receives non-fatal release failures; nil discards them.
	warn func(message string)
}

// SetWarnFunc routes non-fatal lease release failures to a warning consumer.
func (s *RemoteLeaseSet) SetWarnFunc(warn func(message string)) {
	if s != nil {
		s.warn = warn
	}
}

// NewRemoteLeaseSet assembles a lease set from already-acquired leases; used
// by callers that stub lease acquisition.
func NewRemoteLeaseSet(operation string, leases []RemoteLease) *RemoteLeaseSet {
	return &RemoteLeaseSet{
		operation: operation,
		leases:    append([]RemoteLease(nil), leases...),
	}
}

// AcquireRemoteOperationLeases acquires the per-node operation leases that
// serialize mutations for one app/stage across the target mesh nodes.
func AcquireRemoteOperationLeases(pool *ssh.Pool, cfg *config.Config, envName string, serverNames []string, operation string) (*RemoteLeaseSet, error) {
	if pool == nil {
		return nil, fmt.Errorf("ssh pool is not initialized")
	}
	if len(serverNames) == 0 {
		return nil, invalidRequestf("no target nodes configured for %s", operation)
	}

	return AcquireRemoteOperationLeasesWith(cfg.Servers, serverNames, operation, func(serverName string, server config.ServerConfig) (RemoteLease, error) {
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return RemoteLease{}, &ConnectivityError{Server: serverName, Err: fmt.Errorf("failed to connect to lease node %s: %w", serverName, err)}
		}

		manager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, TakodSocketFromConfig(cfg))
		lease, err := manager.AcquireLease(operation, envName, remotestate.DefaultLeaseTTL)
		if err != nil {
			return RemoteLease{}, &LockedError{
				Operation: operation,
				Err:       fmt.Errorf("cannot acquire remote %s lease on %s: %w", operation, serverName, err),
			}
		}

		return RemoteLease{
			ServerName: serverName,
			Manager:    manager,
			Lease:      lease,
		}, nil
	})
}

// RemoteLeaseAcquireFunc acquires one node's lease.
type RemoteLeaseAcquireFunc func(serverName string, server config.ServerConfig) (RemoteLease, error)

type remoteLeaseResult struct {
	index int
	lease RemoteLease
	err   error
}

// AcquireRemoteOperationLeasesWith fans lease acquisition out across nodes,
// releasing every acquired lease when any node fails.
func AcquireRemoteOperationLeasesWith(servers map[string]config.ServerConfig, serverNames []string, operation string, acquire RemoteLeaseAcquireFunc) (*RemoteLeaseSet, error) {
	set := &RemoteLeaseSet{
		operation: operation,
		leases:    make([]RemoteLease, 0, len(serverNames)),
	}

	resultCh := make(chan remoteLeaseResult, len(serverNames))
	var wg sync.WaitGroup

	for index, serverName := range serverNames {
		server, ok := servers[serverName]
		if !ok {
			return nil, invalidRequestf("server %s not found in configuration", serverName)
		}

		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			lease, err := acquire(serverName, server)
			resultCh <- remoteLeaseResult{
				index: index,
				lease: lease,
				err:   err,
			}
		}(index, serverName, server)
	}

	wg.Wait()
	close(resultCh)

	ordered := make([]RemoteLease, len(serverNames))
	var failures []string
	var firstErr error
	for result := range resultCh {
		if result.err != nil {
			failures = append(failures, result.err.Error())
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		ordered[result.index] = result.lease
	}

	for _, lease := range ordered {
		if lease.Lease == nil {
			continue
		}
		set.leases = append(set.leases, lease)
	}

	if len(failures) > 0 {
		set.Release()
		combined := fmt.Errorf("failed to acquire remote %s leases: %s", operation, strings.Join(failures, "; "))
		var locked *LockedError
		if errors.As(firstErr, &locked) {
			return nil, &LockedError{Operation: operation, Holder: locked.Holder, Err: combined}
		}
		var connectivity *ConnectivityError
		if errors.As(firstErr, &connectivity) {
			return nil, &ConnectivityError{Server: connectivity.Server, Err: combined}
		}
		return nil, combined
	}
	return set, nil
}

// Release releases held leases in reverse order. Failures route to the
// warning consumer; release is best-effort by design.
func (s *RemoteLeaseSet) Release() {
	if s == nil {
		return
	}
	for i := len(s.leases) - 1; i >= 0; i-- {
		lease := s.leases[i]
		if lease.Manager == nil || lease.Lease == nil {
			continue
		}
		if err := lease.Manager.ReleaseLease(lease.Lease); err != nil && s.warn != nil {
			s.warn(fmt.Sprintf("Warning: failed to release remote %s lease on %s: %v\n", s.operation, lease.ServerName, err))
		}
	}
}

// Summary lists the held leases as "server:leaseID" pairs.
func (s *RemoteLeaseSet) Summary() string {
	if s == nil || len(s.leases) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.leases))
	for _, lease := range s.leases {
		parts = append(parts, fmt.Sprintf("%s:%s", lease.ServerName, lease.Lease.ID))
	}
	return strings.Join(parts, ", ")
}
