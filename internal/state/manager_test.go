package state

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDecodeStateDocumentContentReturnsNotFoundSentinel(t *testing.T) {
	_, err := decodeStateDocumentContent(`{"found":false}`, "history")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestDecodeStateDocumentContentRejectsInvalidEnvelope(t *testing.T) {
	_, err := decodeStateDocumentContent(`{`, "history")
	if err == nil || !strings.Contains(err.Error(), "failed to parse takod state response") {
		t.Fatalf("error = %v, want parse error", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, should not match ErrNotFound", err)
	}
}

func TestDecodeStateDocumentContentRejectsEmptyContent(t *testing.T) {
	_, err := decodeStateDocumentContent(`{"found":true}`, "history")
	if err == nil || !strings.Contains(err.Error(), "empty takod state document history") {
		t.Fatalf("error = %v, want empty content error", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, should not match ErrNotFound", err)
	}
}

func TestDecodeStateDocumentContentReturnsContent(t *testing.T) {
	content, err := decodeStateDocumentContent(`{"found":true,"content":"{\"deployments\":[]}\n"}`, "history")
	if err != nil {
		t.Fatalf("decodeStateDocumentContent returned error: %v", err)
	}
	if content != "{\"deployments\":[]}\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestStateManagerWithRequestTimeoutUsesCustomDeadline(t *testing.T) {
	client := &fakeStateManagerExecutor{
		output: `{"found":true,"content":"{\"deployments\":[]}\n"}`,
	}
	timeout := 7 * time.Second
	manager := (&StateManager{
		client:      client,
		socket:      "/run/tako/takod.sock",
		projectName: "demo",
		environment: "production",
		server:      "node-a",
	}).WithRequestTimeout(timeout)

	if _, err := manager.LoadHistory(); err != nil {
		t.Fatalf("LoadHistory returned error: %v", err)
	}
	if !client.deadlineWithin(timeout) {
		t.Fatalf("deadline = %s, want near %s", client.deadline.Sub(client.startedAt), timeout)
	}
}

func TestPruneAndSortDeploymentsDropsNilSortsAndLimits(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	got := pruneAndSortDeployments([]*DeploymentState{
		{ID: "old", Timestamp: base},
		nil,
		{ID: "new", Timestamp: base.Add(time.Hour)},
		{ID: "middle", Timestamp: base.Add(time.Minute)},
	}, 2)

	if len(got) != 2 {
		t.Fatalf("deployments = %d, want 2", len(got))
	}
	if got[0].ID != "new" || got[1].ID != "middle" {
		t.Fatalf("deployments order = [%s %s], want [new middle]", got[0].ID, got[1].ID)
	}
}

type fakeStateManagerExecutor struct {
	output    string
	startedAt time.Time
	deadline  time.Time
}

func (f *fakeStateManagerExecutor) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
	f.startedAt = time.Now()
	if deadline, ok := ctx.Deadline(); ok {
		f.deadline = deadline
	}
	return f.output, nil
}

func (f *fakeStateManagerExecutor) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
	f.startedAt = time.Now()
	if deadline, ok := ctx.Deadline(); ok {
		f.deadline = deadline
	}
	return f.output, nil
}

func (f *fakeStateManagerExecutor) deadlineWithin(want time.Duration) bool {
	if f.deadline.IsZero() || f.startedAt.IsZero() {
		return false
	}
	got := f.deadline.Sub(f.startedAt)
	return got > want-time.Second && got < want+time.Second
}
