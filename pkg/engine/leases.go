package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
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
	warn             func(message string)
	renewCancel      context.CancelFunc
	renewDone        chan struct{}
	releaseOnce      sync.Once
	lostOnce         sync.Once
	lostErr          error
	lost             chan struct{}
	released         chan struct{}
	fence            *nodeidentity.OperationFence
	fenceTargets     []operationFenceTarget
	controllerClient *takodclient.AgentClient
}

type operationFenceTarget struct {
	ServerName string
	Client     *takodclient.AgentClient
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
	if controllerName, enrolled, controllerErr := controllerAuthorityServer(cfg, serverNames); controllerErr != nil {
		return nil, controllerErr
	} else if enrolled {
		return acquireControllerOperationLeaseSet(ctx, pool, factory, cfg, envName, controllerName, serverNames, operation)
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

func controllerAuthorityServer(cfg *config.Config, targetNames []string) (string, bool, error) {
	if cfg == nil {
		return "", false, invalidRequestf("controller authority requires a loaded config")
	}
	enrolled := false
	controller := ""
	clusterID := ""
	for name, server := range cfg.Servers {
		if server.ClusterID == "" && server.NodeID == "" {
			continue
		}
		enrolled = true
		if clusterID == "" {
			clusterID = server.ClusterID
		} else if server.ClusterID != clusterID {
			return "", true, invalidRequestf("enrolled servers span multiple cluster identities")
		}
		if server.HasPlatformRole(nodeidentity.RoleControlPlane) {
			if controller != "" && controller != name {
				return "", true, invalidRequestf("enrolled configuration has multiple control-plane authorities")
			}
			controller = name
		}
	}
	if !enrolled {
		return "", false, nil
	}
	if controller == "" {
		return "", true, invalidRequestf("enrolled configuration has no controller authority")
	}
	for _, name := range targetNames {
		server, ok := cfg.Servers[name]
		if !ok || server.ClusterID == "" || server.NodeID == "" || server.ClusterID != clusterID {
			return "", true, invalidRequestf("operation target %s is not a member of the authoritative cluster", name)
		}
	}
	return controller, true, nil
}

func acquireControllerOperationLeaseSet(ctx context.Context, pool *ssh.Pool, factory *nodeclient.Factory, cfg *config.Config, envName, controllerName string, targetNames []string, operation string) (*RemoteLeaseSet, error) {
	controllerServer := cfg.Servers[controllerName]
	controllerClient, _, err := factory.Client(ctx, controllerName)
	if err != nil {
		return nil, &ConnectivityError{Server: controllerName, Err: fmt.Errorf("connect controller operation authority: %w", err)}
	}
	status, err := controllerClient.Status(ctx)
	if err != nil {
		return nil, &ConnectivityError{Server: controllerName, Err: fmt.Errorf("read controller operation authority: %w", err)}
	}
	if !agentHasCapability(status, takod.CapabilityOperationFence) || !agentHasCapability(status, takod.CapabilityNodeMembershipV1) {
		return nil, invalidRequestf("controller %s does not support authoritative operation fencing; upgrade it before mutating enrolled nodes", controllerName)
	}
	authorityTargets := append([]string(nil), targetNames...)
	if !containsString(authorityTargets, controllerName) {
		authorityTargets = append(authorityTargets, controllerName)
	}
	sort.Strings(authorityTargets)
	targetNodeIDs := make([]string, 0, len(authorityTargets))
	for _, name := range authorityTargets {
		targetNodeIDs = append(targetNodeIDs, cfg.Servers[name].NodeID)
	}
	sort.Strings(targetNodeIDs)
	manager := remotestate.NewStateManagerWithSocket(controllerClient, cfg.Project.Name, envName, controllerServer.Host, TakodSocketFromConfig(cfg))
	lease, err := manager.AcquireControllerLeaseContext(ctx, operation, envName, remotestate.DefaultLeaseTTL, targetNodeIDs)
	if err != nil {
		return nil, &LockedError{Operation: operation, Err: fmt.Errorf("cannot acquire controller %s authority on %s: %w", operation, controllerName, err)}
	}
	set := NewRemoteLeaseSet(operation, []RemoteLease{{ServerName: controllerName, Manager: manager, Lease: lease}})
	factory.SetOperationFenceSource(set)
	if pool != nil {
		pool.SetOperationFenceSource(set)
	}
	set.controllerClient = controllerClient
	if lease.Fence == nil {
		set.Release()
		return nil, fmt.Errorf("controller %s acquired a lease without signed fencing authority", controllerName)
	}
	set.fence = cloneOperationFence(lease.Fence)
	// Activate the controller copy first so it can authenticate recovery of an
	// abandoned allocation proposal before any lower ordinary inventory is
	// published to an edge that may already hold the proposal generation.
	if _, err := controllerClient.RequestJSON(ctx, "POST", "/v1/fence", takod.FenceRequest{Fence: *lease.Fence, HolderToken: lease.HolderToken}); err != nil {
		set.Release()
		return nil, &ConnectivityError{Server: controllerName, Err: fmt.Errorf("activate controller recovery fence: %w", err)}
	}
	set.fenceTargets = append(set.fenceTargets, operationFenceTarget{ServerName: controllerName, Client: controllerClient})
	if err := recoverPendingAllocationAuthority(ctx, factory, cfg, envName, controllerName, controllerClient); err != nil {
		set.Release()
		return nil, err
	}
	output, err := controllerClient.RequestJSON(ctx, "GET", "/v1/platform/inventory", nil)
	if err != nil {
		set.Release()
		return nil, &ConnectivityError{Server: controllerName, Err: fmt.Errorf("read signed controller inventory: %w", err)}
	}
	var snapshot nodeidentity.SignedInventorySnapshot
	if err := json.Unmarshal([]byte(output), &snapshot); err != nil {
		set.Release()
		return nil, fmt.Errorf("decode signed controller inventory: %w", err)
	}
	for _, name := range authorityTargets {
		client, _, clientErr := factory.Client(ctx, name)
		if clientErr != nil {
			set.Release()
			return nil, &ConnectivityError{Server: name, Err: fmt.Errorf("connect operation fence target: %w", clientErr)}
		}
		if _, publishErr := client.RequestJSON(ctx, "POST", "/v1/platform/inventory", snapshot); publishErr != nil {
			set.Release()
			return nil, &ConnectivityError{Server: name, Err: fmt.Errorf("publish signed controller inventory: %w", publishErr)}
		}
		if !containsFenceTarget(set.fenceTargets, name) {
			set.fenceTargets = append(set.fenceTargets, operationFenceTarget{ServerName: name, Client: client})
		}
	}
	if err := set.publishFence(ctx, lease.Fence); err != nil {
		set.Release()
		return nil, err
	}
	if err := set.updateControllerPhase(ctx, "targets-fenced"); err != nil {
		set.Release()
		return nil, err
	}
	if err := set.updateControllerPhase(ctx, "mutating"); err != nil {
		set.Release()
		return nil, err
	}
	set.startRenewal(remotestate.DefaultLeaseTTL, remotestate.DefaultLeaseTTL/4)
	if err := set.Err(); err != nil {
		set.Release()
		return nil, err
	}
	return set, nil
}

func recoverPendingAllocationAuthority(ctx context.Context, factory *nodeclient.Factory, cfg *config.Config, environment, controllerName string, controller *takodclient.AgentClient) error {
	output, err := controller.RequestJSON(ctx, "POST", "/v1/platform/allocations/authorize", takod.AllocationAuthorizationRequest{
		Project: cfg.Project.Name, Environment: environment, Phase: "recover",
	})
	if err != nil {
		return &ConnectivityError{Server: controllerName, Err: fmt.Errorf("recover pending allocation authority: %w", err)}
	}
	var recovery takod.AllocationAuthorizationResponse
	if err := json.Unmarshal([]byte(output), &recovery); err != nil {
		return fmt.Errorf("decode pending allocation recovery: %w", err)
	}
	if !recovery.Recovered {
		return nil
	}
	for _, nodeID := range recovery.RecoveryTargetNodeIDs {
		serverName := ""
		for name, server := range cfg.Servers {
			if strings.EqualFold(strings.TrimSpace(server.NodeID), strings.TrimSpace(nodeID)) {
				serverName = name
				break
			}
		}
		if serverName == "" {
			return fmt.Errorf("allocation recovery target node %s is absent from configured cluster membership", nodeID)
		}
		client, _, err := factory.Client(ctx, serverName)
		if err != nil {
			return &ConnectivityError{Server: serverName, Err: fmt.Errorf("connect allocation recovery target: %w", err)}
		}
		if _, err := client.RequestJSON(ctx, "POST", "/v1/platform/inventory", recovery.Snapshot); err != nil {
			return &ConnectivityError{Server: serverName, Err: fmt.Errorf("publish monotonic allocation recovery: %w", err)}
		}
		if _, err := controller.RequestJSON(ctx, "POST", "/v1/platform/allocations/authorize", takod.AllocationAuthorizationRequest{
			Project: cfg.Project.Name, Environment: environment, Phase: "recovery-ack", ProposalID: recovery.ProposalID, TargetNodeID: nodeID,
		}); err != nil {
			return &ConnectivityError{Server: controllerName, Err: fmt.Errorf("persist allocation recovery acknowledgement for %s: %w", serverName, err)}
		}
	}
	if _, err := controller.RequestJSON(ctx, "POST", "/v1/platform/allocations/authorize", takod.AllocationAuthorizationRequest{
		Project: cfg.Project.Name, Environment: environment, Phase: "finalize-recovery", ProposalID: recovery.ProposalID,
	}); err != nil {
		return &ConnectivityError{Server: controllerName, Err: fmt.Errorf("finalize allocation recovery: %w", err)}
	}
	return nil
}

func containsFenceTarget(targets []operationFenceTarget, name string) bool {
	for _, target := range targets {
		if target.ServerName == name {
			return true
		}
	}
	return false
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func agentHasCapability(status *takodclient.AgentStatus, capability string) bool {
	if status == nil {
		return false
	}
	for _, candidate := range status.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func cloneOperationFence(fence *nodeidentity.OperationFence) *nodeidentity.OperationFence {
	if fence == nil {
		return nil
	}
	copy := *fence
	copy.TargetNodeIDs = append([]string(nil), fence.TargetNodeIDs...)
	return &copy
}

func (s *RemoteLeaseSet) publishFence(ctx context.Context, fence *nodeidentity.OperationFence) error {
	if fence == nil {
		return fmt.Errorf("controller operation fence is missing")
	}
	s.mu.RLock()
	targets := append([]operationFenceTarget(nil), s.fenceTargets...)
	holderToken := s.operationHolderTokenLocked()
	s.mu.RUnlock()
	for _, target := range targets {
		if target.Client == nil {
			return fmt.Errorf("operation fence target %s has no runtime client", target.ServerName)
		}
		if _, err := target.Client.RequestJSON(ctx, "POST", "/v1/fence", takod.FenceRequest{Fence: *fence, HolderToken: holderToken}); err != nil {
			return &ConnectivityError{Server: target.ServerName, Err: fmt.Errorf("activate controller operation fence: %w", err)}
		}
	}
	return nil
}

func (s *RemoteLeaseSet) updateControllerPhase(ctx context.Context, phase string) error {
	s.mu.RLock()
	client := s.controllerClient
	fence := cloneOperationFence(s.fence)
	holderToken := s.operationHolderTokenLocked()
	s.mu.RUnlock()
	if client == nil || fence == nil {
		return nil
	}
	_, err := client.RequestJSON(ctx, "POST", "/v1/fence", takod.FenceRequest{Fence: *fence, Phase: phase, HolderToken: holderToken})
	if err != nil {
		return fmt.Errorf("persist controller operation phase %s: %w", phase, err)
	}
	return nil
}

func (s *RemoteLeaseSet) revokeFenceTargets(fence *nodeidentity.OperationFence) error {
	if fence == nil {
		return nil
	}
	s.mu.RLock()
	targets := append([]operationFenceTarget(nil), s.fenceTargets...)
	holderToken := s.operationHolderTokenLocked()
	s.mu.RUnlock()
	var failures []string
	for _, target := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), remoteLeaseRenewRequestTimeout)
		_, err := target.Client.RequestJSON(ctx, "DELETE", "/v1/fence", takod.FenceRequest{Fence: *fence, HolderToken: holderToken})
		cancel()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", target.ServerName, err))
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("revoke controller operation fence: %s", strings.Join(failures, "; "))
	}
	return nil
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
			if renewed.Fence != nil {
				if err := s.publishFence(renewCtx, renewed.Fence); err != nil {
					s.warnf("Warning: failed to renew controller %s fence on targets: %v\n", s.operation, err)
					s.markLost(fmt.Errorf("controller %s target fence renewal failed: %w", s.operation, err))
					return
				}
			}
			s.mu.Lock()
			if index < len(s.leases) && s.leases[index].Lease != nil && s.leases[index].Lease.ID == renewed.ID {
				s.leases[index].Lease = renewed
				if renewed.Fence != nil {
					s.fence = cloneOperationFence(renewed.Fence)
				}
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
	ctx = takodclient.WithOperationFenceSource(ctx, s)
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

// OperationFence returns the latest controller-renewed fence for request
// headers. It satisfies takodclient.OperationFenceSource.
func (s *RemoteLeaseSet) OperationFence() *nodeidentity.OperationFence {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneOperationFence(s.fence)
}

func (s *RemoteLeaseSet) OperationHolderToken() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.operationHolderTokenLocked()
}

func (s *RemoteLeaseSet) operationHolderTokenLocked() string {
	for _, lease := range s.leases {
		if lease.Lease != nil && lease.Lease.HolderToken != "" {
			return lease.Lease.HolderToken
		}
	}
	return ""
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
		fence := cloneOperationFence(s.fence)
		s.mu.RUnlock()
		if err := s.revokeFenceTargets(fence); err != nil {
			s.warnf("Warning: %v; controller lease retained until expiry to prevent overlapping writers\n", err)
			return
		}
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
