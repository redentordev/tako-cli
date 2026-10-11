package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takoapi"
)

// Results without hooks must serialize exactly as before the hooks change:
// no hooks, deploymentId, or (rollback/promote) error keys.
func TestResultsWithoutHooksOmitHookFields(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	documents := map[string]any{
		"deploy":   DeployResult{APIVersion: takoapi.APIVersionCurrent, Kind: KindDeployResult, Project: "demo", Environment: "production", Status: "success", Services: []ServiceOutcome{{Name: "web", Action: OutcomeDeployed}}, StartedAt: started},
		"rollback": RollbackResult{APIVersion: takoapi.APIVersionCurrent, Kind: KindRollbackResult, Project: "demo", Environment: "production", Scope: RollbackScopeService, Service: "web", DeploymentID: "d-1", Status: "rolled_back", StartedAt: started},
		"promote":  PromoteResult{APIVersion: takoapi.APIVersionCurrent, Kind: KindPromoteResult, Project: "demo", Environment: "production", Service: "web", Revision: "r1", Status: "success", StartedAt: started},
		"setup":    SetupResult{APIVersion: takoapi.APIVersionCurrent, Kind: KindSetupResult, Project: "demo", Environment: "production", Nodes: []SetupNodeResult{}},
	}
	want := map[string]string{
		"deploy":   `{"apiVersion":"tako.redentor.dev/v1alpha1","kind":"DeployResult","project":"demo","environment":"production","status":"success","services":[{"name":"web","action":"deployed"}],"startedAt":"2026-01-02T03:04:05Z","durationSeconds":0}`,
		"rollback": `{"apiVersion":"tako.redentor.dev/v1alpha1","kind":"RollbackResult","project":"demo","environment":"production","scope":"service","service":"web","deploymentId":"d-1","status":"rolled_back","startedAt":"2026-01-02T03:04:05Z","durationSeconds":0}`,
		"promote":  `{"apiVersion":"tako.redentor.dev/v1alpha1","kind":"PromoteResult","project":"demo","environment":"production","service":"web","revision":"r1","status":"success","startedAt":"2026-01-02T03:04:05Z","durationSeconds":0}`,
		"setup":    `{"apiVersion":"tako.redentor.dev/v1alpha1","kind":"SetupResult","project":"demo","environment":"production","nodes":[]}`,
	}
	for name, document := range documents {
		payload, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if string(payload) != want[name] {
			t.Fatalf("%s:\n got %s\nwant %s", name, payload, want[name])
		}
	}
}

func TestDeployResultHookFields(t *testing.T) {
	zero, one := 0, 1
	var fast, slow int64 = 0, 1500
	payload, err := json.Marshal(DeployResult{
		Status:       "success",
		DeploymentID: "1700000000_42",
		Hooks: []HookOutcome{
			{Name: "pre-deploy", Status: HookCompleted, ExitCode: &zero, DurationMs: &fast},
			{Name: "post-deploy", Status: HookFailed, ExitCode: &one, DurationMs: &slow},
			{Name: "on-failure", Status: HookSkipped},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"deploymentId":"1700000000_42"`,
		`"hooks":[{"name":"pre-deploy","status":"completed","exitCode":0,"durationMs":0},{"name":"post-deploy","status":"failed","exitCode":1,"durationMs":1500},{"name":"on-failure","status":"skipped"}]`,
	} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("payload %s missing %s", payload, want)
		}
	}
}

func TestClassifyHookErrors(t *testing.T) {
	three := 3
	hookExit := &HookFailedError{Hook: "pre-deploy", ExitCode: &three, Err: errors.New("hook pre-deploy exited with status 3")}
	cases := []struct {
		name string
		err  error
		want Class
	}{
		{"pre hook exit", hookExit, ClassFailed},
		{"pre hook timeout", &HookFailedError{Hook: "pre-deploy", TimedOut: true, Err: errors.New("hook pre-deploy timed out after 2s")}, ClassFailed},
		{"pre hook cancelled", &HookFailedError{Hook: "pre-deploy", Err: fmt.Errorf("hook pre-deploy interrupted: %w", context.Canceled)}, ClassCancelled},
		{"wrapped pre hook", fmt.Errorf("deploy aborted: %w", hookExit), ClassFailed},
		{"post hook attention", &AttentionError{Err: errors.New("hook post-deploy exited with status 1")}, ClassAttention},
		{"post hook attention wrapping hook error", &AttentionError{Err: hookExit}, ClassAttention},
	}
	for _, tc := range cases {
		if got := Classify(tc.err); got != tc.want {
			t.Fatalf("%s: Classify = %d, want %d", tc.name, got, tc.want)
		}
	}
	if !strings.Contains(hookExit.Error(), "pre-deploy") || errors.Unwrap(hookExit) == nil {
		t.Fatalf("hook error = %v", hookExit)
	}
}

func TestCallBeforeMutation(t *testing.T) {
	info := MutationInfo{Operation: "promote", Services: []string{"web"}, Revision: "r1"}
	if err := callBeforeMutation(context.Background(), nil, info); err != nil {
		t.Fatalf("nil callback returned %v", err)
	}
	var seen MutationInfo
	blocked := errors.New("blocked by pre-promote")
	err := callBeforeMutation(context.Background(), func(_ context.Context, got MutationInfo) error {
		seen = got
		return blocked
	}, info)
	if !errors.Is(err, blocked) || seen.Revision != "r1" || seen.Operation != "promote" {
		t.Fatalf("err = %v seen = %+v", err, seen)
	}
}

// Rollback and promote fail before reaching BeforeMutation when the request
// is rejected up front; the callback must not run for those failures.
func TestBeforeMutationNotCalledForEarlyRejections(t *testing.T) {
	called := false
	callback := func(context.Context, MutationInfo) error {
		called = true
		return nil
	}
	if _, err := New(Options{}).Rollback(context.Background(), RollbackRequest{Environment: "production", Service: "web", BeforeMutation: callback}); err == nil {
		t.Fatal("rollback without config succeeded")
	}
	if _, err := New(Options{}).Promote(context.Background(), PromoteRequest{Environment: "production", ServiceName: "web", BeforeMutation: callback}); err == nil {
		t.Fatal("promote without config succeeded")
	}
	if called {
		t.Fatal("BeforeMutation ran for a rejected request")
	}
}
