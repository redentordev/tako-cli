package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

func TestStateRepairSelectsAndMergesFreshestSources(t *testing.T) {
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	mgrA := &fakeStateRepairHistoryManager{}
	runtimeA := &fakeStateRepairRuntime{}
	mgrB := &fakeStateRepairHistoryManager{}
	runtimeB := &fakeStateRepairRuntime{}
	result, err := New(Options{}).StateRepair(context.Background(), StateRepairRequest{
		Config:      testStateRepairConfig(),
		Environment: "production",
		Nodes: []StateRepairNode{
			{Name: "node-a", HistoryManager: mgrA, Runtime: runtimeA},
			{Name: "node-b", HistoryManager: mgrB, Runtime: runtimeB},
		},
		Histories: []StateRepairHistoryCandidate{
			{Source: "node-a", History: repairHistory(base, "old")},
			{Source: "node-b", History: repairHistory(base.Add(time.Hour), "new")},
		},
		Desired: []StateRepairDesiredCandidate{
			{Source: "node-a", Desired: repairDesired("rev-a", base)},
			{Source: "node-b", Desired: repairDesired("rev-b", base.Add(time.Hour))},
		},
		Actual: []StateRepairActualCandidate{{Source: "node-a", Actual: repairActual("", base, "web")}},
		NodeActual: []StateRepairNodeActualCandidate{
			{Source: "node-a", Node: "node-a", Actual: repairNodeActual("node-a", base.Add(2*time.Hour), "web")},
			{Source: "node-b", Node: "node-b", Actual: repairNodeActual("node-b", base.Add(3*time.Hour), "worker")},
		},
	})
	if err != nil {
		t.Fatalf("StateRepair returned error: %v", err)
	}
	if result.Sources.History == nil || result.Sources.History.Source != "node-b" || result.Sources.History.Count != 1 {
		t.Fatalf("history source = %#v", result.Sources.History)
	}
	if result.Sources.Desired == nil || result.Sources.Desired.RevisionID != "rev-b" {
		t.Fatalf("desired source = %#v", result.Sources.Desired)
	}
	if result.Sources.Actual == nil || result.Sources.Actual.ServiceCount != 2 || len(result.Sources.NodeActual) != 2 {
		t.Fatalf("actual/node sources = %#v / %#v", result.Sources.Actual, result.Sources.NodeActual)
	}
	if got := runtimeA.writtenActual.Services["web"].Replicas; got != 1 {
		t.Fatalf("merged web replicas = %d, want 1", got)
	}
	if _, ok := runtimeA.writtenActual.Services["worker"]; !ok {
		t.Fatalf("merged aggregate missing worker: %#v", runtimeA.writtenActual.Services)
	}
	if mgrA.savedHistory.Deployments[0].ID != "new" || mgrB.savedHistory.Deployments[0].ID != "new" {
		t.Fatalf("history was not repaired from freshest source")
	}
}

func TestStateRepairWriteWarningsReturnIncompleteWithCounts(t *testing.T) {
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	result, err := New(Options{}).StateRepair(context.Background(), StateRepairRequest{
		Config:      testStateRepairConfig(),
		Environment: "production",
		Nodes: []StateRepairNode{
			{Name: "node-a", HistoryManager: &fakeStateRepairHistoryManager{}, Runtime: &fakeStateRepairRuntime{}},
			{Name: "node-b", HistoryManager: &fakeStateRepairHistoryManager{saveErr: fmt.Errorf("disk full")}, Runtime: &fakeStateRepairRuntime{}},
		},
		Histories: []StateRepairHistoryCandidate{{Source: "node-a", History: repairHistory(base, "deploy-1")}},
	})
	if err == nil || !strings.Contains(err.Error(), "state repair incomplete") {
		t.Fatalf("err = %v, want incomplete", err)
	}
	if result == nil || result.Writes.Counts.History != 1 || result.Status != StateRepairStatusFailed {
		t.Fatalf("result counts/status = %#v", result)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "failed to repair deployment history on node-b") {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
}

func TestStateRepairZeroWritesFatalBehavior(t *testing.T) {
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	result, err := New(Options{}).StateRepair(context.Background(), StateRepairRequest{
		Config:      testStateRepairConfig(),
		Environment: "production",
		Nodes: []StateRepairNode{
			{Name: "node-a", HistoryManager: &fakeStateRepairHistoryManager{saveErr: fmt.Errorf("disk full")}, Runtime: &fakeStateRepairRuntime{}},
			{Name: "node-b", HistoryManager: &fakeStateRepairHistoryManager{saveErr: fmt.Errorf("disk full")}, Runtime: &fakeStateRepairRuntime{}},
		},
		Histories: []StateRepairHistoryCandidate{{Source: "node-a", History: repairHistory(base, "deploy-1")}},
	})
	if err == nil || !strings.Contains(err.Error(), "failed to write repaired deployment history to any reachable node") {
		t.Fatalf("err = %v, want zero-write fatal", err)
	}
	if result == nil || result.Writes.Counts.History != 0 || len(result.Warnings) != 2 {
		t.Fatalf("result = %#v", result)
	}
}

func TestStateRepairDeletesStaleNodeActual(t *testing.T) {
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	runtime := &fakeStateRepairRuntime{previousActual: &takodstate.ActualSnapshot{
		Project:     "demo",
		Environment: "production",
		TargetNodes: []string{"node-a", "node-b"},
		Nodes: map[string]takodstate.ActualNodeSnapshot{
			"node-b": {Node: "node-b", CapturedAt: base},
		},
		CapturedAt: base,
	}}
	result, err := New(Options{}).StateRepair(context.Background(), StateRepairRequest{
		Config:      testStateRepairConfig(),
		Environment: "production",
		Nodes:       []StateRepairNode{{Name: "node-a", Runtime: runtime}},
		Actual:      []StateRepairActualCandidate{{Source: "node-a", Actual: repairActual("", base.Add(time.Hour), "web")}},
		NodeActual:  []StateRepairNodeActualCandidate{{Source: "node-a", Node: "node-a", Actual: repairNodeActual("node-a", base.Add(time.Hour), "web")}},
	})
	if err != nil {
		t.Fatalf("StateRepair returned error: %v", err)
	}
	if got := strings.Join(runtime.deleted, ","); got != "node-b" {
		t.Fatalf("deleted = %q, want node-b", got)
	}
	if result.Writes.Counts.Actual != 1 || result.Writes.Counts.NodeActual != 1 {
		t.Fatalf("write counts = %#v", result.Writes.Counts)
	}
}

func testStateRepairConfig() *config.Config {
	return &config.Config{Project: config.ProjectConfig{Name: "demo"}}
}

func repairHistory(ts time.Time, id string) *remotestate.DeploymentHistory {
	return &remotestate.DeploymentHistory{
		ProjectName: "demo",
		Environment: "production",
		LastUpdated: ts,
		Deployments: []*remotestate.DeploymentState{{ID: id, Timestamp: ts, Status: remotestate.StatusSuccess}},
	}
}

func repairDesired(revision string, ts time.Time) *takodstate.DesiredRevision {
	return &takodstate.DesiredRevision{RevisionID: revision, CreatedAt: ts}
}

func repairActual(node string, ts time.Time, services ...string) *takodstate.ActualSnapshot {
	snapshot := &takodstate.ActualSnapshot{Project: "demo", Environment: "production", Node: node, Services: map[string]takodstate.ActualService{}, CapturedAt: ts}
	for _, service := range services {
		snapshot.Services[service] = takodstate.ActualService{Name: service, Replicas: 1}
	}
	return snapshot
}

func repairNodeActual(node string, ts time.Time, services ...string) *takodstate.ActualSnapshot {
	snapshot := repairActual(node, ts, services...)
	snapshot.Node = node
	return snapshot
}

type fakeStateRepairHistoryManager struct {
	savedHistory *remotestate.DeploymentHistory
	saveErr      error
}

func (m *fakeStateRepairHistoryManager) SaveHistory(history *remotestate.DeploymentHistory) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.savedHistory = history
	return nil
}

type fakeStateRepairRuntime struct {
	previousActual *takodstate.ActualSnapshot
	writtenDesired *takodstate.DesiredRevision
	writtenActual  *takodstate.ActualSnapshot
	nodeActual     map[string]*takodstate.ActualSnapshot
	deleted        []string
	writeErr       error
}

func (m *fakeStateRepairRuntime) WriteDesired(revision *takodstate.DesiredRevision) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	m.writtenDesired = revision
	return nil
}

func (m *fakeStateRepairRuntime) ReadActual() (*takodstate.ActualSnapshot, error) {
	if m.previousActual == nil {
		return nil, takodstate.ErrNotFound
	}
	return m.previousActual, nil
}

func (m *fakeStateRepairRuntime) WriteActual(snapshot *takodstate.ActualSnapshot) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	m.writtenActual = snapshot
	return nil
}

func (m *fakeStateRepairRuntime) WriteNodeActual(node string, snapshot *takodstate.ActualSnapshot) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	if m.nodeActual == nil {
		m.nodeActual = make(map[string]*takodstate.ActualSnapshot)
	}
	m.nodeActual[node] = snapshot
	return nil
}

func (m *fakeStateRepairRuntime) DeleteNodeActual(node string) error {
	m.deleted = append(m.deleted, node)
	return nil
}
