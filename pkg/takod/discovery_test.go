package takod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestListExportDiscoveryReadsNetworkLabels(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	t.Setenv("TAKO_FAKE_NETWORK_LS_OUTPUT", strings.Join([]string{
		"tako_backend_api_production_api_export",
		"tako_metrics_preview_collector_export",
	}, "\n")+"\n")
	labels := map[string]string{
		"tako_backend_api_production_api_export": `{"tako.discovery":"export","tako.project":"backend-api","tako.environment":"production","tako.service":"api","tako.export.alias":"backend-api-production-api","tako.runtime":"takod"}`,
		"tako_metrics_preview_collector_export":  `{"tako.discovery":"export","tako.project":"metrics","tako.environment":"preview","tako.service":"collector","tako.export.alias":"metrics-preview-collector","tako.runtime":"takod"}`,
	}
	encoded, err := json.Marshal(labels)
	if err != nil {
		t.Fatalf("failed to encode labels: %v", err)
	}
	t.Setenv("TAKO_FAKE_NETWORK_INSPECT_LABELS_BY_NAME", string(encoded))

	response, err := ListExportDiscovery(context.Background(), "production")
	if err != nil {
		t.Fatalf("ListExportDiscovery returned error: %v", err)
	}
	if len(response.Exports) != 1 {
		t.Fatalf("exports = %#v, want one production export", response.Exports)
	}
	got := response.Exports[0]
	if got.Network != "tako_backend_api_production_api_export" ||
		got.Project != "backend-api" ||
		got.Environment != "production" ||
		got.Service != "api" ||
		got.Alias != "backend-api-production-api" ||
		got.Runtime != "takod" {
		t.Fatalf("unexpected export record: %#v", got)
	}
}

func TestExportDiscoveryRecordFromLabelsSkipsMalformedRecords(t *testing.T) {
	labels := map[string]string{
		"tako.discovery":    "export",
		"tako.project":      "bad/project",
		"tako.environment":  "production",
		"tako.service":      "api",
		"tako.export.alias": "backend-api-production-api",
	}

	if _, ok := exportDiscoveryRecordFromLabels("tako_backend_api_production_api_export", labels); ok {
		t.Fatal("malformed discovery labels should be skipped")
	}
}

func TestHandleDiscoveryExportsRejectsInvalidEnvironment(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	req := httptest.NewRequest(http.MethodGet, "/v1/discovery/exports?environment=prod%0Abad", nil)
	recorder := httptest.NewRecorder()

	server.handleDiscoveryExports(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "invalid environment name") {
		t.Fatalf("unexpected response: %q", recorder.Body.String())
	}
}
