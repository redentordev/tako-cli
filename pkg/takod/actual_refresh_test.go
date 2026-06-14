package takod

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestRefreshActualStateDocumentsWritesNodeAndAggregateState(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := WriteStateDocument(context.Background(), dataDir, StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentDesired,
		Content:     "{\"revisionId\":\"rev-1\"}\n",
	}); err != nil {
		t.Fatalf("failed to write desired fixture: %v", err)
	}

	restore := useFakeActualDocker(t)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo_production_web_1|demo/web:1|container-a|hash-web\n")

	refreshed, err := RefreshActualStateDocuments(context.Background(), dataDir, "node-a")
	if err != nil {
		t.Fatalf("RefreshActualStateDocuments returned error: %v", err)
	}
	if refreshed != 1 {
		t.Fatalf("refreshed = %d, want 1", refreshed)
	}

	nodeSnapshot := readActualSnapshotFixture(t, dataDir, StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentActualNode,
		Node:        "node-a",
	})
	if nodeSnapshot.Node != "node-a" {
		t.Fatalf("node snapshot node = %q, want node-a", nodeSnapshot.Node)
	}
	if got := nodeSnapshot.Services["web"].Replicas; got != 1 {
		t.Fatalf("node web replicas = %d, want 1", got)
	}
	if got := nodeSnapshot.Services["web"].ConfigHash; got != "hash-web" {
		t.Fatalf("node web config hash = %q, want hash-web", got)
	}

	aggregate := readActualSnapshotFixture(t, dataDir, StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentActual,
	})
	if got := aggregate.Services["web"].Replicas; got != 1 {
		t.Fatalf("aggregate web replicas = %d, want 1", got)
	}
	if got := aggregate.Services["web"].ConfigHash; got != "hash-web" {
		t.Fatalf("aggregate web config hash = %q, want hash-web", got)
	}
	if _, ok := aggregate.Nodes["node-a"]; !ok {
		t.Fatalf("aggregate missing node-a snapshot: %#v", aggregate.Nodes)
	}
}

func TestRefreshActualStateDocumentsSkipsHeldLease(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := WriteStateDocument(context.Background(), dataDir, StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentDesired,
		Content:     "{\"revisionId\":\"rev-1\"}\n",
	}); err != nil {
		t.Fatalf("failed to write desired fixture: %v", err)
	}
	if _, err := AcquireLease(context.Background(), dataDir, LeaseRequest{
		ID:          "lease-1",
		Project:     "demo",
		Environment: "production",
		Operation:   "deploy",
		Who:         "test",
		PID:         os.Getpid(),
		TTLSeconds:  int64((5 * time.Minute).Seconds()),
	}); err != nil {
		t.Fatalf("AcquireLease returned error: %v", err)
	}

	restore := useFakeActualDocker(t)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo_production_web_1|demo/web:1|container-a\n")

	refreshed, err := RefreshActualStateDocuments(context.Background(), dataDir, "node-a")
	if err != nil {
		t.Fatalf("RefreshActualStateDocuments returned error: %v", err)
	}
	if refreshed != 0 {
		t.Fatalf("refreshed = %d, want 0", refreshed)
	}
	response, err := ReadStateDocument(context.Background(), dataDir, StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentActualNode,
		Node:        "node-a",
	})
	if err != nil {
		t.Fatalf("ReadStateDocument returned error: %v", err)
	}
	if response.Found {
		t.Fatalf("expected no node actual state while lease is held: %#v", response)
	}
}

func readActualSnapshotFixture(t *testing.T, dataDir string, request StateDocumentRequest) persistedActualSnapshot {
	t.Helper()
	response, err := ReadStateDocument(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("ReadStateDocument returned error: %v", err)
	}
	if !response.Found {
		t.Fatalf("expected actual state document to exist: %#v", request)
	}
	var snapshot persistedActualSnapshot
	if err := json.Unmarshal([]byte(response.Content), &snapshot); err != nil {
		t.Fatalf("failed to decode actual state: %v", err)
	}
	return snapshot
}

func useFakeActualDocker(t *testing.T) func() {
	t.Helper()
	oldDocker := actualDockerCommandContext
	actualDockerCommandContext = fakeCommandContext
	t.Setenv("GO_WANT_TAKOD_COMMAND_HELPER", "1")
	return func() {
		actualDockerCommandContext = oldDocker
	}
}
