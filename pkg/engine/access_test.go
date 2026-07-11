package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takoapi"
)

func TestAccessResultJSONShape(t *testing.T) {
	result := AccessResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindAccessResult,
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Tail:        50,
		Status:      logsStatusSuccess,
		Nodes:       []AccessNodeResult{{Name: "node-a", Host: "node-a.example.test", Status: logsStatusSuccess}},
		StartedAt:   time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		Duration:    1.25,
		Message:     "streamed access logs from 1 node(s)",
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(payload)
	for _, want := range []string{`"kind":"AccessResult"`, `"service":"web"`, `"nodes":[{"name":"node-a"`, `"durationSeconds":1.25`} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("result JSON missing %s: %s", want, jsonText)
		}
	}
}

func TestNewAccessResultSuccessAndFailure(t *testing.T) {
	startedAt := time.Now().Add(-time.Second)
	nodes := []AccessNodeResult{
		{Name: "node-a", Host: "10.0.0.1", Status: logsStatusSuccess},
		{Name: "node-b", Host: "10.0.0.2", Status: logsStatusFailed, Error: "connect: refused"},
	}

	success := NewAccessResult("demo", "production", "", 50, true, startedAt, nodes[:1], nil)
	if success.Status != logsStatusSuccess {
		t.Fatalf("expected success status, got %q", success.Status)
	}
	if success.Message == "" || success.Error != "" {
		t.Fatalf("expected message and no error, got message=%q error=%q", success.Message, success.Error)
	}
	if success.Duration <= 0 {
		t.Fatalf("expected positive duration, got %v", success.Duration)
	}
	if success.Service != "" {
		t.Fatalf("expected empty service to stay empty, got %q", success.Service)
	}

	failure := NewAccessResult("demo", "production", "web", 50, false, startedAt, nodes, errors.New("access log streaming completed with 1 node error(s)"))
	if failure.Status != logsStatusFailed {
		t.Fatalf("expected failed status, got %q", failure.Status)
	}
	if failure.Error == "" || failure.Message != "" {
		t.Fatalf("expected error and no message, got message=%q error=%q", failure.Message, failure.Error)
	}
	if len(failure.Nodes) != 2 {
		t.Fatalf("expected both node outcomes, got %d", len(failure.Nodes))
	}
}
