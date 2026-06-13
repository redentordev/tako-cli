package takod

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStreamProxyAccessLogsTailsConfiguredFile(t *testing.T) {
	restore := useTempAccessLog(t, "one\ntwo\nthree\n")
	defer restore()

	var output bytes.Buffer
	if err := StreamProxyAccessLogs(context.Background(), 2, false, &output); err != nil {
		t.Fatalf("StreamProxyAccessLogs returned error: %v", err)
	}
	if output.String() != "two\nthree\n" {
		t.Fatalf("unexpected access log output: %q", output.String())
	}
}

func TestStreamProxyAccessLogsRejectsInvalidTail(t *testing.T) {
	var output bytes.Buffer
	err := StreamProxyAccessLogs(context.Background(), -1, false, &output)
	if err == nil || !strings.Contains(err.Error(), "tail cannot be negative") {
		t.Fatalf("expected invalid tail error, got %v", err)
	}
}

func useTempAccessLog(t *testing.T, content string) func() {
	t.Helper()
	oldPath := proxyAccessLogPath
	path := filepath.Join(t.TempDir(), "access.log")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write access log fixture: %v", err)
	}
	proxyAccessLogPath = path
	return func() {
		proxyAccessLogPath = oldPath
	}
}
