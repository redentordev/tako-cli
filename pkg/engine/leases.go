package engine

import (
	"context"
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

// RemoteLeaseContextManager releases remote operation leases with cancellation.
type RemoteLeaseContextManager interface {
	ReleaseLeaseContext(context.Context, *remotestate.LeaseInfo) error
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
	return AcquireRemoteOperationLeasesContext(context.Background(), pool, cfg, envName, serverNames, operation)
}

// AcquireRemoteOperationLeasesContext acquires the per-node operation leases
// and returns promptly when ctx is cancelled. In-flight SSH/takod calls may not
// be interruptible, so leases acquired after cancellation are released by a
// best-effort cleanup path.
func AcquireRemoteOperationLeasesContext(ctx context.Context, pool *ssh.Pool, cfg *config.Config, envName string, serverNames []string, operation string) (*RemoteLeaseSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if pool == nil {
		return nil, fmt.Errorf("ssh pool is not initialized")
	}
	if len(serverNames) == 0 {
		return nil, invalidRequestf("no target nodes configured for %s", operation)
	}

	return AcquireRemoteOperationLeasesWithContext(ctx, cfg.Servers, serverNames, operation, func(ctx context.Context, serverName string, server config.ServerConfig) (RemoteLease, error) {
		if err := ctx.Err(); err != nil {
			return RemoteLease{}, err
		}
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return RemoteLease{}, &ConnectivityError{Server: serverName, Err: fmt.Errorf("failed to connect to lease node %s: %w", serverName, err)}
		}
		if err := ctx.Err(); err != nil {
			return RemoteLease{}, err
		}

		manager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, TakodSocketFromConfig(cfg))
		lease, err := manager.AcquireLeaseContext(ctx, operation, envName, remotestate.DefaultLeaseTTL)
		if err != nil {
			return RemoteLease{}, &LockedError{
				Operation: operation,
				Err:       fmt.Errorf("cannot acquire remote %s lease on %s: %w", operation, serverName, err),
			}
		}
		if err := ctx.Err(); err != nil {
			_ = manager.ReleaseLeaseContext(context.Background(), lease)
			return RemoteLease{}, err
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

// RemoteLeaseAcquireContextFunc acquires one node's lease with cancellation.
type RemoteLeaseAcquireContextFunc func(ctx context.Context, serverName string, server config.ServerConfig) (RemoteLease, error)

type remoteLeaseResult struct {
	index int
	lease RemoteLease
	err   error
}

// AcquireRemoteOperationLeasesWith fans lease acquisition out across nodes,
// releasing every acquired lease when any node fails.
func AcquireRemoteOperationLeasesWith(servers map[string]config.ServerConfig, serverNames []string, operation string, acquire RemoteLeaseAcquireFunc) (*RemoteLeaseSet, error) {
	return AcquireRemoteOperationLeasesWithContext(context.Background(), servers, serverNames, operation, func(_ context.Context, serverName string, server config.ServerConfig) (RemoteLease, error) {
		return acquire(serverName, server)
	})
}

// AcquireRemoteOperationLeasesWithContext fans lease acquisition out across
// nodes, returns on context cancellation, and releases every acquired lease
// when any node fails or cancellation wins the fan-out race.
func AcquireRemoteOperationLeasesWithContext(ctx context.Context, servers map[string]config.ServerConfig, serverNames []string, operation string, acquire RemoteLeaseAcquireContextFunc) (*RemoteLeaseSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if acquire == nil {
		return nil, invalidRequestf("lease acquire function is required for %s", operation)
	}

	type leaseTarget struct {
		index      int
		serverName string
		server     config.ServerConfig
	}
	targets := make([]leaseTarget, 0, len(serverNames))
	for index, serverName := range serverNames {
		server, ok := servers[serverName]
		if !ok {
			return nil, invalidRequestf("server %s not found in configuration", serverName)
		}
		targets = append(targets, leaseTarget{index: index, serverName: serverName, server: server})
	}

	resultCh := make(chan remoteLeaseResult, len(targets))
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func(target leaseTarget) {
			defer wg.Done()
			if err := ctx.Err(); err != nil {
				resultCh <- remoteLeaseResult{index: target.index, err: err}
				return
			}
			lease, err := acquire(ctx, target.serverName, target.server)
			if err == nil {
				if cancelErr := ctx.Err(); cancelErr != nil {
					releaseRemoteLease(operation, lease)
					resultCh <- remoteLeaseResult{index: target.index, err: cancelErr}
					return
				}
			}
			resultCh <- remoteLeaseResult{index: target.index, lease: lease, err: err}
		}(target)
	}

	set := &RemoteLeaseSet{
		operation: operation,
		leases:    make([]RemoteLease, 0, len(serverNames)),
	}
	ordered := make([]RemoteLease, len(serverNames))
	var failures []string
	var firstErr error
	for remaining := len(targets); remaining > 0; {
		select {
		case result := <-resultCh:
			remaining--
			if result.err != nil {
				failures = append(failures, result.err.Error())
				if firstErr == nil {
					firstErr = result.err
				}
				continue
			}
			ordered[result.index] = result.lease
		case <-ctx.Done():
			appendAcquiredLeases(set, ordered)
			set.Release()
			go cleanupLeaseResults(operation, resultCh, &wg)
			return nil, fmt.Errorf("failed to acquire remote %s leases: %w", operation, ctx.Err())
		}
	}

	appendAcquiredLeases(set, ordered)
	if len(failures) > 0 {
		set.Release()
		combined := fmt.Errorf("failed to acquire remote %s leases: %s", operation, strings.Join(failures, "; "))
		if errors.Is(firstErr, context.Canceled) || errors.Is(firstErr, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s: %w", combined.Error(), firstErr)
		}
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

func appendAcquiredLeases(set *RemoteLeaseSet, ordered []RemoteLease) {
	for _, lease := range ordered {
		if lease.Lease == nil {
			continue
		}
		set.leases = append(set.leases, lease)
	}
}

func cleanupLeaseResults(operation string, resultCh <-chan remoteLeaseResult, wg *sync.WaitGroup) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	for {
		select {
		case result := <-resultCh:
			if result.err == nil {
				releaseRemoteLease(operation, result.lease)
			}
		case <-done:
			for {
				select {
				case result := <-resultCh:
					if result.err == nil {
						releaseRemoteLease(operation, result.lease)
					}
				default:
					return
				}
			}
		}
	}
}

func releaseRemoteLease(operation string, lease RemoteLease) {
	_ = releaseRemoteLeaseErr(lease)
}

func releaseRemoteLeaseErr(lease RemoteLease) error {
	if lease.Manager == nil || lease.Lease == nil {
		return nil
	}
	if contextManager, ok := lease.Manager.(RemoteLeaseContextManager); ok {
		return contextManager.ReleaseLeaseContext(context.Background(), lease.Lease)
	}
	return lease.Manager.ReleaseLease(lease.Lease)
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
		if err := releaseRemoteLeaseErr(lease); err != nil && s.warn != nil {
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
