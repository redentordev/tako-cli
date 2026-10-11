package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// NodeMaintenanceLeaseTTL bounds a node maintenance lease. Holders renew it
// every NodeMaintenanceLeaseRenewInterval (KeepAlive) for as long as the
// operation runs; if the release fails the lease expires on its own.
const NodeMaintenanceLeaseTTL = 15 * time.Minute

// NodeMaintenanceLeaseRenewInterval leaves two renewal attempts before the
// lease could lapse, so one attempt failing while takod restarts is harmless.
const NodeMaintenanceLeaseRenewInterval = NodeMaintenanceLeaseTTL / 3

const nodeMaintenanceLeaseEndpoint = "/v1/node-lease/maintenance"

// NodeMaintenanceLeaseRequest names the node and operation of a node
// maintenance lease. Project and Environment select the fallback environment
// lease used when the node's takod predates node maintenance leases.
type NodeMaintenanceLeaseRequest struct {
	Client      any
	Socket      string
	Server      string
	Host        string
	Project     string
	Environment string
	Operation   string
	// Status is the node's current GET /v1/status response.
	Status *takod.Status
}

// NodeMaintenanceLease is one held node maintenance lease, or the
// environment lease that stands in for it on an older agent.
type NodeMaintenanceLease struct {
	Server string
	// Fallback reports that only the selected environment's lease is held.
	Fallback bool
	// Warning explains the reduced fencing of a fallback lease.
	Warning string

	client          any
	socket          string
	id              string
	operation       string
	who             string
	mu              sync.Mutex
	lost            error
	fallbackManager *remotestate.StateManager
	fallbackLease   *remotestate.LeaseInfo
}

// AcquireNodeMaintenanceLease takes the node-wide maintenance lease on one
// node, failing fast with a LockedError (exit 3) while another operation of
// any project holds an environment lease there. An agent without
// lease.node-maintenance-v1 gets the selected environment's lease instead,
// with a warning that other environments and projects are not fenced: failing
// would make the upgrade that adds the capability impossible.
func AcquireNodeMaintenanceLease(ctx context.Context, req NodeMaintenanceLeaseRequest) (*NodeMaintenanceLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req.Client == nil {
		return nil, fmt.Errorf("node maintenance lease on %s requires a takod client", req.Server)
	}
	if req.Status != nil && statusHasCapability(req.Status, takod.CapabilityNodeMaintenanceLeaseV1) {
		return acquireNodeMaintenanceLeaseV1(ctx, req)
	}
	manager := remotestate.NewStateManagerWithSocket(req.Client, req.Project, req.Environment, req.Host, req.Socket)
	lease, err := manager.AcquireLeaseContext(ctx, req.Operation, req.Environment, NodeMaintenanceLeaseTTL)
	if err != nil {
		return nil, &LockedError{Operation: req.Operation, Err: fmt.Errorf("cannot acquire %s lease for %s/%s on %s: %w", req.Operation, req.Project, req.Environment, req.Server, err)}
	}
	return &NodeMaintenanceLease{
		Server:          req.Server,
		Fallback:        true,
		Warning:         fmt.Sprintf("takod on %s does not support node maintenance leases; holding only the %s/%s environment lease, so other environments and projects on this node are not fenced during %s", req.Server, req.Project, req.Environment, req.Operation),
		client:          req.Client,
		socket:          req.Socket,
		fallbackManager: manager,
		fallbackLease:   lease,
	}, nil
}

func acquireNodeMaintenanceLeaseV1(ctx context.Context, req NodeMaintenanceLeaseRequest) (*NodeMaintenanceLease, error) {
	id, err := newNodeMaintenanceLeaseID()
	if err != nil {
		return nil, err
	}
	who := nodeMaintenancePrincipal()
	output, err := takodclient.RequestJSONWithContext(ctx, req.Client, req.Socket, "POST", nodeMaintenanceLeaseEndpoint, takod.NodeMaintenanceLeaseRequest{
		ID:         id,
		Operation:  req.Operation,
		Who:        who,
		PID:        os.Getpid(),
		TTLSeconds: int64(NodeMaintenanceLeaseTTL / time.Second),
	})
	if err != nil {
		return nil, &ConnectivityError{Server: req.Server, Err: fmt.Errorf("acquire node maintenance lease on %s: %w", req.Server, err)}
	}
	var response takod.NodeMaintenanceLeaseResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("parse node maintenance lease response from %s: %w", req.Server, err)
	}
	if !response.Acquired {
		return nil, nodeMaintenanceLockedError(req, response)
	}
	return &NodeMaintenanceLease{Server: req.Server, client: req.Client, socket: req.Socket, id: id, operation: req.Operation, who: who}, nil
}

func nodeMaintenanceLockedError(req NodeMaintenanceLeaseRequest, response takod.NodeMaintenanceLeaseResponse) error {
	if len(response.Holders) > 0 {
		holders := make([]string, 0, len(response.Holders))
		for _, holder := range response.Holders {
			holders = append(holders, fmt.Sprintf("%s/%s (%s by %s, expires in %s)", holder.ProjectName, holder.Environment, holder.Operation, holder.Who, time.Until(holder.ExpiresAt).Round(time.Second)))
		}
		return &LockedError{
			Operation: req.Operation,
			Holder:    strings.Join(holders, ", "),
			Err:       fmt.Errorf("cannot start %s on %s: environment operations hold leases on the node: %s; retry when they finish", req.Operation, req.Server, strings.Join(holders, ", ")),
		}
	}
	holder := "another operation"
	if response.Lease != nil {
		holder = fmt.Sprintf("%s by %s, expires in %s", response.Lease.Operation, response.Lease.Who, time.Until(response.Lease.ExpiresAt).Round(time.Second))
	}
	return &LockedError{
		Operation: req.Operation,
		Holder:    holder,
		Err:       fmt.Errorf("cannot start %s on %s: node maintenance lease is held (%s)", req.Operation, req.Server, holder),
	}
}

// Release removes the lease. Call it against the node's current agent, which
// after an upgrade is the restarted one; the v1 lease file survives restarts.
func (l *NodeMaintenanceLease) Release(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if l.Fallback {
		l.mu.Lock()
		lease := l.fallbackLease
		l.mu.Unlock()
		return l.fallbackManager.ReleaseLeaseContext(ctx, lease)
	}
	_, err := takodclient.RequestJSONWithContext(ctx, l.client, l.socket, "DELETE", nodeMaintenanceLeaseEndpoint, takod.NodeMaintenanceLeaseRequest{ID: l.id})
	if err != nil {
		return fmt.Errorf("release node maintenance lease on %s (it expires within %s): %w", l.Server, NodeMaintenanceLeaseTTL, err)
	}
	return nil
}

// Renew extends the lease to NodeMaintenanceLeaseTTL from now by acquiring it
// again under the same ID (takod keeps the creation time). It returns a
// LockedError, and Err reports it from then on, when the lease was lost: it
// lapsed and another operation took the node. Other errors (for example the
// agent restarting) are transient and leave the lease as it was.
func (l *NodeMaintenanceLease) Renew(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if l.Fallback {
		if l.fallbackManager == nil {
			return nil
		}
		l.mu.Lock()
		current := l.fallbackLease
		l.mu.Unlock()
		renewed, err := l.fallbackManager.RenewLeaseContext(ctx, current, NodeMaintenanceLeaseTTL)
		if err != nil {
			if errors.Is(err, remotestate.ErrLeaseLost) {
				return l.markLost(&LockedError{Operation: current.Operation, Err: fmt.Errorf("lost the %s lease on %s: %w", current.Operation, l.Server, err)})
			}
			return fmt.Errorf("renew %s lease on %s: %w", current.Operation, l.Server, err)
		}
		l.mu.Lock()
		l.fallbackLease = renewed
		l.mu.Unlock()
		return nil
	}
	output, err := takodclient.RequestJSONWithContext(ctx, l.client, l.socket, "POST", nodeMaintenanceLeaseEndpoint, takod.NodeMaintenanceLeaseRequest{
		ID:         l.id,
		Operation:  l.operation,
		Who:        l.who,
		PID:        os.Getpid(),
		TTLSeconds: int64(NodeMaintenanceLeaseTTL / time.Second),
	})
	if err != nil {
		return fmt.Errorf("renew node maintenance lease on %s: %w", l.Server, err)
	}
	var response takod.NodeMaintenanceLeaseResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return fmt.Errorf("parse node maintenance lease renewal from %s: %w", l.Server, err)
	}
	if !response.Acquired {
		refusal := nodeMaintenanceLockedError(NodeMaintenanceLeaseRequest{Server: l.Server, Operation: l.operation}, response)
		holder := ""
		var locked *LockedError
		if errors.As(refusal, &locked) {
			holder = locked.Holder
		}
		return l.markLost(&LockedError{Operation: l.operation, Holder: holder, Err: fmt.Errorf("lost the node maintenance lease on %s: %w", l.Server, refusal)})
	}
	return nil
}

func (l *NodeMaintenanceLease) markLost(err error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost == nil {
		l.lost = err
	}
	return err
}

// Err reports a lost lease (nil while it is held). Holders check it before
// each mutation step and stop when it is set.
func (l *NodeMaintenanceLease) Err() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lost
}

// KeepAlive renews the lease every interval until stop is called, so a long
// setup or upgrade cannot outlive it. Renewal errors go to onError; after a
// lost lease renewal stops and Err reports it. stop waits for an in-flight
// renewal and must be called before Release.
func (l *NodeMaintenanceLease) KeepAlive(interval time.Duration, onError func(error)) (stop func()) {
	if l == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = NodeMaintenanceLeaseRenewInterval
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), interval)
				err := l.Renew(ctx)
				cancel()
				if err != nil && onError != nil {
					onError(err)
				}
				if l.Err() != nil {
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
		})
	}
}

func statusHasCapability(status *takod.Status, capability string) bool {
	for _, candidate := range status.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func newNodeMaintenanceLeaseID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("create node maintenance lease identity: %w", err)
	}
	return "maint-" + hex.EncodeToString(value), nil
}

func nodeMaintenancePrincipal() string {
	who := remotestate.GetCurrentUser()
	if hostname, _ := os.Hostname(); hostname != "" {
		return who + "@" + hostname
	}
	return who
}
