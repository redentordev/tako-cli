package cmd

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
)

func TestAcquireStateRepairLeasesWithRunsConcurrently(t *testing.T) {
	nodes := []stateRepairNode{
		{name: "node-a"},
		{name: "node-b"},
		{name: "node-c"},
	}
	started := make(chan string, len(nodes))
	release := make(chan struct{})

	leasesDone := make(chan []stateRepairLease, 1)
	errDone := make(chan error, 1)
	go func() {
		leases, err := acquireStateRepairLeasesWith(nodes, func(node stateRepairNode) (stateRepairLease, error) {
			started <- node.name
			<-release
			return stateRepairLease{
				serverName: node.name,
				manager:    &recordingStateRepairManager{},
				lease:      &remotestate.LeaseInfo{ID: "lease-" + node.name},
			}, nil
		})
		leasesDone <- leases
		errDone <- err
	}()

	waitForStateRepairLeaseStarts(t, started, len(nodes))
	close(release)

	if err := <-errDone; err != nil {
		t.Fatalf("acquireStateRepairLeasesWith returned error: %v", err)
	}
	leases := <-leasesDone
	if got, want := stateRepairLeaseSummary(leases), "node-a:lease-node-a, node-b:lease-node-b, node-c:lease-node-c"; got != want {
		t.Fatalf("stateRepairLeaseSummary = %q, want %q", got, want)
	}
}

func TestAcquireStateRepairLeasesWithReleasesOnFailure(t *testing.T) {
	nodes := []stateRepairNode{
		{name: "node-a"},
		{name: "node-b"},
		{name: "node-c"},
	}
	manager := &recordingStateRepairManager{}

	_, err := acquireStateRepairLeasesWith(nodes, func(node stateRepairNode) (stateRepairLease, error) {
		if node.name == "node-c" {
			return stateRepairLease{}, fmt.Errorf("node-c failed")
		}
		return stateRepairLease{
			serverName: node.name,
			manager:    manager,
			lease:      &remotestate.LeaseInfo{ID: "lease-" + node.name},
		}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "node-c failed") {
		t.Fatalf("expected node-c failure, got %v", err)
	}

	released := manager.Released()
	if got, want := strings.Join(released, ","), "lease-node-b,lease-node-a"; got != want {
		t.Fatalf("released leases = %q, want %q", got, want)
	}
}

func waitForStateRepairLeaseStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for state repair lease fanout; saw %v", seen)
		}
	}
}

type recordingStateRepairManager struct {
	mu       sync.Mutex
	released []string
}

func (m *recordingStateRepairManager) LoadHistory() (*remotestate.DeploymentHistory, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *recordingStateRepairManager) SaveHistory(*remotestate.DeploymentHistory) error {
	return fmt.Errorf("not implemented")
}

func (m *recordingStateRepairManager) AcquireLease(string, string, time.Duration) (*remotestate.LeaseInfo, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *recordingStateRepairManager) ReleaseLease(lease *remotestate.LeaseInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.released = append(m.released, lease.ID)
	return nil
}

func (m *recordingStateRepairManager) Released() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.released...)
}
