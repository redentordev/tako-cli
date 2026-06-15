package takod

import (
	"bytes"
	"context"
	"os/exec"
	"slices"
	"testing"
)

func TestStreamNodeLogsRunsAllowedJournalUnit(t *testing.T) {
	oldCommand := nodeLogsCommandContext
	var gotName string
	var gotArgs []string
	nodeLogsCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return exec.CommandContext(ctx, "sh", "-c", "printf 'node log\\n'")
	}
	t.Cleanup(func() { nodeLogsCommandContext = oldCommand })

	var output bytes.Buffer
	if err := StreamNodeLogs(context.Background(), NodeLogsRequest{Tail: 25}, &output); err != nil {
		t.Fatalf("StreamNodeLogs returned error: %v", err)
	}
	if gotName != "journalctl" {
		t.Fatalf("command = %q, want journalctl", gotName)
	}
	wantArgs := []string{"-u", "takod", "--no-pager", "-n", "25"}
	if !slices.Equal(gotArgs, wantArgs) {
		t.Fatalf("args = %#v, want %#v", gotArgs, wantArgs)
	}
	if output.String() != "node log\n" {
		t.Fatalf("output = %q, want node log", output.String())
	}
}

func TestStreamNodeLogsRejectsUnsupportedUnit(t *testing.T) {
	var output bytes.Buffer
	if err := StreamNodeLogs(context.Background(), NodeLogsRequest{Unit: "ssh"}, &output); err == nil {
		t.Fatal("expected unsupported unit to be rejected")
	}
}
