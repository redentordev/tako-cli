package takod

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestRenderPrometheusMetricsIncludesNodeAndProjectMetrics(t *testing.T) {
	restoreMetrics := useTempMetricsFile(t, `{
  "timestamp": "2026-06-14T18:00:00Z",
  "cpu_percent": "12.5",
  "memory": {"total_mb": 1024, "used_mb": 256, "available_mb": 768, "percent": "25", "swap_total_mb": 512, "swap_used_mb": 0},
  "disk": {"total_mb": 2048, "used_mb": 1024, "available_mb": 1024, "percent": "50"},
  "network": {"rx_bytes": 100, "tx_bytes": 200},
  "disk_io": {"read_sectors": 300, "write_sectors": 400},
  "uptime_seconds": 3600,
  "load_average": {"1min": "0.1", "5min": "0.2", "15min": "0.3"}
}`)
	defer restoreMetrics()
	restoreDocker := useFakeActualDocker(t)
	defer restoreDocker()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo_production_web_1|demo/web:1|container-a|hash-web||demo|production|web\n")

	dataDir := t.TempDir()
	if _, err := WriteStateDocument(context.Background(), dataDir, StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentDesired,
		RevisionID:  "rev-1",
		Content:     `{"project":"demo","environment":"production","revisionId":"rev-1","services":{"web":{"replicas":2}}}` + "\n",
	}); err != nil {
		t.Fatalf("failed to write desired fixture: %v", err)
	}
	if _, err := WriteStateDocument(context.Background(), dataDir, StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentHistory,
		Content:     `{"projectName":"demo","environment":"production","deployments":[{"status":"success","timestamp":"2026-06-14T18:01:00Z","duration":2000000000}],"lastUpdated":"2026-06-14T18:01:00Z"}` + "\n",
	}); err != nil {
		t.Fatalf("failed to write history fixture: %v", err)
	}
	if _, err := AcquireLease(context.Background(), dataDir, LeaseRequest{
		ID:          "lease-1",
		Project:     "demo",
		Environment: "production",
		Operation:   "deploy",
		Who:         "test",
		PID:         os.Getpid(),
		TTLSeconds:  300,
	}); err != nil {
		t.Fatalf("failed to acquire lease fixture: %v", err)
	}

	output, err := RenderPrometheusMetrics(context.Background(), PrometheusMetricsRequest{
		Project:     "demo",
		Environment: "production",
		Node:        "node-a",
		DataDir:     dataDir,
		StartedAt:   time.Date(2026, 6, 14, 17, 59, 0, 0, time.UTC),
		Now:         time.Date(2026, 6, 14, 18, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RenderPrometheusMetrics returned error: %v", err)
	}
	for _, want := range []string{
		`tako_node_cpu_percent{node="node-a"} 12.5`,
		`tako_takod_uptime_seconds{node="node-a"} 60`,
		`tako_node_memory_bytes{node="node-a",kind="total"} 1073741824`,
		`tako_service_replicas{node="node-a",project="demo",environment="production",service="web",state="running"} 1`,
		`tako_service_replicas{node="node-a",project="demo",environment="production",service="web",state="desired"} 2`,
		`tako_lease_held{node="node-a",project="demo",environment="production",operation="deploy"} 1`,
		`tako_deployment_last_status{node="node-a",project="demo",environment="production",status="success"} 1`,
		`tako_deployment_last_duration_seconds{node="node-a",project="demo",environment="production"} 2`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("prometheus output missing %q:\n%s", want, output)
		}
	}
	if strings.Count(output, "# HELP tako_node_memory_bytes") != 1 {
		t.Fatalf("expected one HELP line per metric name:\n%s", output)
	}
}

func TestRenderPrometheusMetricsRequiresProjectAndEnvironmentTogether(t *testing.T) {
	restoreMetrics := useTempMetricsFile(t, `{"cpu_percent":"12.5"}`)
	defer restoreMetrics()

	_, err := RenderPrometheusMetrics(context.Background(), PrometheusMetricsRequest{
		Project: "demo",
	})
	if err == nil {
		t.Fatal("expected project without environment to be rejected")
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
