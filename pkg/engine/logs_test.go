package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

func TestStreamLogNodesWithRunsConcurrentlyAndKeepsSortedOrder(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-c": {Host: "node-c.example.test"},
		"node-a": {Host: "node-a.example.test"},
		"node-b": {Host: "node-b.example.test"},
	}
	started := make(chan string, len(servers))
	release := make(chan struct{})

	resultsDone := make(chan []LogNodeResult, 1)
	go func() {
		resultsDone <- StreamLogNodesWith(context.Background(), servers, func(serverName string, _ config.ServerConfig, prefix bool) error {
			if !prefix {
				return fmt.Errorf("expected multi-node stream to request prefixes")
			}
			started <- serverName
			<-release
			return nil
		})
	}()

	waitForEngineLogStarts(t, started, len(servers))
	close(release)

	results := <-resultsDone
	wantNames := []string{"node-a", "node-b", "node-c"}
	for i, want := range wantNames {
		if results[i].ServerName != want {
			t.Fatalf("result %d server = %q, want %q", i, results[i].ServerName, want)
		}
		if results[i].Err != nil {
			t.Fatalf("result %d err = %v", i, results[i].Err)
		}
	}
}

func TestSummarizeLogStreamResultsReportsSortedErrors(t *testing.T) {
	err := SummarizeLogStreamResults([]LogNodeResult{
		{ServerName: "node-b", Err: fmt.Errorf("second")},
		{ServerName: "node-a", Err: fmt.Errorf("first")},
	})
	if err == nil {
		t.Fatal("expected all-node failure")
	}
	message := err.Error()
	if !strings.Contains(message, "failed to stream logs from all target nodes") {
		t.Fatalf("unexpected error message: %s", message)
	}
	if strings.Index(message, "node-a") > strings.Index(message, "node-b") {
		t.Fatalf("expected node errors to be sorted: %s", message)
	}
}

func TestEmitLogLineUsesStructuredEventAndHistoricalMessage(t *testing.T) {
	sink := &events.BufferSink{}
	eng := New(Options{Sink: sink})

	eng.emitLogLine("web", "node-a", "hello", true)

	emitted := sink.Events()
	if len(emitted) != 1 {
		t.Fatalf("events = %d, want 1", len(emitted))
	}
	event := emitted[0]
	if event.Type != events.TypeLogLine || event.Phase != events.PhaseLogs || event.Service != "web" || event.Node != "node-a" {
		t.Fatalf("unexpected event identity: %#v", event)
	}
	if event.Message != "[node-a] hello\n" {
		t.Fatalf("message = %q", event.Message)
	}
	if event.Data["service"] != "web" || event.Data["node"] != "node-a" || event.Data["data"] != "hello" {
		t.Fatalf("unexpected event data: %#v", event.Data)
	}
}

func TestLogsResultJSONShape(t *testing.T) {
	result := LogsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindLogsResult,
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Tail:        25,
		Status:      logsStatusSuccess,
		Nodes:       []LogsNodeResult{{Name: "node-a", Host: "node-a.example.test", Status: logsStatusSuccess}},
		StartedAt:   time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Duration:    1.25,
		Message:     "streamed logs from 1 node(s)",
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(payload)
	for _, want := range []string{`"kind":"LogsResult"`, `"service":"web"`, `"nodes":[{"name":"node-a"`, `"durationSeconds":1.25`} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("result JSON missing %s: %s", want, jsonText)
		}
	}
}

func waitForEngineLogStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for log fanout; saw %v", seen)
		}
	}
}
