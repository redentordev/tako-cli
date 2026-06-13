package takod

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestStreamServiceLogsUsesLabelFilteredContainers(t *testing.T) {
	logPath := t.TempDir() + "/commands.log"
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo_production_web_1\n")

	var output bytes.Buffer
	err := StreamServiceLogs(context.Background(), LogsRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Tail:        25,
	}, &output)
	if err != nil {
		t.Fatalf("StreamServiceLogs returned error: %v", err)
	}
	if output.String() != "logs\n" {
		t.Fatalf("unexpected log output %q", output.String())
	}

	entries := readCommandLog(t, logPath)
	if len(entries) != 2 {
		t.Fatalf("expected list and logs commands, got %#v", entries)
	}
	if !strings.Contains(entries[0], "docker ps -a") ||
		!strings.Contains(entries[0], "label=tako.project=demo") ||
		!strings.Contains(entries[0], "label=tako.environment=production") ||
		!strings.Contains(entries[0], "label=tako.service=web") {
		t.Fatalf("unexpected container discovery command: %q", entries[0])
	}
	if entries[1] != "docker logs --tail 25 demo_production_web_1" {
		t.Fatalf("unexpected logs command: %q", entries[1])
	}
}

func TestStreamServiceLogsRejectsInvalidTail(t *testing.T) {
	var output bytes.Buffer
	err := StreamServiceLogs(context.Background(), LogsRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Tail:        -1,
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "tail cannot be negative") {
		t.Fatalf("expected invalid tail error, got %v", err)
	}
}
