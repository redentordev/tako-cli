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

// TestStatusResultDocumentGolden pins the machine-facing ps schema,
// including the dashboard fields (image, strategy, health, per-node
// placement breakdown). health and nodes are empty when the node agent
// predates health capture.
func TestStatusResultDocumentGolden(t *testing.T) {
	result := engine.StatusResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindStatusResult,
		Project:     "demo",
		Environment: "production",
		Servers:     []string{"node-a", "node-b"},
		Services: []engine.StatusService{
			{
				Name:     "web",
				Desired:  2,
				Running:  2,
				Status:   "running",
				Ports:    "8080-8081",
				Revision: "abcdef123456",
				Warming:  1,
				Internal: false,
				Image:    "demo/web:abcdef123456",
				Strategy: "rolling",
				Health:   takod.HealthStateHealthy,
				Nodes: []engine.StatusServiceNode{
					{Name: "node-a", Running: 1, Warming: 1, Health: takod.HealthStateHealthy},
					{Name: "node-b", Running: 1},
				},
			},
			{
				Name:     "reporter",
				Status:   "scheduled",
				Ports:    "-",
				Internal: true,
				Kind:     "job",
				Schedule: "0 3 * * *",
				LastRun:  "success",
			},
		},
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "StatusResult",
  "project": "demo",
  "environment": "production",
  "servers": [
    "node-a",
    "node-b"
  ],
  "services": [
    {
      "name": "web",
      "desired": 2,
      "running": 2,
      "status": "running",
      "ports": "8080-8081",
      "revision": "abcdef123456",
      "warming": 1,
      "internal": false,
      "image": "demo/web:abcdef123456",
      "strategy": "rolling",
      "health": "healthy",
      "nodes": [
        {
          "name": "node-a",
          "running": 1,
          "warming": 1,
          "health": "healthy"
        },
        {
          "name": "node-b",
          "running": 1
        }
      ]
    },
    {
      "name": "reporter",
      "desired": 0,
      "running": 0,
      "status": "scheduled",
      "ports": "-",
      "internal": true,
      "kind": "job",
      "schedule": "0 3 * * *",
      "lastRun": "success"
    }
  ]
}`
	if string(payload) != want {
		t.Fatalf("status result document drifted:\n%s", payload)
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

// TestDomainsResultDocumentsGolden pins the machine-facing domains schemas.
func TestDomainsResultDocumentsGolden(t *testing.T) {
	status := engine.DomainsResult{
		APIVersion:      takoapi.APIVersionCurrent,
		Kind:            engine.KindDomainsResult,
		Project:         "demo",
		Environment:     "production",
		ExpectedTargets: []string{"203.0.113.10"},
		AllActive:       false,
		Domains: []engine.DomainStatusEntry{
			{Service: "web", Domain: "app.example.com", Role: "serving", State: "active", DNS: "proxied", TLS: "active", ResolvedIPs: []string{"198.51.100.20"}, Warning: "access controls are configured behind a suspected proxy/CDN without proxy.trustedProxies"},
			{Service: "web", Domain: "www.example.com", Role: "redirect", State: "pending_dns", DNS: "unresolved", TLS: "unknown", DNSError: "no such host"},
		},
	}
	payload, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		t.Fatalf("marshal domains result: %v", err)
	}
	wantStatus := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "DomainsResult",
  "project": "demo",
  "environment": "production",
  "expectedTargets": [
    "203.0.113.10"
  ],
  "allActive": false,
  "domains": [
    {
      "service": "web",
      "domain": "app.example.com",
      "role": "serving",
      "state": "active",
      "dns": "proxied",
      "tls": "active",
      "resolvedIps": [
        "198.51.100.20"
      ],
      "warning": "access controls are configured behind a suspected proxy/CDN without proxy.trustedProxies"
    },
    {
      "service": "web",
      "domain": "www.example.com",
      "role": "redirect",
      "state": "pending_dns",
      "dns": "unresolved",
      "tls": "unknown",
      "dnsError": "no such host"
    }
  ]
}`
	if string(payload) != wantStatus {
		t.Fatalf("domains status document drifted:\n%s", payload)
	}

	hosts := engine.DomainsHostsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindDomainsHostsResult,
		Project:     "demo",
		Environment: "production",
		AddressMode: "auto",
		Entries: []engine.InternalHostEntry{
			{Service: "api", Host: "api.internal", Address: "10.210.0.1", Server: "node-a", Source: "mesh"},
		},
	}
	payload, err = json.MarshalIndent(hosts, "", "  ")
	if err != nil {
		t.Fatalf("marshal hosts result: %v", err)
	}
	wantHosts := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "DomainsHostsResult",
  "project": "demo",
  "environment": "production",
  "addressMode": "auto",
  "entries": [
    {
      "service": "api",
      "host": "api.internal",
      "address": "10.210.0.1",
      "server": "node-a",
      "source": "mesh"
    }
  ]
}`
	if string(payload) != wantHosts {
		t.Fatalf("domains hosts document drifted:\n%s", payload)
	}
}

func TestCertsResultDocumentGoldenExcludesPrivateMaterial(t *testing.T) {
	started := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	expires := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	result := engine.CertsResult{
		APIVersion: takoapi.APIVersionCurrent,
		Kind:       engine.KindCertsResult,
		Project:    "demo", Environment: "production", Action: "list",
		Nodes: []engine.CertsNodeResult{{
			Server: "node-a", Host: "203.0.113.10",
			Certificates: []takod.ProxyCertificateMetadata{{Domain: "*.example.com", Source: takod.CertificateSourcePushed, NotAfter: expires}},
		}},
		StartedAt: started, Duration: 0.25,
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "CertsResult",
  "project": "demo",
  "environment": "production",
  "action": "list",
  "nodes": [
    {
      "server": "node-a",
      "host": "203.0.113.10",
      "certificates": [
        {
          "domain": "*.example.com",
          "source": "pushed",
          "notBefore": "0001-01-01T00:00:00Z",
          "notAfter": "2026-10-09T12:00:00Z",
          "issuedAt": "0001-01-01T00:00:00Z",
          "updatedAt": "0001-01-01T00:00:00Z"
        }
      ]
    }
  ],
  "startedAt": "2026-07-11T12:00:00Z",
  "durationSeconds": 0.25
}`
	if string(payload) != want {
		t.Fatalf("certs result document drifted:\n%s", payload)
	}
	if strings.Contains(string(payload), "PRIVATE KEY") || strings.Contains(string(payload), "certPem") || strings.Contains(string(payload), "keyPem") {
		t.Fatal("CertsResult leaked private key material")
	}
}

// TestDiscoveryExportsResultDocumentGolden pins the machine-facing discovery schema.
func TestDiscoveryExportsResultDocumentGolden(t *testing.T) {
	result := engine.DiscoveryExportsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindDiscoveryExportsResult,
		Environment: "production",
		Nodes: []engine.DiscoveryNodeExports{
			{
				Server: "node-a",
				Host:   "10.0.0.1",
				Exports: []takod.ExportDiscoveryRecord{{
					Network:     "tako_backend_api_production_api_export",
					Project:     "backend-api",
					Environment: "production",
					Service:     "api",
					Alias:       "backend-api-production-api",
				}},
			},
			{Server: "node-b", Host: "10.0.0.2", Error: "connect: refused"},
		},
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "DiscoveryExportsResult",
  "environment": "production",
  "nodes": [
    {
      "server": "node-a",
      "host": "10.0.0.1",
      "exports": [
        {
          "network": "tako_backend_api_production_api_export",
          "project": "backend-api",
          "environment": "production",
          "service": "api",
          "alias": "backend-api-production-api"
        }
      ]
    },
    {
      "server": "node-b",
      "host": "10.0.0.2",
      "error": "connect: refused"
    }
  ]
}`
	if string(payload) != want {
		t.Fatalf("discovery exports document drifted:\n%s", payload)
	}
}

// TestActionResultDocumentGolden pins the machine-facing ack schema used by
// maintenance, live, and cleanup.
func TestActionResultDocumentGolden(t *testing.T) {
	result := engine.ActionResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindActionResult,
		Project:     "demo",
		Environment: "production",
		Action:      engine.ActionMaintenanceEnable,
		Service:     "web",
		Outcome:     engine.ActionOutcomePartial,
		Servers: []engine.ActionNodeOutcome{
			{Server: "node-a", Host: "10.0.0.1", Done: true},
			{Server: "node-b", Host: "10.0.0.2", Done: false, Error: "connect: refused"},
		},
		Error: "maintenance mode failed on 1/2 node(s): node-b: connect: refused",
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "ActionResult",
  "project": "demo",
  "environment": "production",
  "action": "maintenance.enable",
  "service": "web",
  "outcome": "partial",
  "servers": [
    {
      "server": "node-a",
      "host": "10.0.0.1",
      "done": true
    },
    {
      "server": "node-b",
      "host": "10.0.0.2",
      "done": false,
      "error": "connect: refused"
    }
  ],
  "error": "maintenance mode failed on 1/2 node(s): node-b: connect: refused"
}`
	if string(payload) != want {
		t.Fatalf("action result document drifted:\n%s", payload)
	}
}

// TestBackupResultDocumentGolden pins the machine-facing backup schema.
func TestBackupResultDocumentGolden(t *testing.T) {
	result := engine.BackupResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindBackupResult,
		Project:     "demo",
		Environment: "production",
		Action:      engine.BackupActionCreate,
		Volume:      "data",
		BackupID:    "20260706-120000",
		Nodes: []engine.BackupNodeOutcome{
			{
				Server: "node-a",
				Host:   "10.0.0.1",
				Backups: []takod.BackupInfo{
					{ID: "20260706-120000", Volume: "data", Size: 1024, CreatedAt: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC), Path: "/var/lib/tako/backups/data_20260706-120000.tar.gz", Compression: "gzip"},
				},
			},
			{Server: "node-b", Host: "10.0.0.2", Skipped: []string{"data: volume not present on node"}},
		},
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "BackupResult",
  "project": "demo",
  "environment": "production",
  "action": "create",
  "volume": "data",
  "backupId": "20260706-120000",
  "nodes": [
    {
      "server": "node-a",
      "host": "10.0.0.1",
      "backups": [
        {
          "id": "20260706-120000",
          "volume": "data",
          "size": 1024,
          "createdAt": "2026-07-06T12:00:00Z",
          "path": "/var/lib/tako/backups/data_20260706-120000.tar.gz",
          "compression": "gzip"
        }
      ]
    },
    {
      "server": "node-b",
      "host": "10.0.0.2",
      "skipped": [
        "data: volume not present on node"
      ]
    }
  ]
}`
	if string(payload) != want {
		t.Fatalf("backup result document drifted:\n%s", payload)
	}
}

// TestSetupResultDocumentGolden pins the machine-facing setup schema.
func TestSetupResultDocumentGolden(t *testing.T) {
	result := engine.SetupResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindSetupResult,
		Project:     "demo",
		Environment: "production",
		Nodes: []engine.SetupNodeResult{
			{
				Server:        "node-a",
				Host:          "10.0.0.1",
				Mode:          engine.SetupModeFresh,
				OS:            "Ubuntu 24.04 LTS",
				DockerVersion: "27.0.3",
				TakodVersion:  "1.2.3",
				SetupVersion:  "3",
				FirewallPorts: []string{"22/tcp", "80/tcp", "443/tcp", "443/udp", "51820/udp"},
				HostKey: &engine.SetupHostKey{
					Type:        "ssh-ed25519",
					Key:         "AAAAC3NzaC1lZDI1NTE5AAAAIP//////////////////////////////////////////",
					Fingerprint: "SHA256:HP0d5nqvfsCJyg2NPMRnyoRNS7RhBkkAn1V6HzsccAo",
				},
				Steps: []engine.SetupStepOutcome{
					{Step: engine.SetupStepOSCheck, Title: "Checking system requirements", Status: engine.SetupStepCompleted},
					{Step: engine.SetupStepTakodService, Title: "Configuring takod service", Status: engine.SetupStepCompleted},
				},
			},
			{
				Server: "node-b",
				Host:   "10.0.0.2",
				Mode:   engine.SetupModeConverge,
				Steps: []engine.SetupStepOutcome{
					{Step: engine.SetupStepOSCheck, Title: "Checking system requirements", Status: engine.SetupStepSkipped},
					{Step: engine.SetupStepFirewall, Title: "Configuring firewall (UFW)", Status: engine.SetupStepFailed, Error: "ufw not available"},
				},
				Error: "failed at step 'Configuring firewall (UFW)' on server node-b: ufw not available",
			},
		},
		Error: "failed at step 'Configuring firewall (UFW)' on server node-b: ufw not available",
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "SetupResult",
  "project": "demo",
  "environment": "production",
  "nodes": [
    {
      "server": "node-a",
      "host": "10.0.0.1",
      "mode": "fresh",
      "os": "Ubuntu 24.04 LTS",
      "dockerVersion": "27.0.3",
      "takodVersion": "1.2.3",
      "setupVersion": "3",
      "firewallPorts": [
        "22/tcp",
        "80/tcp",
        "443/tcp",
        "443/udp",
        "51820/udp"
      ],
      "hostKey": {
        "type": "ssh-ed25519",
        "key": "AAAAC3NzaC1lZDI1NTE5AAAAIP//////////////////////////////////////////",
        "fingerprint": "SHA256:HP0d5nqvfsCJyg2NPMRnyoRNS7RhBkkAn1V6HzsccAo"
      },
      "steps": [
        {
          "step": "os-check",
          "title": "Checking system requirements",
          "status": "completed"
        },
        {
          "step": "takod-service",
          "title": "Configuring takod service",
          "status": "completed"
        }
      ]
    },
    {
      "server": "node-b",
      "host": "10.0.0.2",
      "mode": "converge",
      "steps": [
        {
          "step": "os-check",
          "title": "Checking system requirements",
          "status": "skipped"
        },
        {
          "step": "firewall",
          "title": "Configuring firewall (UFW)",
          "status": "failed",
          "error": "ufw not available"
        }
      ],
      "error": "failed at step 'Configuring firewall (UFW)' on server node-b: ufw not available"
    }
  ],
  "error": "failed at step 'Configuring firewall (UFW)' on server node-b: ufw not available"
}`
	if string(payload) != want {
		t.Fatalf("setup result document drifted:\n%s", payload)
	}
}

// TestUpgradeServersResultDocumentGolden pins the machine-facing server
// agent upgrade schema.
func TestUpgradeServersResultDocumentGolden(t *testing.T) {
	result := engine.UpgradeServersResult{
		APIVersion:    takoapi.APIVersionCurrent,
		Kind:          engine.KindUpgradeServersResult,
		Project:       "demo",
		Environment:   "production",
		TargetVersion: "1.2.4",
		Nodes: []engine.UpgradeServersNodeOutcome{
			{Server: "node-a", Host: "10.0.0.1", FromVersion: "1.2.3", ToVersion: "1.2.4", Outcome: engine.UpgradeOutcomeUpgraded},
			{Server: "node-b", Host: "10.0.0.2", ToVersion: "1.2.4", Outcome: engine.UpgradeOutcomeFailed, Error: "server node-b is not set up; run 'tako setup --server node-b' first"},
		},
		Error: "server agent upgrade failed on 1 of 2 node(s)",
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "UpgradeServersResult",
  "project": "demo",
  "environment": "production",
  "targetVersion": "1.2.4",
  "nodes": [
    {
      "server": "node-a",
      "host": "10.0.0.1",
      "fromVersion": "1.2.3",
      "toVersion": "1.2.4",
      "outcome": "upgraded"
    },
    {
      "server": "node-b",
      "host": "10.0.0.2",
      "toVersion": "1.2.4",
      "outcome": "failed",
      "error": "server node-b is not set up; run 'tako setup --server node-b' first"
    }
  ],
  "error": "server agent upgrade failed on 1 of 2 node(s)"
}`
	if string(payload) != want {
		t.Fatalf("upgrade servers result document drifted:\n%s", payload)
	}
}

// TestExecResultDocumentGolden pins the machine-facing exec schema.
func TestExecResultDocumentGolden(t *testing.T) {
	result := engine.ExecResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindExecResult,
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Server:      "node-a",
		Host:        "10.0.0.1",
		Container:   "tako_demo_production_web_1",
		Mode:        "attach",
		Command:     []string{"env"},
		ExitCode:    0,
		DurationMs:  431,
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "ExecResult",
  "project": "demo",
  "environment": "production",
  "service": "web",
  "server": "node-a",
  "host": "10.0.0.1",
  "container": "tako_demo_production_web_1",
  "mode": "attach",
  "command": [
    "env"
  ],
  "exitCode": 0,
  "durationMs": 431
}`
	if string(payload) != want {
		t.Fatalf("exec result document drifted:\n%s", payload)
	}
}

// TestJobsResultDocumentGolden pins the machine-facing jobs list schema.
func TestJobsResultDocumentGolden(t *testing.T) {
	nextRun := time.Date(2026, 7, 6, 12, 5, 0, 0, time.UTC)
	started := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	finished := started.Add(3 * time.Second)
	result := engine.JobsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindJobsResult,
		Project:     "demo",
		Environment: "production",
		Jobs: []engine.JobInfo{
			{
				Name:           "report",
				Server:         "node-a",
				Schedule:       "*/5 * * * *",
				Timezone:       "Europe/Berlin",
				Image:          "demo-production-report:abc123",
				Command:        []string{"sh", "-c", "generate-report"},
				TimeoutSeconds: 1800,
				NextRun:        &nextRun,
				LastRun: &engine.JobRunInfo{
					Job:        "report",
					Server:     "node-a",
					Trigger:    "schedule",
					Container:  "tako_demo_production_report_job_1",
					StartedAt:  started,
					FinishedAt: finished,
					DurationMs: 3000,
					ExitCode:   0,
					Status:     "succeeded",
				},
			},
		},
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "JobsResult",
  "project": "demo",
  "environment": "production",
  "jobs": [
    {
      "name": "report",
      "server": "node-a",
      "schedule": "*/5 * * * *",
      "timezone": "Europe/Berlin",
      "image": "demo-production-report:abc123",
      "command": [
        "sh",
        "-c",
        "generate-report"
      ],
      "timeoutSeconds": 1800,
      "nextRun": "2026-07-06T12:05:00Z",
      "lastRun": {
        "job": "report",
        "server": "node-a",
        "trigger": "schedule",
        "container": "tako_demo_production_report_job_1",
        "startedAt": "2026-07-06T12:00:00Z",
        "finishedAt": "2026-07-06T12:00:03Z",
        "durationMs": 3000,
        "exitCode": 0,
        "status": "succeeded"
      }
    }
  ]
}`
	if string(payload) != want {
		t.Fatalf("jobs result document drifted:\n%s", payload)
	}
}

// TestJobRunsResultDocumentGolden pins the machine-facing run history schema.
func TestJobRunsResultDocumentGolden(t *testing.T) {
	started := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	result := engine.JobRunsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindJobRunsResult,
		Project:     "demo",
		Environment: "production",
		Job:         "report",
		Runs: []engine.JobRunInfo{
			{
				Job:        "report",
				Server:     "node-a",
				Trigger:    "manual",
				Container:  "tako_demo_production_report_job_2",
				StartedAt:  started,
				FinishedAt: started.Add(2 * time.Second),
				DurationMs: 2000,
				ExitCode:   1,
				Status:     "failed",
				Output:     "boom\n",
			},
		},
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "JobRunsResult",
  "project": "demo",
  "environment": "production",
  "job": "report",
  "runs": [
    {
      "job": "report",
      "server": "node-a",
      "trigger": "manual",
      "container": "tako_demo_production_report_job_2",
      "startedAt": "2026-07-06T12:00:00Z",
      "finishedAt": "2026-07-06T12:00:02Z",
      "durationMs": 2000,
      "exitCode": 1,
      "status": "failed",
      "output": "boom\n"
    }
  ]
}`
	if string(payload) != want {
		t.Fatalf("job runs result document drifted:\n%s", payload)
	}
}

// TestJobTriggerResultDocumentGolden pins the machine-facing trigger schema.
func TestJobTriggerResultDocumentGolden(t *testing.T) {
	result := engine.JobTriggerResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindJobTriggerResult,
		Project:     "demo",
		Environment: "production",
		Job:         "report",
		Server:      "node-a",
		Container:   "tako_demo_production_report_job_3",
		ExitCode:    0,
		DurationMs:  2150,
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "JobTriggerResult",
  "project": "demo",
  "environment": "production",
  "job": "report",
  "server": "node-a",
  "container": "tako_demo_production_report_job_3",
  "exitCode": 0,
  "durationMs": 2150
}`
	if string(payload) != want {
		t.Fatalf("job trigger result document drifted:\n%s", payload)
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
