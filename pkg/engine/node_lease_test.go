package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod"
)

// fakeLeaseNode is one node's lease state, shared by every agent instance
// that serves it (an upgrade restarts the agent but keeps its data dir).
type fakeLeaseNode struct {
	mu          sync.Mutex
	maintenance *takod.NodeMaintenanceLease
	environment map[string]takod.LeaseInfo
	requests    []string
}

func newFakeLeaseNode() *fakeLeaseNode {
	return &fakeLeaseNode{environment: map[string]takod.LeaseInfo{}}
}

// fakeLeaseAgent is one running takod on a fakeLeaseNode.
type fakeLeaseAgent struct {
	node    *fakeLeaseNode
	capable bool
}

func (a *fakeLeaseAgent) status() *takod.Status {
	status := &takod.Status{Runtime: "takod"}
	if a.capable {
		status.Capabilities = []string{takod.CapabilityNodeMaintenanceLeaseV1}
	}
	return status
}

func (a *fakeLeaseAgent) RequestJSON(ctx context.Context, method, endpoint string, value any) (string, error) {
	return a.RequestJSONWithTimeout(ctx, method, endpoint, value, time.Second)
}

func (a *fakeLeaseAgent) RequestJSONWithTimeout(_ context.Context, method, endpoint string, value any, _ time.Duration) (string, error) {
	node := a.node
	node.mu.Lock()
	defer node.mu.Unlock()
	node.requests = append(node.requests, method+" "+endpoint)
	data, _ := json.Marshal(value)
	reply := func(v any) (string, error) {
		out, err := json.Marshal(v)
		return string(out), err
	}
	switch {
	case endpoint == "/v1/node-lease/maintenance" && a.capable:
		var request takod.NodeMaintenanceLeaseRequest
		_ = json.Unmarshal(data, &request)
		if method == "DELETE" {
			if node.maintenance == nil || node.maintenance.ID != request.ID {
				return "", fmt.Errorf("not the holder")
			}
			node.maintenance = nil
			return reply(takod.NodeMaintenanceLeaseResponse{})
		}
		if node.maintenance != nil && node.maintenance.ID != request.ID {
			return reply(takod.NodeMaintenanceLeaseResponse{Found: true, Lease: node.maintenance, Message: "node maintenance lease is held"})
		}
		if len(node.environment) > 0 {
			var holders []takod.LeaseInfo
			for _, lease := range node.environment {
				holders = append(holders, lease)
			}
			return reply(takod.NodeMaintenanceLeaseResponse{Holders: holders, Message: "environment leases are held"})
		}
		node.maintenance = &takod.NodeMaintenanceLease{Version: 1, ID: request.ID, Operation: request.Operation, Who: request.Who, ExpiresAt: time.Now().Add(time.Duration(request.TTLSeconds) * time.Second)}
		return reply(takod.NodeMaintenanceLeaseResponse{Acquired: true, Found: true, Lease: node.maintenance})
	case endpoint == "/v1/lease":
		var request takod.LeaseRequest
		_ = json.Unmarshal(data, &request)
		key := request.Project + "/" + request.Environment
		if method == "DELETE" {
			delete(node.environment, key)
			return reply(takod.LeaseResponse{})
		}
		if node.maintenance != nil && a.capable {
			return reply(takod.LeaseResponse{Found: true, Lease: &takod.LeaseInfo{Operation: "node maintenance (" + node.maintenance.Operation + ")", Who: node.maintenance.Who, ExpiresAt: node.maintenance.ExpiresAt}})
		}
		if held, ok := node.environment[key]; ok {
			if request.Renew && held.ID == request.ID {
				held.ExpiresAt = time.Now().Add(time.Duration(request.TTLSeconds) * time.Second)
				node.environment[key] = held
				return reply(takod.LeaseResponse{Acquired: true, Found: true, Lease: &held})
			}
			return reply(takod.LeaseResponse{Found: true, Lease: &held})
		}
		lease := takod.LeaseInfo{ID: request.ID, ProjectName: request.Project, Environment: request.Environment, Operation: request.Operation, Who: request.Who, ExpiresAt: time.Now().Add(time.Duration(request.TTLSeconds) * time.Second)}
		node.environment[key] = lease
		return reply(takod.LeaseResponse{Acquired: true, Found: true, Lease: &lease})
	}
	return "", fmt.Errorf("unexpected %s %s", method, endpoint)
}

func maintenanceLeaseRequest(agent *fakeLeaseAgent, operation string) NodeMaintenanceLeaseRequest {
	return NodeMaintenanceLeaseRequest{
		Client: agent, Server: "s1", Host: "203.0.113.5", Project: "app", Environment: "preview",
		Operation: operation, Status: agent.status(),
	}
}

func TestNodeMaintenanceLeaseFencesEnvironmentLeasesAcrossRestart(t *testing.T) {
	node := newFakeLeaseNode()
	before := &fakeLeaseAgent{node: node, capable: true}
	lease, err := AcquireNodeMaintenanceLease(context.Background(), maintenanceLeaseRequest(before, "upgrade-servers"))
	if err != nil {
		t.Fatal(err)
	}
	if lease.Fallback || lease.Warning != "" {
		t.Fatalf("capable agent used the fallback: %+v", lease)
	}

	// The upgrade restarts takod; the new agent still refuses deploys.
	after := &fakeLeaseAgent{node: node, capable: true}
	lease.client = after
	deploy := NodeMaintenanceLeaseRequest{Client: after, Server: "s1", Project: "app", Environment: "production", Operation: "deploy", Status: &takod.Status{Runtime: "takod"}}
	if _, err := AcquireNodeMaintenanceLease(context.Background(), deploy); err == nil || !strings.Contains(err.Error(), "node maintenance (upgrade-servers)") {
		t.Fatalf("deploy during maintenance err = %v", err)
	} else if Classify(err) != ClassLocked {
		t.Fatalf("deploy during maintenance class = %v, want locked", Classify(err))
	}

	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if node.maintenance != nil {
		t.Fatal("maintenance lease still held after release on the restarted agent")
	}
}

func TestNodeMaintenanceLeaseFailsFastOnLiveEnvironmentLease(t *testing.T) {
	node := newFakeLeaseNode()
	node.environment["app/preview"] = takod.LeaseInfo{ProjectName: "app", Environment: "preview", Operation: "deploy", Who: "ci@runner", ExpiresAt: time.Now().Add(10 * time.Minute)}
	agent := &fakeLeaseAgent{node: node, capable: true}
	_, err := AcquireNodeMaintenanceLease(context.Background(), maintenanceLeaseRequest(agent, "upgrade-servers"))
	if err == nil {
		t.Fatal("maintenance lease granted during a deploy")
	}
	if Classify(err) != ClassLocked {
		t.Fatalf("class = %v, want locked (exit 3)", Classify(err))
	}
	if !strings.Contains(err.Error(), "app/preview (deploy by ci@runner") {
		t.Fatalf("error does not name the holder: %v", err)
	}
	if node.maintenance != nil {
		t.Fatal("refused maintenance lease was recorded")
	}
}

func TestNodeMaintenanceLeaseFallsBackToEnvironmentLeaseOnOlderAgent(t *testing.T) {
	node := newFakeLeaseNode()
	agent := &fakeLeaseAgent{node: node, capable: false}
	lease, err := AcquireNodeMaintenanceLease(context.Background(), maintenanceLeaseRequest(agent, "upgrade-servers"))
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Fallback || !strings.Contains(lease.Warning, "other environments and projects on this node are not fenced") {
		t.Fatalf("fallback lease = %+v", lease)
	}
	if _, held := node.environment["app/preview"]; !held {
		t.Fatalf("fallback did not take the selected environment's lease: %v", node.environment)
	}
	for _, request := range node.requests {
		if strings.Contains(request, "node-lease") {
			t.Fatalf("older agent received a node lease request: %v", node.requests)
		}
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(node.environment) != 0 {
		t.Fatalf("fallback lease not released: %v", node.environment)
	}

	// The fallback still fails fast when the environment is busy.
	node.environment["app/preview"] = takod.LeaseInfo{ProjectName: "app", Environment: "preview", Operation: "deploy", Who: "ci", ExpiresAt: time.Now().Add(time.Minute)}
	if _, err := AcquireNodeMaintenanceLease(context.Background(), maintenanceLeaseRequest(agent, "upgrade-servers")); Classify(err) != ClassLocked {
		t.Fatalf("busy fallback err = %v", err)
	}
}

func (n *fakeLeaseNode) countRequests(match string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, request := range n.requests {
		if request == match {
			count++
		}
	}
	return count
}

// Review finding 3: setup and upgrade can outlive the 15-minute TTL, so the
// holder renews the lease under the same ID, also against the restarted
// agent.
func TestNodeMaintenanceLeaseRenewExtendsAcrossRestart(t *testing.T) {
	node := newFakeLeaseNode()
	lease, err := AcquireNodeMaintenanceLease(context.Background(), maintenanceLeaseRequest(&fakeLeaseAgent{node: node, capable: true}, "setup"))
	if err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	node.maintenance.ExpiresAt = time.Now().Add(time.Minute)
	node.mu.Unlock()
	lease.client = &fakeLeaseAgent{node: node, capable: true}
	if err := lease.Renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	expires := node.maintenance.ExpiresAt
	operation := node.maintenance.Operation
	node.mu.Unlock()
	if time.Until(expires) < NodeMaintenanceLeaseTTL-time.Minute || operation != "setup" {
		t.Fatalf("renewed lease expires in %s (operation %q), want about %s", time.Until(expires), operation, NodeMaintenanceLeaseTTL)
	}
	if lease.Err() != nil {
		t.Fatalf("held lease reports %v", lease.Err())
	}
}

func TestNodeMaintenanceLeaseRenewReportsLostLease(t *testing.T) {
	node := newFakeLeaseNode()
	lease, err := AcquireNodeMaintenanceLease(context.Background(), maintenanceLeaseRequest(&fakeLeaseAgent{node: node, capable: true}, "setup"))
	if err != nil {
		t.Fatal(err)
	}
	// The lease lapsed and another maintenance operation took the node.
	node.mu.Lock()
	node.maintenance = &takod.NodeMaintenanceLease{Version: 1, ID: "maint-other", Operation: "upgrade-servers", Who: "ops", ExpiresAt: time.Now().Add(time.Hour)}
	node.mu.Unlock()
	err = lease.Renew(context.Background())
	if Classify(err) != ClassLocked || lease.Err() == nil {
		t.Fatalf("renew err = %v, Err = %v; want a lost (locked) lease", err, lease.Err())
	}
}

func TestNodeMaintenanceLeaseKeepAliveRenewsUntilStopped(t *testing.T) {
	node := newFakeLeaseNode()
	lease, err := AcquireNodeMaintenanceLease(context.Background(), maintenanceLeaseRequest(&fakeLeaseAgent{node: node, capable: true}, "setup"))
	if err != nil {
		t.Fatal(err)
	}
	stop := lease.KeepAlive(5*time.Millisecond, func(err error) { t.Errorf("renewal error: %v", err) })
	deadline := time.Now().Add(5 * time.Second)
	for node.countRequests("POST /v1/node-lease/maintenance") < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	renewals := node.countRequests("POST /v1/node-lease/maintenance")
	if renewals < 4 {
		t.Fatalf("keep-alive sent %d acquire/renew requests, want at least 4", renewals)
	}
	time.Sleep(30 * time.Millisecond)
	if after := node.countRequests("POST /v1/node-lease/maintenance"); after != renewals {
		t.Fatalf("renewals continued after stop: %d -> %d", renewals, after)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNodeMaintenanceLeaseFallbackRenews(t *testing.T) {
	node := newFakeLeaseNode()
	lease, err := AcquireNodeMaintenanceLease(context.Background(), maintenanceLeaseRequest(&fakeLeaseAgent{node: node, capable: false}, "setup"))
	if err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	held := node.environment["app/preview"]
	held.ExpiresAt = time.Now().Add(time.Minute)
	node.environment["app/preview"] = held
	node.mu.Unlock()
	if err := lease.Renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	expires := node.environment["app/preview"].ExpiresAt
	node.mu.Unlock()
	if time.Until(expires) < NodeMaintenanceLeaseTTL-time.Minute {
		t.Fatalf("fallback lease expires in %s after renewal", time.Until(expires))
	}
}
