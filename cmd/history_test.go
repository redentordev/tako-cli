package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/internal/state"
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
