package state

import (
	"errors"
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
