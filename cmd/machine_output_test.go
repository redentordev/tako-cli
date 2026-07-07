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
	"github.com/redentordev/tako-cli/pkg/takod"
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

// TestRollbackResultDocumentGolden pins the machine-facing rollback schema.
func TestRollbackResultDocumentGolden(t *testing.T) {
	result := engine.RollbackResult{
		APIVersion:   takoapi.APIVersionCurrent,
		Kind:         engine.KindRollbackResult,
		Project:      "demo",
		Environment:  "production",
		Service:      "web",
		DeploymentID: "deploy-123",
		Version:      "abc123",
		Status:       takoapi.StatusSuccess,
		StartedAt:    time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Duration:     3.5,
		Message:      "rolled back web to deploy-123",
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "RollbackResult",
  "project": "demo",
  "environment": "production",
  "service": "web",
  "deploymentId": "deploy-123",
  "version": "abc123",
  "status": "success",
  "startedAt": "2026-07-06T12:00:00Z",
  "durationSeconds": 3.5,
  "message": "rolled back web to deploy-123"
}`
	if string(payload) != want {
		t.Fatalf("rollback result document drifted:\n%s", payload)
	}
}

// TestPromoteResultDocumentGolden pins the machine-facing promote schema.
func TestPromoteResultDocumentGolden(t *testing.T) {
	result := engine.PromoteResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindPromoteResult,
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Revision:    "abc123",
		Image:       "demo/web:abc123",
		Status:      takoapi.StatusSuccess,
		StartedAt:   time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Duration:    2.1,
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "PromoteResult",
  "project": "demo",
  "environment": "production",
  "service": "web",
  "revision": "abc123",
  "image": "demo/web:abc123",
  "status": "success",
  "startedAt": "2026-07-06T12:00:00Z",
  "durationSeconds": 2.1
}`
	if string(payload) != want {
		t.Fatalf("promote result document drifted:\n%s", payload)
	}
}

// TestScaleResultDocumentGolden pins the machine-facing scale schema.
func TestScaleResultDocumentGolden(t *testing.T) {
	result := engine.ScaleResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindScaleResult,
		Project:     "demo",
		Environment: "production",
		Status:      takoapi.StatusSuccess,
		Services: []engine.ServiceOutcome{
			{Name: "web", Action: engine.OutcomeDeployed, Replicas: 3},
		},
		StartedAt: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Duration:  1.2,
		Message:   "scaled web=3",
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "ScaleResult",
  "project": "demo",
  "environment": "production",
  "status": "success",
  "services": [
    {
      "name": "web",
      "action": "deployed",
      "replicas": 3
    }
  ],
  "startedAt": "2026-07-06T12:00:00Z",
  "durationSeconds": 1.2,
  "message": "scaled web=3"
}`
	if string(payload) != want {
		t.Fatalf("scale result document drifted:\n%s", payload)
	}
}

// TestRemoveResultDocumentGolden pins the machine-facing remove schema.
func TestRemoveResultDocumentGolden(t *testing.T) {
	result := engine.RemoveResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindRemoveResult,
		Project:     "demo",
		Environment: "production",
		Scoped:      true,
		Servers: []engine.RemoveServerOutcome{
			{Name: "node-a", Host: "10.0.0.1", Removed: true},
		},
		StartedAt: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Duration:  6.7,
		Message:   "services removed from selected server(s) in environment production",
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "RemoveResult",
  "project": "demo",
  "environment": "production",
  "scoped": true,
  "servers": [
    {
      "name": "node-a",
      "host": "10.0.0.1",
      "removed": true
    }
  ],
  "startedAt": "2026-07-06T12:00:00Z",
  "durationSeconds": 6.7,
  "message": "services removed from selected server(s) in environment production"
}`
	if string(payload) != want {
		t.Fatalf("remove result document drifted:\n%s", payload)
	}
}

// TestDestroyResultDocumentGolden pins the machine-facing destroy schema.
func TestDestroyResultDocumentGolden(t *testing.T) {
	result := engine.DestroyResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindDestroyResult,
		Project:     "demo",
		Environment: "production",
		Mode:        engine.DestroyModePurge,
		PurgeAll:    true,
		Servers: []engine.DestroyServerOutcome{
			{Name: "node-a", Host: "10.0.0.1", Destroyed: true},
		},
		StartedAt: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Duration:  8.9,
		Message:   "app-owned leftovers pruned; shared server setup preserved",
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "DestroyResult",
  "project": "demo",
  "environment": "production",
  "mode": "PURGE",
  "purgeAll": true,
  "servers": [
    {
      "name": "node-a",
      "host": "10.0.0.1",
      "destroyed": true
    }
  ],
  "startedAt": "2026-07-06T12:00:00Z",
  "durationSeconds": 8.9,
  "message": "app-owned leftovers pruned; shared server setup preserved"
}`
	if string(payload) != want {
		t.Fatalf("destroy result document drifted:\n%s", payload)
	}
}

// TestValidateResultDocumentGolden pins the machine-facing validate schema.
func TestValidateResultDocumentGolden(t *testing.T) {
	result := engine.ValidateResult{
		APIVersion:      takoapi.APIVersionCurrent,
		Kind:            engine.KindValidateResult,
		ConfigPath:      "tako.yaml",
		Project:         "demo",
		Environment:     "production",
		Valid:           true,
		Runtime:         "takod",
		StateBackend:    "replicated",
		Consistency:     "lease",
		MeshEnabled:     true,
		MeshNetworkCIDR: "10.210.0.0/16",
		MeshInterface:   "tako",
		Servers:         1,
		Services:        1,
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "ValidateResult",
  "configPath": "tako.yaml",
  "project": "demo",
  "environment": "production",
  "valid": true,
  "runtime": "takod",
  "stateBackend": "replicated",
  "consistency": "lease",
  "meshEnabled": true,
  "meshNetworkCIDR": "10.210.0.0/16",
  "meshInterface": "tako",
  "servers": 1,
  "services": 1
}`
	if string(payload) != want {
		t.Fatalf("validate result document drifted:\n%s", payload)
	}
}

// TestValidateResultFindingGolden pins the finding schema on the invalid path.
func TestValidateResultFindingGolden(t *testing.T) {
	result := engine.ValidateResult{
		APIVersion: takoapi.APIVersionCurrent,
		Kind:       engine.KindValidateResult,
		ConfigPath: "tako.yaml",
		Valid:      false,
		Findings: []engine.ValidateFinding{
			{Severity: engine.ValidateSeverityError, Path: "tako.yaml", Message: "config validation failed"},
		},
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "ValidateResult",
  "configPath": "tako.yaml",
  "valid": false,
  "findings": [
    {
      "severity": "error",
      "path": "tako.yaml",
      "message": "config validation failed"
    }
  ]
}`
	if string(payload) != want {
		t.Fatalf("validate finding document drifted:\n%s", payload)
	}
}

// TestDoctorResultDocumentGolden pins the machine-facing doctor schema.
func TestDoctorResultDocumentGolden(t *testing.T) {
	result := engine.DoctorResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindDoctorResult,
		Project:     "demo",
		Environment: "production",
		SkipRemote:  true,
		Status:      "attention",
		Checks: []engine.DoctorCheck{
			{Name: "Configuration", Status: engine.DoctorStatusPass, Detail: "Config file: Found tako.yaml"},
			{Name: "SSH Keys", Status: engine.DoctorStatusFail, Detail: "node-a: SSH key not found: /tmp/id", Remediation: "Check key path or copy key to this machine"},
		},
		Passed: 1,
		Warned: 0,
		Failed: 1,
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "DoctorResult",
  "project": "demo",
  "environment": "production",
  "skipRemote": true,
  "status": "attention",
  "checks": [
    {
      "name": "Configuration",
      "status": "pass",
      "detail": "Config file: Found tako.yaml"
    },
    {
      "name": "SSH Keys",
      "status": "fail",
      "detail": "node-a: SSH key not found: /tmp/id",
      "remediation": "Check key path or copy key to this machine"
    }
  ],
  "passed": 1,
  "warned": 0,
  "failed": 1
}`
	if string(payload) != want {
		t.Fatalf("doctor result document drifted:\n%s", payload)
	}
}

// TestDriftResultDocumentGolden pins the machine-facing drift schema.
func TestDriftResultDocumentGolden(t *testing.T) {
	result := engine.DriftResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindDriftResult,
		Project:     "demo",
		Environment: "production",
		Drifted:     true,
		Drifts: []engine.DriftEntry{
			{Service: "web", Type: "replica_count", Severity: "high", Expected: "3 replicas", Actual: "1 replicas"},
		},
		ServicesOK: []string{"api"},
		CheckedAt:  time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Duration:   0.8,
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "DriftResult",
  "project": "demo",
  "environment": "production",
  "drifted": true,
  "drifts": [
    {
      "service": "web",
      "type": "replica_count",
      "severity": "high",
      "expected": "3 replicas",
      "actual": "1 replicas"
    }
  ],
  "servicesOk": [
    "api"
  ],
  "checkedAt": "2026-07-06T12:00:00Z",
  "durationSeconds": 0.8
}`
	if string(payload) != want {
		t.Fatalf("drift result document drifted:\n%s", payload)
	}
}

// TestMetricsResultDocumentGolden pins the machine-facing metrics schema.
// The per-node `metrics` payload is the takod /v1/metrics document verbatim
// (monitoring-agent schema) and is intentionally not repinned field-by-field.
func TestMetricsResultDocumentGolden(t *testing.T) {
	result := engine.MetricsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindMetricsResult,
		Project:     "demo",
		Environment: "production",
		CollectedAt: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Nodes: []engine.MetricsNodeSample{
			{Server: "node-a", Host: "10.0.0.1", Metrics: json.RawMessage(`{"cpu_percent":"12.5"}`)},
			{Server: "node-b", Host: "10.0.0.2", Error: "connect: dial tcp: refused"},
		},
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "MetricsResult",
  "project": "demo",
  "environment": "production",
  "collectedAt": "2026-07-06T12:00:00Z",
  "nodes": [
    {
      "server": "node-a",
      "host": "10.0.0.1",
      "metrics": {
        "cpu_percent": "12.5"
      }
    },
    {
      "server": "node-b",
      "host": "10.0.0.2",
      "error": "connect: dial tcp: refused"
    }
  ]
}`
	if string(payload) != want {
		t.Fatalf("metrics result document drifted:\n%s", payload)
	}
}

// TestStatsResultDocumentGolden pins the machine-facing stats schema.
func TestStatsResultDocumentGolden(t *testing.T) {
	result := engine.StatsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindStatsResult,
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		CollectedAt: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		Nodes: []engine.StatsNodeSample{
			{
				Server: "node-a",
				Host:   "10.0.0.1",
				Containers: []takod.ContainerStat{
					{Name: "demo-production-web-1", CPUPercent: "1.2%", MemUsage: "64MiB / 1GiB", MemPercent: "6.4%", NetIO: "1kB / 2kB", BlockIO: "0B / 0B", PIDs: "4"},
				},
			},
			{Server: "node-b", Host: "10.0.0.2", Error: "stats: takod unreachable"},
		},
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "StatsResult",
  "project": "demo",
  "environment": "production",
  "service": "web",
  "collectedAt": "2026-07-06T12:00:00Z",
  "nodes": [
    {
      "server": "node-a",
      "host": "10.0.0.1",
      "containers": [
        {
          "name": "demo-production-web-1",
          "cpuPercent": "1.2%",
          "memUsage": "64MiB / 1GiB",
          "memPercent": "6.4%",
          "netIO": "1kB / 2kB",
          "blockIO": "0B / 0B",
          "pids": "4"
        }
      ]
    },
    {
      "server": "node-b",
      "host": "10.0.0.2",
      "error": "stats: takod unreachable"
    }
  ]
}`
	if string(payload) != want {
		t.Fatalf("stats result document drifted:\n%s", payload)
	}
}

// TestSecretsResultDocumentsGolden pins the machine-facing secrets schemas.
// These documents carry secret KEYS only — never values.
func TestSecretsResultDocumentsGolden(t *testing.T) {
	list := engine.SecretsListResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindSecretsListResult,
		Environment: "production",
		Keys:        []string{"API_KEY", "DATABASE_URL"},
		Count:       2,
	}
	payload, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		t.Fatalf("marshal list result: %v", err)
	}
	wantList := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "SecretsListResult",
  "environment": "production",
  "keys": [
    "API_KEY",
    "DATABASE_URL"
  ],
  "count": 2
}`
	if string(payload) != wantList {
		t.Fatalf("secrets list document drifted:\n%s", payload)
	}

	validate := engine.SecretsValidateResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindSecretsValidateResult,
		Project:     "demo",
		Environment: "production",
		Valid:       false,
		Required:    []string{"API_KEY", "DATABASE_URL"},
		Missing:     []string{"DATABASE_URL"},
	}
	payload, err = json.MarshalIndent(validate, "", "  ")
	if err != nil {
		t.Fatalf("marshal validate result: %v", err)
	}
	wantValidate := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "SecretsValidateResult",
  "project": "demo",
  "environment": "production",
  "valid": false,
  "required": [
    "API_KEY",
    "DATABASE_URL"
  ],
  "missing": [
    "DATABASE_URL"
  ]
}`
	if string(payload) != wantValidate {
		t.Fatalf("secrets validate document drifted:\n%s", payload)
	}
}

func TestOperationConfirmationRequiredDocumentShape(t *testing.T) {
	doc := newOperationConfirmationRequiredDocument(
		"remove deletes all deployed services for this project from the environment",
		"remove", "demo", "production", []string{"node-a", "node-b"},
	)
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
	if decoded["operation"] != "remove" {
		t.Fatalf("operation = %v", decoded["operation"])
	}
	if decoded["project"] != "demo" || decoded["environment"] != "production" {
		t.Fatalf("identity fields wrong: %s", payload)
	}
	servers, ok := decoded["servers"].([]any)
	if !ok || len(servers) != 2 {
		t.Fatalf("servers missing from document: %s", payload)
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
