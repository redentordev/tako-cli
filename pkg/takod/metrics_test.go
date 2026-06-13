package takod

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadNodeMetricsReadsCurrentMetricsFile(t *testing.T) {
	restore := useTempMetricsFile(t, `{"cpu_percent":"12.5","uptime_seconds":123}`+"\n")
	defer restore()

	response, err := ReadNodeMetrics(context.Background(), false)
	if err != nil {
		t.Fatalf("ReadNodeMetrics returned error: %v", err)
	}
	if response.Collected {
		t.Fatal("expected Collected=false")
	}
	if !strings.Contains(string(response.Metrics), `"cpu_percent":"12.5"`) {
		t.Fatalf("unexpected metrics payload: %s", string(response.Metrics))
	}
}

func TestReadNodeMetricsRejectsInvalidJSON(t *testing.T) {
	restore := useTempMetricsFile(t, `not-json`)
	defer restore()

	_, err := ReadNodeMetrics(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("expected invalid JSON error, got %v", err)
	}
}

func useTempMetricsFile(t *testing.T, content string) func() {
	t.Helper()
	oldPath := metricsCurrentPath
	path := filepath.Join(t.TempDir(), "current.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write metrics fixture: %v", err)
	}
	metricsCurrentPath = path
	return func() {
		metricsCurrentPath = oldPath
	}
}
