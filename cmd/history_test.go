package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/engine"
)

func TestFormatStatus(t *testing.T) {
	tests := []struct {
		status state.DeploymentStatus
		want   string
	}{
		{status: state.StatusSuccess, want: "✓ success"},
		{status: state.StatusWarmed, want: "◌ warmed"},
		{status: state.StatusFailed, want: "✗ failed"},
		{status: state.StatusRolledBack, want: "↺ rolled_back"},
		{status: state.StatusInProgress, want: "⋯ in_progress"},
		{status: state.DeploymentStatus("custom"), want: "custom"},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := formatStatus(tt.status); got != tt.want {
				t.Fatalf("formatStatus(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestRenderHistoryResultMachineJSONKeepsStdoutParseable(t *testing.T) {
	withMachineOutput(t, outputFormatJSON, "", func() {
		result := &engine.HistoryResult{
			APIVersion:    "tako.redentor.dev/v1alpha1",
			Kind:          engine.KindHistoryResult,
			Project:       "demo",
			Environment:   "production",
			SourceServer:  "node-a",
			Limit:         10,
			IncludeFailed: true,
			Deployments: []engine.HistoryDeployment{{
				ID:        "deployment-1234567890",
				DisplayID: "deployment",
				Commit:    "abc1234",
				Timestamp: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
				Version:   "v1",
				Status:    state.StatusSuccess,
				Duration:  "1.0s",
				Message:   "deploy web",
			}},
		}
		stdout := captureConfigExportStdout(t, func() {
			if err := renderHistoryResult(result); err != nil {
				t.Fatalf("renderHistoryResult returned error: %v", err)
			}
		})
		if strings.Contains(stdout, "Deployment History") || strings.Contains(stdout, "No deployments found") || strings.Contains(stdout, "To rollback") {
			t.Fatalf("machine stdout contains human table/prose: %q", stdout)
		}
		var decoded engine.HistoryResult
		if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
			t.Fatalf("stdout is not parseable history JSON: %v\n%s", err, stdout)
		}
		if decoded.Kind != engine.KindHistoryResult || len(decoded.Deployments) != 1 || decoded.Deployments[0].ID != "deployment-1234567890" {
			t.Fatalf("decoded history result = %#v", decoded)
		}
	})
}

func TestHistoryNextStepsReferenceCurrentCommands(t *testing.T) {
	output := historyNextSteps()
	for _, want := range []string{
		"tako rollback <deployment-id>",
		"tako logs --service <service> --tail 200",
		"tako access <service>",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("history next steps = %q, want %q", output, want)
		}
	}
	for _, stale := range []string{"coming" + " " + "soon", "logs" + " " + "show"} {
		if strings.Contains(output, stale) {
			t.Fatalf("history next steps still include stale guidance %q: %q", stale, output)
		}
	}
}
