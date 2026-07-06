package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodstate"
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

func TestAcquireStateForgetNodeLeasesUsesForgetOperation(t *testing.T) {
	manager := &operationRecordingLeaseManager{}
	nodes := []stateRepairNode{
		{name: "node-a", manager: manager},
	}

	leases, err := acquireStateForgetNodeLeases(nodes, "production")
	if err != nil {
		t.Fatalf("acquireStateForgetNodeLeases returned error: %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}
	if got := strings.Join(manager.operations, ","); got != "state-forget-node" {
		t.Fatalf("operations = %q, want state-forget-node", got)
	}
}

func TestRunStateRepairMachineJSONSuppressesHumanOutput(t *testing.T) {
	withMachineOutput(t, outputFormatJSON, "", func() {
		tempDir := t.TempDir()
		configPath := filepath.Join(tempDir, "tako.yaml")
		if err := os.WriteFile(configPath, []byte(`project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
    password: ${TEST_SSH_PASSWORD}
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
`), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}

		t.Setenv("TEST_SSH_PASSWORD", "secret")

		oldCfgFile, oldEnvFlag, oldStateServer := cfgFile, envFlag, stateServer
		oldCollect := collectStateRepairNodesForCommand
		oldAcquire := acquireStateRepairLeasesForCommand
		oldSync := syncStateRepairHistoryToLocalForCommand
		oldEngine := cliEngineInstance
		cfgFile = configPath
		envFlag = "production"
		stateServer = ""
		cliEngineInstance = nil
		collectStateRepairNodesForCommand = func(pool *ssh.Pool, cfg *config.Config, envName string, requestedServer string) (*stateRepairInventory, error) {
			return &stateRepairInventory{
				nodes:     []stateRepairNode{{name: "node-a", manager: &blockingHistoryRepairManager{}, runtime: &recordingStateRepairRuntime{}}},
				histories: []stateHistoryCandidate{{source: "node-a", history: testRepairHistory()}},
			}, nil
		}
		acquireStateRepairLeasesForCommand = func(nodes []stateRepairNode, envName string) ([]stateRepairLease, error) {
			return nil, nil
		}
		syncStateRepairHistoryToLocalForCommand = func(cfg *config.Config, envName string, history *remotestate.DeploymentHistory) (int, error) {
			return 1, nil
		}
		defer func() {
			cfgFile = oldCfgFile
			envFlag = oldEnvFlag
			stateServer = oldStateServer
			collectStateRepairNodesForCommand = oldCollect
			acquireStateRepairLeasesForCommand = oldAcquire
			syncStateRepairHistoryToLocalForCommand = oldSync
			cliEngineInstance = oldEngine
		}()

		stdout := captureConfigExportStdout(t, func() {
			if err := runStateRepair(nil, nil); err != nil {
				t.Fatalf("runStateRepair returned error: %v", err)
			}
		})
		for _, human := range []string{"Project:", "Deployment history source", "Synced", "Repaired deployment history"} {
			if strings.Contains(stdout, human) {
				t.Fatalf("machine stdout contains human repair output %q: %q", human, stdout)
			}
		}
		var decoded engine.StateRepairResult
		if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
			t.Fatalf("stdout is not parseable repair JSON: %v\n%s", err, stdout)
		}
		if decoded.Kind != engine.KindStateRepairResult || decoded.Project != "demo" || decoded.Counts.ReachableNodes != 1 || decoded.Local.Status != engine.StateRepairLocalSyncStatusSynced {
			t.Fatalf("decoded result = %#v", decoded)
		}
	})
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

func TestWriteStateRepairDocumentsDeletesStaleNodeActual(t *testing.T) {
	runtime := &recordingStateRepairRuntime{
		previousActual: &takodstate.ActualSnapshot{
			Project:     "demo",
			Environment: "production",
			TargetNodes: []string{"node-a", "node-b"},
			Nodes: map[string]takodstate.ActualNodeSnapshot{
				"node-b": {Node: "node-b"},
			},
			CapturedAt: time.Now().UTC().Add(-time.Hour),
		},
	}
	nodes := []stateRepairNode{
		{name: "node-a", runtime: runtime},
	}
	currentActual := &takodstate.ActualSnapshot{
		Project:     "demo",
		Environment: "production",
		TargetNodes: []string{"node-a"},
		Services:    map[string]takodstate.ActualService{},
		CapturedAt:  time.Now().UTC(),
	}

	_, _, actualWritten, nodeActualWritten, err := writeStateRepairDocuments(
		nodes,
		stateHistoryCandidate{},
		false,
		stateDesiredCandidate{},
		false,
		stateActualCandidate{actual: currentActual},
		true,
		map[string]stateNodeActualCandidate{
			"node-a": {node: "node-a", actual: nodeActualSnapshot("node-a", time.Now().UTC(), "web")},
		},
	)
	if err != nil {
		t.Fatalf("writeStateRepairDocuments returned error: %v", err)
	}
	if actualWritten != 1 || nodeActualWritten != 1 {
		t.Fatalf("actualWritten=%d nodeActualWritten=%d, want 1/1", actualWritten, nodeActualWritten)
	}
	if got, want := strings.Join(runtime.deleted, ","), "node-b"; got != want {
		t.Fatalf("deleted stale node actual = %q, want %q", got, want)
	}
}

func TestCloseStateRepairNodesUsesCleanupCallback(t *testing.T) {
	cleaned := 0
	closeStateRepairNodes([]stateRepairNode{
		{
			name: "node-a",
			cleanup: func() {
				cleaned++
			},
		},
		{
			name: "node-b",
			cleanup: func() {
				cleaned++
			},
		},
	})

	if cleaned != 2 {
		t.Fatalf("cleanup calls = %d, want 2", cleaned)
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

type operationRecordingLeaseManager struct {
	operations []string
}

func (m *operationRecordingLeaseManager) LoadHistory() (*remotestate.DeploymentHistory, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *operationRecordingLeaseManager) SaveHistory(*remotestate.DeploymentHistory) error {
	return fmt.Errorf("not implemented")
}

func (m *operationRecordingLeaseManager) AcquireLease(operation string, environment string, ttl time.Duration) (*remotestate.LeaseInfo, error) {
	m.operations = append(m.operations, operation)
	return &remotestate.LeaseInfo{ID: operation + "-lease"}, nil
}

func (m *operationRecordingLeaseManager) ReleaseLease(*remotestate.LeaseInfo) error {
	return nil
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

type recordingStateRepairRuntime struct {
	previousActual *takodstate.ActualSnapshot
	nodeActual     map[string]*takodstate.ActualSnapshot
	writtenActual  *takodstate.ActualSnapshot
	deleted        []string
	events         []takodstate.Event
}

func (m *recordingStateRepairRuntime) ReadActual() (*takodstate.ActualSnapshot, error) {
	if m.previousActual == nil {
		return nil, takodstate.ErrNotFound
	}
	return m.previousActual, nil
}

func (m *recordingStateRepairRuntime) ReadNodeActual(node string) (*takodstate.ActualSnapshot, error) {
	if m.nodeActual == nil {
		return nil, takodstate.ErrNotFound
	}
	actual, ok := m.nodeActual[node]
	if !ok {
		return nil, takodstate.ErrNotFound
	}
	return actual, nil
}

func (m *recordingStateRepairRuntime) WriteDesired(*takodstate.DesiredRevision) error {
	return nil
}

func (m *recordingStateRepairRuntime) WriteActual(actual *takodstate.ActualSnapshot) error {
	m.writtenActual = actual
	return nil
}

func (m *recordingStateRepairRuntime) WriteNodeActual(string, *takodstate.ActualSnapshot) error {
	return nil
}

func (m *recordingStateRepairRuntime) DeleteNodeActual(node string) error {
	m.deleted = append(m.deleted, node)
	return nil
}

func (m *recordingStateRepairRuntime) AppendEvent(event takodstate.Event) error {
	m.events = append(m.events, event)
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
