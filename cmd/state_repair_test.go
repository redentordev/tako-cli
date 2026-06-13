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

func TestWriteStateRepairDocumentsWritesHistoryConcurrently(t *testing.T) {
	nodes := []stateRepairNode{
		{name: "node-a"},
		{name: "node-b"},
		{name: "node-c"},
	}
	started := make(chan string, len(nodes))
	release := make(chan struct{})
	for i := range nodes {
		nodes[i].manager = &blockingHistoryRepairManager{
			nodeName: nodes[i].name,
			started:  started,
			release:  release,
		}
	}

	done := make(chan struct {
		history int
		err     error
	}, 1)
	go func() {
		historyWritten, _, _, _, err := writeStateRepairDocuments(
			nodes,
			stateHistoryCandidate{history: testRepairHistory()},
			true,
			stateDesiredCandidate{},
			false,
			stateActualCandidate{},
			false,
			nil,
		)
		done <- struct {
			history int
			err     error
		}{history: historyWritten, err: err}
	}()

	waitForStateRepairLeaseStarts(t, started, len(nodes))
	close(release)

	result := <-done
	if result.err != nil {
		t.Fatalf("writeStateRepairDocuments returned error: %v", result.err)
	}
	if result.history != len(nodes) {
		t.Fatalf("historyWritten = %d, want %d", result.history, len(nodes))
	}
}

func TestWriteStateRepairDocumentsFailsWhenAllHistoryWritesFail(t *testing.T) {
	nodes := []stateRepairNode{
		{name: "node-a", manager: &blockingHistoryRepairManager{failSave: true}},
		{name: "node-b", manager: &blockingHistoryRepairManager{failSave: true}},
	}

	historyWritten, _, _, _, err := writeStateRepairDocuments(
		nodes,
		stateHistoryCandidate{history: testRepairHistory()},
		true,
		stateDesiredCandidate{},
		false,
		stateActualCandidate{},
		false,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "failed to write repaired deployment history") {
		t.Fatalf("expected all-history-write failure, got historyWritten=%d err=%v", historyWritten, err)
	}
	if historyWritten != 0 {
		t.Fatalf("historyWritten = %d, want 0", historyWritten)
	}
}

func TestWriteStateRepairDocumentsFailsWhenAnyReachableHistoryWriteFails(t *testing.T) {
	nodes := []stateRepairNode{
		{name: "node-a", manager: &blockingHistoryRepairManager{}},
		{name: "node-b", manager: &blockingHistoryRepairManager{failSave: true}},
	}

	historyWritten, _, _, _, err := writeStateRepairDocuments(
		nodes,
		stateHistoryCandidate{history: testRepairHistory()},
		true,
		stateDesiredCandidate{},
		false,
		stateActualCandidate{},
		false,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "state repair incomplete") {
		t.Fatalf("expected incomplete repair failure, got historyWritten=%d err=%v", historyWritten, err)
	}
	if historyWritten != 1 {
		t.Fatalf("historyWritten = %d, want 1", historyWritten)
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

type blockingHistoryRepairManager struct {
	nodeName string
	started  chan<- string
	release  <-chan struct{}
	failSave bool
}

func (m *blockingHistoryRepairManager) LoadHistory() (*remotestate.DeploymentHistory, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *blockingHistoryRepairManager) SaveHistory(*remotestate.DeploymentHistory) error {
	if m.started != nil {
		m.started <- m.nodeName
	}
	if m.release != nil {
		<-m.release
	}
	if m.failSave {
		return fmt.Errorf("save failed")
	}
	return nil
}

func (m *blockingHistoryRepairManager) AcquireLease(string, string, time.Duration) (*remotestate.LeaseInfo, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *blockingHistoryRepairManager) ReleaseLease(*remotestate.LeaseInfo) error {
	return nil
}

func testRepairHistory() *remotestate.DeploymentHistory {
	return &remotestate.DeploymentHistory{
		ProjectName: "demo",
		Environment: "production",
		Deployments: []*remotestate.DeploymentState{
			{ID: "deploy-1", Timestamp: time.Now().UTC(), Status: remotestate.StatusSuccess},
		},
		LastUpdated: time.Now().UTC(),
	}
}
