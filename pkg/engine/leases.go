package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
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

// RemoteLeaseRenewContextManager renews a remote lease using its existing ID
// holder token. StateManager implements this optional interface.
type RemoteLeaseRenewContextManager interface {
	RenewLeaseContext(context.Context, *remotestate.LeaseInfo, time.Duration) (*remotestate.LeaseInfo, error)
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
	mu        sync.RWMutex
	// warn receives non-fatal release failures; nil discards them.
	warn        func(message string)
	renewCancel context.CancelFunc
	renewDone   chan struct{}
	releaseOnce sync.Once
	lostOnce    sync.Once
	lostErr     error
	lost        chan struct{}
	released    chan struct{}
}

// SetWarnFunc routes non-fatal lease release failures to a warning consumer.
func (s *RemoteLeaseSet) SetWarnFunc(warn func(message string)) {
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.warn = warn
	}
}

// NewRemoteLeaseSet assembles a lease set from already-acquired leases; used
// by callers that stub lease acquisition.
func NewRemoteLeaseSet(operation string, leases []RemoteLease) *RemoteLeaseSet {
	return &RemoteLeaseSet{
		operation: operation,
		leases:    append([]RemoteLease(nil), leases...),
		lost:      make(chan struct{}),
		released:  make(chan struct{}),
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
	factory, err := nodeclient.NewFactory(cfg, pool, TakodSocketFromConfig(cfg))
	if err != nil {
		return nil, err
	}

	acquireCtx, cancelAcquire := context.WithTimeout(ctx, remotestate.DefaultLeaseTTL/4)
	defer cancelAcquire()
	set, err := AcquireRemoteOperationLeasesWithContext(acquireCtx, cfg.Servers, serverNames, operation, func(ctx context.Context, serverName string, server config.ServerConfig) (RemoteLease, error) {
		if err := ctx.Err(); err != nil {
			return RemoteLease{}, err
		}
		client, _, err := factory.Client(ctx, serverName)
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
	if err != nil {
		return nil, err
	}
	// Keep the holder token alive for long deploys (for example, a first
	// Sentry deployment can exceed the default 30-minute lease). Renewal is
	// stopped and joined before Release removes any lease.
	set.startRenewal(remotestate.DefaultLeaseTTL, remotestate.DefaultLeaseTTL/4)
	if renewErr := set.Err(); renewErr != nil {
		set.Release()
		return nil, renewErr
	}
	return set, nil
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
		lost:      make(chan struct{}),
		released:  make(chan struct{}),
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

const remoteLeaseRenewRequestTimeout = 30 * time.Second

func (s *RemoteLeaseSet) startRenewal(ttl time.Duration, interval time.Duration) {
	if s == nil || ttl <= 0 || interval <= 0 {
		return
	}

	s.mu.Lock()
	if s.renewCancel != nil {
		s.mu.Unlock()
		return
	}
	hasRenewer := false
	for _, lease := range s.leases {
		if _, ok := lease.Manager.(RemoteLeaseRenewContextManager); ok && lease.Lease != nil {
			hasRenewer = true
			break
		}
	}
	if !hasRenewer {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.renewCancel = cancel
	s.renewDone = done
	s.mu.Unlock()

	// Refresh every lease immediately after bounded fan-out acquisition so
	// early nodes receive a full TTL before the caller starts work.
	s.renewAll(ctx, ttl, interval)
	if s.Err() != nil {
		cancel()
		close(done)
		return
	}
	go s.runRenewalLoop(ctx, done, ttl, interval)
}

func (s *RemoteLeaseSet) runRenewalLoop(ctx context.Context, done chan<- struct{}, ttl time.Duration, interval time.Duration) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.renewAll(ctx, ttl, interval)
			if s.Err() != nil {
				return
			}
		}
	}
}

func (s *RemoteLeaseSet) renewAll(ctx context.Context, ttl time.Duration, interval time.Duration) {
	s.mu.RLock()
	leases := append([]RemoteLease(nil), s.leases...)
	s.mu.RUnlock()

	var wg sync.WaitGroup
	for index, lease := range leases {
		renewer, ok := lease.Manager.(RemoteLeaseRenewContextManager)
		if !ok || lease.Lease == nil {
			continue
		}
		wg.Add(1)
		go func(index int, lease RemoteLease, renewer RemoteLeaseRenewContextManager) {
			defer wg.Done()
			timeout := remoteLeaseRenewRequestTimeout
			if ttl/4 < timeout {
				timeout = ttl / 4
			}
			if timeout <= 0 {
				timeout = time.Second
			}
			renewCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			renewed, err := renewer.RenewLeaseContext(renewCtx, lease.Lease, ttl)
			if err != nil {
				s.warnf("Warning: failed to renew remote %s lease on %s: %v\n", s.operation, lease.ServerName, err)
				// A confirmed holder mismatch/expiry is immediately fatal. For
				// transport failures, fail closed while enough confirmed TTL
				// remains to stop the operation before another holder can enter.
				margin := interval + timeout
				if errors.Is(err, remotestate.ErrLeaseLost) || !time.Now().Add(margin).Before(lease.Lease.ExpiresAt) {
					s.markLost(fmt.Errorf("remote %s lease on %s was lost: %w", s.operation, lease.ServerName, err))
				}
				return
			}
			if renewed == nil || renewed.ID != lease.Lease.ID {
				err := fmt.Errorf("renewal returned a different holder")
				s.warnf("Warning: failed to renew remote %s lease on %s: %v\n", s.operation, lease.ServerName, err)
				s.markLost(fmt.Errorf("remote %s lease on %s was lost: %w", s.operation, lease.ServerName, err))
				return
			}
			s.mu.Lock()
			if index < len(s.leases) && s.leases[index].Lease != nil && s.leases[index].Lease.ID == renewed.ID {
				s.leases[index].Lease = renewed
			}
			s.mu.Unlock()
		}(index, lease, renewer)
	}
	wg.Wait()
}

func (s *RemoteLeaseSet) markLost(err error) {
	if s == nil || err == nil {
		return
	}
	s.lostOnce.Do(func() {
		s.mu.Lock()
		s.lostErr = err
		lost := s.lost
		s.mu.Unlock()
		close(lost)
	})
}

// Err returns the terminal renewal error, if continued ownership could no
// longer be proven.
func (s *RemoteLeaseSet) Err() error {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lostErr
}

// BindContext returns a child canceled when lease ownership is lost or the
// set is released. Callers must invoke the returned cancel function.
func (s *RemoteLeaseSet) BindContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancelCause := context.WithCancelCause(parent)
	if s == nil {
		return ctx, func() { cancelCause(context.Canceled) }
	}
	s.mu.RLock()
	lost := s.lost
	released := s.released
	err := s.lostErr
	s.mu.RUnlock()
	if err != nil {
		cancelCause(err)
		return ctx, func() { cancelCause(context.Canceled) }
	}
	go func() {
		select {
		case <-lost:
			cancelCause(s.Err())
		case <-released:
			cancelCause(context.Canceled)
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancelCause(context.Canceled) }
}

func (s *RemoteLeaseSet) warnf(format string, args ...any) {
	s.mu.RLock()
	warn := s.warn
	s.mu.RUnlock()
	if warn != nil {
		warn(fmt.Sprintf(format, args...))
	}
}

func (s *RemoteLeaseSet) stopRenewal() {
	s.mu.Lock()
	cancel := s.renewCancel
	done := s.renewDone
	s.renewCancel = nil
	s.renewDone = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Release releases held leases in reverse order. Failures route to the
// warning consumer; release is best-effort by design.
func (s *RemoteLeaseSet) Release() {
	if s == nil {
		return
	}
	s.releaseOnce.Do(func() {
		s.stopRenewal()
		close(s.released)
		s.mu.RLock()
		leases := append([]RemoteLease(nil), s.leases...)
		s.mu.RUnlock()
		for i := len(leases) - 1; i >= 0; i-- {
			lease := leases[i]
			if lease.Manager == nil || lease.Lease == nil {
				continue
			}
			if err := releaseRemoteLeaseErr(lease); err != nil {
				s.warnf("Warning: failed to release remote %s lease on %s: %v\n", s.operation, lease.ServerName, err)
			}
		}
	})
}

// Summary lists the held leases as "server:leaseID" pairs.
func (s *RemoteLeaseSet) Summary() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.leases) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.leases))
	for _, lease := range s.leases {
		parts = append(parts, fmt.Sprintf("%s:%s", lease.ServerName, lease.Lease.ID))
	}
	return strings.Join(parts, ", ")
}
