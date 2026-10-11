package engine

import (
	"encoding/json"
	"strings"
	"testing"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

func previewSummaryInput() deploySummaryInput {
	services := map[string]config.ServiceConfig{
		"web": {Image: "web:new", Port: 80, Proxy: &config.ProxyConfig{Domain: "pr-42.preview.example.com"}},
		"api": {Image: "api:new", Port: 80, Proxy: &config.ProxyConfig{Visibility: "internal", Host: "api.pr-42.internal"}},
	}
	return deploySummaryInput{
		Project: "shop", Environment: "preview-pr-42", StartedAt: summaryTestStart,
		Services: services, ServicesToDeploy: services,
		Plan: &reconcile.ReconciliationPlan{Changes: []reconcile.ServiceChange{{ServiceName: "web", Type: reconcile.ChangeAdd}}},
		Outcomes: []ServiceOutcome{
			{Name: "web", Image: "web:new", Action: OutcomeWarmed},
			{Name: "api", Image: "api:new", Action: OutcomeDeployed},
		},
		DomainChecked:  true,
		DomainStatuses: []health.DomainStatus{{Domain: "pr-42.preview.example.com", State: health.DomainStatePendingTLS}},
		HealthRead:     true,
		Health:         map[string]serviceHealth{"api": {Desired: 1, Running: 0, Health: SummaryHealthUnhealthy}},
		Preview:        &SummaryPreview{Name: "pr-42", Base: "preview", ExpiresAt: "2026-10-18T14:02:00Z"},
	}
}

func summaryCommands(summary *OperationSummary) []string {
	var commands []string
	for _, step := range summary.NextSteps {
		commands = append(commands, step.Command)
	}
	if summary.Undo != nil && summary.Undo.Command != "" {
		commands = append(commands, summary.Undo.Command)
	}
	return commands
}

func TestPreviewDeploySummaryUsesPreviewSelector(t *testing.T) {
	summary := buildDeploySummary(identityRedact, previewSummaryInput())
	if summary.Environment.Kind != SummaryEnvironmentKindPreview || summary.Environment.Preview == nil || summary.Environment.Preview.Base != "preview" || summary.Environment.Name != "preview-pr-42" {
		t.Fatalf("environment = %+v", summary.Environment)
	}
	commands := summaryCommands(summary)
	if len(commands) == 0 {
		t.Fatal("expected next steps")
	}
	for _, command := range commands {
		if strings.Contains(command, "-e preview-") {
			t.Fatalf("command %q selects the preview with -e", command)
		}
		if strings.Contains(command, "tako promote") || strings.Contains(command, "domains status") || strings.Contains(command, "domains hosts") {
			t.Fatalf("command %q does not accept --preview", command)
		}
	}
	found := false
	for _, command := range commands {
		found = found || command == "tako ps --preview pr-42"
	}
	if !found {
		t.Fatalf("missing ps --preview step: %v", commands)
	}
	for _, url := range summary.URLs {
		if url.Kind != SummaryURLKindPublic && url.Kind != SummaryURLKindInternal {
			t.Fatalf("url kind %q is not public/internal", url.Kind)
		}
	}
	for _, note := range summary.Notes {
		if strings.Contains(note, "-e preview-") {
			t.Fatalf("note %q selects the preview with -e", note)
		}
	}
	payload, _ := json.Marshal(summary.Environment)
	if string(payload) != `{"name":"preview-pr-42","kind":"preview","preview":{"name":"pr-42","base":"preview","expiresAt":"2026-10-18T14:02:00Z"}}` {
		t.Fatalf("environment JSON = %s", payload)
	}
}

func TestPreviewDeployUndo(t *testing.T) {
	in := previewSummaryInput()
	in.Outcomes = []ServiceOutcome{{Name: "web", Image: "web:new", Action: OutcomeDeployed}, {Name: "api", Image: "api:new", Action: OutcomeDeployed}}
	first := buildDeploySummary(identityRedact, in)
	if first.Undo == nil || first.Undo.Command != "tako preview destroy pr-42" {
		t.Fatalf("first deploy undo = %+v", first.Undo)
	}

	in.PreviousStable = &remotestate.DeploymentState{ID: "1767225000123456789"}
	again := buildDeploySummary(identityRedact, in)
	if again.Undo == nil || again.Undo.Command != "tako rollback --preview pr-42 1767225000" {
		t.Fatalf("redeploy undo = %+v", again.Undo)
	}
}

func TestStandardSummaryKeepsEnvironmentSelector(t *testing.T) {
	in := previewSummaryInput()
	in.Environment = "staging"
	in.Preview = nil
	summary := buildDeploySummary(identityRedact, in)
	if summary.Environment.Kind != SummaryEnvironmentKindStandard || summary.Environment.Preview != nil {
		t.Fatalf("environment = %+v", summary.Environment)
	}
	found := false
	for _, command := range summaryCommands(summary) {
		found = found || strings.HasPrefix(command, "tako promote web -e staging")
	}
	if !found {
		t.Fatalf("standard summary lost its promote step: %v", summaryCommands(summary))
	}
}

func TestRegisteredPreviewEnvironmentAppliesToOperationSummaries(t *testing.T) {
	e := New(Options{})
	e.RegisterPreviewEnvironment("preview-pr-42", SummaryPreview{Name: "pr-42", Base: "preview"})
	summary := e.FailedBeforeMutationSummary(SummaryOperationRollback, "shop", "preview-pr-42", []string{"web"}, summaryTestStart, "boom")
	if summary.Environment.Kind != SummaryEnvironmentKindPreview {
		t.Fatalf("kind = %q", summary.Environment.Kind)
	}
	undo := undoForRollback(summary.Environment, SummaryOutcomeSucceeded, "", &remotestate.DeploymentState{ID: "1767225900123456789"}, &remotestate.DeploymentState{ID: "1767225000123456789"})
	if undo.Command != "tako rollback --preview pr-42 1767225900" {
		t.Fatalf("rollback undo = %q", undo.Command)
	}
	other := e.FailedBeforeMutationSummary(SummaryOperationRollback, "shop", "staging", []string{"web"}, summaryTestStart, "boom")
	if other.Environment.Kind != SummaryEnvironmentKindStandard {
		t.Fatalf("unregistered environment kind = %q", other.Environment.Kind)
	}
}
