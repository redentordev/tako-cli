package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

func TestExitCodeForErrorTaxonomy(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"plain failure", errors.New("boom"), 1},
		{"invalid request", &engine.InvalidRequestError{Err: errors.New("bad flag")}, 2},
		{"confirmation required", &engine.ConfirmationRequiredError{Reason: "destructive"}, 2},
		{"locked", &engine.LockedError{Operation: "deploy", Err: errors.New("held")}, 3},
		{"connectivity", &engine.ConnectivityError{Server: "node-a", Err: errors.New("refused")}, 4},
		{"cancelled", context.Canceled, 5},
		{"attention", &engine.AttentionError{Err: errors.New("domains pending")}, 6},
		{"wrapped locked", fmt.Errorf("outer: %w", &engine.LockedError{Err: errors.New("held")}), 3},
	}
	for _, tc := range cases {
		if got := exitCodeForError(tc.err); got != tc.want {
			t.Errorf("%s: exitCodeForError = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestValidateMachineOutputFlags(t *testing.T) {
	restoreOutput, restoreEvents := outputFormatFlag, eventsFormatFlag
	t.Cleanup(func() { outputFormatFlag, eventsFormatFlag = restoreOutput, restoreEvents })

	outputFormatFlag, eventsFormatFlag = outputFormatText, ""
	if err := validateMachineOutputFlags(); err != nil {
		t.Fatalf("text mode returned error: %v", err)
	}
	outputFormatFlag = "yaml"
	if err := validateMachineOutputFlags(); engine.Classify(err) != engine.ClassInvalid {
		t.Fatalf("invalid --output classified as %d, want ClassInvalid", engine.Classify(err))
	}
	outputFormatFlag, eventsFormatFlag = outputFormatJSON, "sse"
	if err := validateMachineOutputFlags(); engine.Classify(err) != engine.ClassInvalid {
		t.Fatalf("invalid --events classified as %d, want ClassInvalid", engine.Classify(err))
	}
}

func sampleDeployPlan() engine.DeployPlan {
	return engine.DeployPlan{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindDeployPlan,
		Project:     "demo",
		Environment: "production",
		Revision:    "abc123",
		Source:      "deploy",
		Servers:     []string{"node-a"},
		Services:    []string{"web"},
		Changes: []engine.PlanChange{
			{Type: "update", Service: "web", Reasons: []string{"Image changed"}},
		},
		Destructive: true,
		HumanText:   "plan table",
	}
}

// TestDeployPlanDocumentGolden pins the machine-facing plan schema; a failure
// here means a breaking (non-additive) change to the plan document.
func TestDeployPlanDocumentGolden(t *testing.T) {
	payload, err := json.MarshalIndent(sampleDeployPlan(), "", "  ")
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "DeployPlan",
  "project": "demo",
  "environment": "production",
  "revision": "abc123",
  "source": "deploy",
  "servers": [
    "node-a"
  ],
  "services": [
    "web"
  ],
  "changes": [
    {
      "type": "update",
      "service": "web",
      "reasons": [
        "Image changed"
      ]
    }
  ],
  "destructive": true,
  "empty": false,
  "humanText": "plan table"
}`
	if string(payload) != want {
		t.Fatalf("plan document drifted:\n%s", payload)
	}
}

// TestDeployResultDocumentGolden pins the machine-facing result schema.
func TestDeployResultDocumentGolden(t *testing.T) {
	result := engine.DeployResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindDeployResult,
		Project:     "demo",
		Environment: "production",
		Status:      takoapi.StatusSuccess,
		Revision:    "abc123",
		Services: []engine.ServiceOutcome{
			{Name: "web", Image: "demo/web:abc123", Action: engine.OutcomeDeployed, Replicas: 2},
		},
		URLs:      []string{"https://app.example.com"},
		StartedAt: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Duration:  4.2,
		PlanHash:  "deadbeef",
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "DeployResult",
  "project": "demo",
  "environment": "production",
  "status": "success",
  "revision": "abc123",
  "services": [
    {
      "name": "web",
      "image": "demo/web:abc123",
      "action": "deployed",
      "replicas": 2
    }
  ],
  "urls": [
    "https://app.example.com"
  ],
  "startedAt": "2026-07-06T12:00:00Z",
  "durationSeconds": 4.2,
  "planHash": "deadbeef"
}`
	if string(payload) != want {
		t.Fatalf("result document drifted:\n%s", payload)
	}
}

func TestConfirmationRequiredDocumentShape(t *testing.T) {
	doc := newConfirmationRequiredDocument("deployment plan includes destructive changes", sampleDeployPlan())
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal confirmation doc: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if decoded["kind"] != "ConfirmationRequired" {
		t.Fatalf("kind = %v", decoded["kind"])
	}
	if decoded["reason"] != "deployment plan includes destructive changes" {
		t.Fatalf("reason = %v", decoded["reason"])
	}
	if _, ok := decoded["plan"].(map[string]any); !ok {
		t.Fatalf("plan missing from document: %s", payload)
	}
}

func TestVerifyPlanFileMatchesDetectsDrift(t *testing.T) {
	plan := sampleDeployPlan()
	planPath := filepath.Join(t.TempDir(), "plan.json")
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if err := os.WriteFile(planPath, payload, 0o600); err != nil {
		t.Fatalf("write plan file: %v", err)
	}

	if err := verifyPlanFileMatches(planPath, plan); err != nil {
		t.Fatalf("matching plan rejected: %v", err)
	}

	drifted := plan
	drifted.Revision = "def456"
	err = verifyPlanFileMatches(planPath, drifted)
	if err == nil || !strings.Contains(err.Error(), "plan drift detected") {
		t.Fatalf("drift not detected: %v", err)
	}
	if engine.Classify(err) != engine.ClassInvalid {
		t.Fatalf("drift classified as %d, want ClassInvalid", engine.Classify(err))
	}
}

// TestEventJSONGolden pins the NDJSON event schema emitted in --events mode.
func TestEventJSONGolden(t *testing.T) {
	stream := events.NewStream(&goldenSink{t: t}, nil)
	stream.SetNowFunc(func() time.Time { return time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC) })
	stream.Emit(events.Event{
		Type:    events.TypeDeployServiceReconciled,
		Phase:   events.PhaseDeploy,
		Service: "web",
		Message: "  ✓ Service web reconciled by takod\n",
		Data:    map[string]any{"image": "demo/web:abc123"},
	})
}

type goldenSink struct {
	t *testing.T
}

func (s *goldenSink) Emit(event events.Event) {
	payload, err := json.Marshal(event)
	if err != nil {
		s.t.Fatalf("marshal event: %v", err)
	}
	want := `{"apiVersion":"tako.redentor.dev/v1alpha1","kind":"Event","seq":1,"time":"2026-07-06T12:00:00Z","type":"deploy.service.reconciled","phase":"deploy","level":"info","service":"web","message":"  ✓ Service web reconciled by takod\n","data":{"image":"demo/web:abc123"}}`
	if string(payload) != want {
		s.t.Fatalf("event schema drifted:\n%s", payload)
	}
}
