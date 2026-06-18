package cmd

import (
	"strings"
	"testing"
)

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
