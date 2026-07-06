package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

type recordingRemover struct {
	removed []string
	fail    string
}

func (r *recordingRemover) RemoveServiceTakod(serviceName string) error {
	if r.fail == serviceName {
		return fmt.Errorf("boom on %s", serviceName)
	}
	r.removed = append(r.removed, serviceName)
	return nil
}

func TestApplyRemovalsEmitsOrderedEvents(t *testing.T) {
	sink := &events.BufferSink{}
	eng := New(Options{Sink: sink})
	remover := &recordingRemover{}
	plan := &reconcile.ReconciliationPlan{
		Changes: []reconcile.ServiceChange{
			{Type: reconcile.ChangeUpdate, ServiceName: "web"},
			{Type: reconcile.ChangeRemove, ServiceName: "worker"},
			{Type: reconcile.ChangeRemove, ServiceName: "cron"},
		},
	}

	if err := eng.ApplyRemovals(remover, plan); err != nil {
		t.Fatalf("ApplyRemovals returned error: %v", err)
	}
	if got := strings.Join(remover.removed, ","); got != "worker,cron" {
		t.Fatalf("removed = %q, want worker,cron", got)
	}

	emitted := sink.Events()
	wantTypes := []string{
		events.TypeDeployServiceStarted,
		events.TypeDeployServiceRemoved,
		events.TypeDeployServiceStarted,
		events.TypeDeployServiceRemoved,
	}
	if len(emitted) != len(wantTypes) {
		t.Fatalf("events = %d, want %d", len(emitted), len(wantTypes))
	}
	for i, want := range wantTypes {
		if emitted[i].Type != want {
			t.Fatalf("event[%d].Type = %q, want %q", i, emitted[i].Type, want)
		}
		if emitted[i].Seq != int64(i+1) {
			t.Fatalf("event[%d].Seq = %d, want %d", i, emitted[i].Seq, i+1)
		}
	}
	if emitted[0].Message != "→ Removing service: worker\n" {
		t.Fatalf("message = %q", emitted[0].Message)
	}
}

func TestApplyRemovalsStopsOnFailure(t *testing.T) {
	eng := New(Options{})
	remover := &recordingRemover{fail: "worker"}
	plan := &reconcile.ReconciliationPlan{
		Changes: []reconcile.ServiceChange{
			{Type: reconcile.ChangeRemove, ServiceName: "worker"},
			{Type: reconcile.ChangeRemove, ServiceName: "cron"},
		},
	}
	err := eng.ApplyRemovals(remover, plan)
	if err == nil || !strings.Contains(err.Error(), "remove failed for worker") {
		t.Fatalf("err = %v", err)
	}
	if len(remover.removed) != 0 {
		t.Fatalf("removed = %v, want none", remover.removed)
	}
}

func TestEngineEventsRedactRegisteredSecrets(t *testing.T) {
	sink := &events.BufferSink{}
	eng := New(Options{Sink: sink})
	eng.RegisterSecret("hunter2-super-secret")

	eng.info(events.TypeLogLine, events.PhaseDeploy, "env value hunter2-super-secret leaked?\n")

	emitted := sink.Events()
	if len(emitted) != 1 {
		t.Fatalf("events = %d, want 1", len(emitted))
	}
	if strings.Contains(emitted[0].Message, "hunter2-super-secret") {
		t.Fatalf("event leaked registered secret: %q", emitted[0].Message)
	}
}

type recordingPruner struct {
	pruned bool
}

func (p *recordingPruner) PruneTakodServiceRevisions(services map[string]config.ServiceConfig, keepRevisions map[string]string) error {
	p.pruned = true
	return nil
}

func TestPruneRevisionsAfterGraceSkipsWithoutRevisions(t *testing.T) {
	eng := New(Options{})
	pruner := &recordingPruner{}
	if err := eng.PruneRevisionsAfterGrace(pruner, nil, nil, func(time.Duration) {}); err != nil {
		t.Fatalf("PruneRevisionsAfterGrace returned error: %v", err)
	}
	if pruner.pruned {
		t.Fatal("pruned without keep revisions")
	}
}

func TestDeployPlanHashIgnoresHumanText(t *testing.T) {
	plan := DeployPlan{
		Project:     "demo",
		Environment: "production",
		Revision:    "abc123",
		Services:    []string{"web"},
	}
	withText := plan
	withText.HumanText = "pretty table"
	if plan.Hash() == "" {
		t.Fatal("hash is empty")
	}
	if plan.Hash() != withText.Hash() {
		t.Fatal("hash changed with human text")
	}
	changed := plan
	changed.Revision = "def456"
	if plan.Hash() == changed.Hash() {
		t.Fatal("hash did not change with revision")
	}
}

func TestPlanDeployRejectsInvalidRequests(t *testing.T) {
	eng := New(Options{})
	if _, err := eng.PlanDeploy(t.Context(), DeployRequest{}); Classify(err) != ClassInvalid {
		t.Fatalf("missing config: Classify = %d, want ClassInvalid (%v)", Classify(err), err)
	}
	if _, err := eng.PlanDeploy(t.Context(), DeployRequest{Config: &config.Config{}}); Classify(err) != ClassInvalid {
		t.Fatalf("missing environment: Classify = %d, want ClassInvalid (%v)", Classify(err), err)
	}
}
