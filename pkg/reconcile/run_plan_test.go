package reconcile

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func runPlanFixture() config.ServiceConfig {
	return config.ServiceConfig{
		Kind: config.ServiceKindRun, Image: "busybox:1.36",
		Command: config.ListValue("sh", "-c", "echo migrate"),
	}
}

func TestComputePlanRunUsesRecordedFingerprintWithoutContainer(t *testing.T) {
	desired := runPlanFixture()
	hash, ok := SafeServiceConfigHash(desired)
	if !ok {
		t.Fatal("expected run hash")
	}
	snapshot := desired
	actual := &ActualService{Name: "migrate", ConfigHash: hash, ConfigSnapshot: &snapshot}
	plan := ComputePlan("demo", "production", map[string]config.ServiceConfig{"migrate": desired}, map[string]*ActualService{"migrate": actual})
	if len(plan.Changes) != 1 || plan.Changes[0].Type != ChangeNone {
		t.Fatalf("plan = %#v, want run no-op", plan.Changes)
	}
	if plan.Summary.NoOps != 1 {
		t.Fatalf("summary = %#v", plan.Summary)
	}
}

func TestComputePlanRunAddsAndUpdatesByFingerprint(t *testing.T) {
	desired := runPlanFixture()
	added := ComputePlan("demo", "production", map[string]config.ServiceConfig{"migrate": desired}, nil)
	if added.Changes[0].Type != ChangeAdd {
		t.Fatalf("absent run change = %s", added.Changes[0].Type)
	}
	snapshot := desired
	actual := &ActualService{Name: "migrate", ConfigHash: "old", ConfigSnapshot: &snapshot}
	updated := ComputePlan("demo", "production", map[string]config.ServiceConfig{"migrate": desired}, map[string]*ActualService{"migrate": actual})
	if updated.Changes[0].Type != ChangeUpdate {
		t.Fatalf("changed run change = %s", updated.Changes[0].Type)
	}
}
