package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

func TestStateForgetNodePrunesAggregateDeletesStandaloneAndReturnsResult(t *testing.T) {
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	actual := &takodstate.ActualSnapshot{
		Project:     "demo",
		Environment: "production",
		TargetNodes: []string{"node-a", "node-b"},
		Nodes: map[string]takodstate.ActualNodeSnapshot{
			"node-a": {Node: "node-a", CapturedAt: base},
			"node-b": {Node: "node-b", CapturedAt: base},
		},
		CapturedAt: base,
	}
	runtime := &forgetNodeRuntimeFake{
		actual: actual,
		nodeActual: map[string]*takodstate.ActualSnapshot{
			"node-b": {Project: "demo", Environment: "production", Node: "node-b", CapturedAt: base},
		},
	}
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {Servers: []string{"node-a"}},
		},
	}

	result, err := New(Options{}).StateForgetNode(t.Context(), StateForgetNodeRequest{
		Config:      cfg,
		Environment: "production",
		NodeName:    "node-b",
		Nodes:       []StateForgetNodeNode{{Name: "node-a", Runtime: runtime}},
	})
	if err != nil {
		t.Fatalf("StateForgetNode returned error: %v", err)
	}
	if result.Kind != KindStateForgetNodeResult || result.Status != StateForgetNodeStatusSuccess {
		t.Fatalf("result kind/status = %s/%s", result.Kind, result.Status)
	}
	if result.Summary.ReachableNodes != 1 || result.Summary.StandaloneSnapshotsFound != 1 || result.Summary.AggregateActualStatesPruned != 1 {
		t.Fatalf("summary = %#v", result.Summary)
	}
	if runtime.writtenActual == nil {
		t.Fatal("expected pruned aggregate actual state to be written")
	}
	if _, ok := runtime.writtenActual.Nodes["node-b"]; ok {
		t.Fatalf("node-b still present in written aggregate: %#v", runtime.writtenActual.Nodes)
	}
	if got := strings.Join(runtime.deleted, ","); got != "node-b" {
		t.Fatalf("deleted nodes = %q, want node-b", got)
	}
	if len(runtime.events) != 1 || runtime.events[0].Type != "state_node_forgotten" {
		t.Fatalf("events = %#v", runtime.events)
	}
}

func TestStateForgetNodeRejectsConfiguredNodeWithoutForce(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {Servers: []string{"node-a"}},
		},
	}

	_, err := New(Options{}).StateForgetNode(t.Context(), StateForgetNodeRequest{
		Config:      cfg,
		Environment: "production",
		NodeName:    "node-a",
		Nodes:       []StateForgetNodeNode{{Name: "node-a", Runtime: &forgetNodeRuntimeFake{}}},
	})
	if err == nil || !strings.Contains(err.Error(), "still listed in environment") {
		t.Fatalf("error = %v, want still-listed validation", err)
	}
	if Classify(err) != ClassInvalid {
		t.Fatalf("class = %d, want ClassInvalid", Classify(err))
	}
}

func TestForgetNodeFromRuntimeNodesAggregatesFatalErrors(t *testing.T) {
	results, err := ForgetNodeFromRuntimeNodes([]StateForgetNodeNode{
		{Name: "node-a", Runtime: nil},
		{Name: "node-b", Runtime: &forgetNodeRuntimeFake{}},
	}, "demo", "production", "node-z")
	if err == nil || !strings.Contains(err.Error(), "node-a") {
		t.Fatalf("error = %v, want node-a fatal", err)
	}
	if len(results) != 2 || results[0].Error == "" || results[1].Error != "" {
		t.Fatalf("results = %#v", results)
	}
}

type forgetNodeRuntimeFake struct {
	actual        *takodstate.ActualSnapshot
	nodeActual    map[string]*takodstate.ActualSnapshot
	writtenActual *takodstate.ActualSnapshot
	deleted       []string
	events        []takodstate.Event
}

func (m *forgetNodeRuntimeFake) ReadActual() (*takodstate.ActualSnapshot, error) {
	if m.actual == nil {
		return nil, takodstate.ErrNotFound
	}
	return m.actual, nil
}

func (m *forgetNodeRuntimeFake) ReadNodeActual(node string) (*takodstate.ActualSnapshot, error) {
	if m.nodeActual == nil {
		return nil, takodstate.ErrNotFound
	}
	actual, ok := m.nodeActual[node]
	if !ok {
		return nil, takodstate.ErrNotFound
	}
	return actual, nil
}

func (m *forgetNodeRuntimeFake) WriteActual(actual *takodstate.ActualSnapshot) error {
	m.writtenActual = actual
	return nil
}

func (m *forgetNodeRuntimeFake) DeleteNodeActual(node string) error {
	m.deleted = append(m.deleted, node)
	return nil
}

func (m *forgetNodeRuntimeFake) AppendEvent(event takodstate.Event) error {
	m.events = append(m.events, event)
	return nil
}
