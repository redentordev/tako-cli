package reconcile

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func jobServiceFixture() config.ServiceConfig {
	return config.ServiceConfig{
		Kind:     config.ServiceKindJob,
		Schedule: "*/5 * * * *",
		Build:    "./report",
		Command:  config.StringValue("generate-report"),
	}
}

func scheduledJobActual(desired config.ServiceConfig) *ActualService {
	hash, _ := SafeServiceConfigHash(desired)
	return &ActualService{
		Name:       "report",
		Image:      "demo/report:abc",
		ConfigHash: hash,
		ConfigSnapshot: &config.ServiceConfig{
			Kind:     config.ServiceKindJob,
			Schedule: desired.Schedule,
		},
	}
}

func TestComputePlanJobNotScheduledIsAdd(t *testing.T) {
	plan := ComputePlan("demo", "production",
		map[string]config.ServiceConfig{"report": jobServiceFixture()},
		map[string]*ActualService{},
	)
	if plan.Summary.Adds != 1 || plan.Summary.Total != 1 {
		t.Fatalf("summary = %+v", plan.Summary)
	}
	if plan.Changes[0].Type != ChangeAdd {
		t.Fatalf("change = %+v", plan.Changes[0])
	}
}

func TestComputePlanJobUpToDateIsNoOp(t *testing.T) {
	desired := jobServiceFixture()
	plan := ComputePlan("demo", "production",
		map[string]config.ServiceConfig{"report": desired},
		map[string]*ActualService{"report": scheduledJobActual(desired)},
	)
	if plan.Summary.NoOps != 1 || plan.Summary.Total != 1 {
		t.Fatalf("summary = %+v", plan.Summary)
	}
	if !plan.IsEmpty() {
		t.Fatal("plan with in-sync job should be empty")
	}
}

func TestComputePlanJobScheduleChangeIsUpdate(t *testing.T) {
	deployed := jobServiceFixture()
	desired := jobServiceFixture()
	desired.Schedule = "0 4 * * *"
	plan := ComputePlan("demo", "production",
		map[string]config.ServiceConfig{"report": desired},
		map[string]*ActualService{"report": scheduledJobActual(deployed)},
	)
	if plan.Summary.Updates != 1 {
		t.Fatalf("summary = %+v", plan.Summary)
	}
	if plan.Changes[0].Type != ChangeUpdate {
		t.Fatalf("change = %+v", plan.Changes[0])
	}
}

func TestComputePlanJobRunContainerDoesNotCountAsDeployed(t *testing.T) {
	// A job's transient run container surfaces as a plain actual service
	// (no job snapshot); the job must still plan as add, not update/remove.
	desired := jobServiceFixture()
	runContainer := &ActualService{
		Name:           "report",
		Image:          "demo/report:abc",
		Replicas:       1,
		Containers:     []string{"cid"},
		ConfigSnapshot: &config.ServiceConfig{Image: "demo/report:abc"},
	}
	plan := ComputePlan("demo", "production",
		map[string]config.ServiceConfig{"report": desired},
		map[string]*ActualService{"report": runContainer},
	)
	if plan.Summary.Adds != 1 || plan.Summary.Removes != 0 {
		t.Fatalf("summary = %+v", plan.Summary)
	}
}

func TestComputePlanStaleJobIsRemove(t *testing.T) {
	deployed := jobServiceFixture()
	plan := ComputePlan("demo", "production",
		map[string]config.ServiceConfig{},
		map[string]*ActualService{"report": scheduledJobActual(deployed)},
	)
	if plan.Summary.Removes != 1 {
		t.Fatalf("summary = %+v", plan.Summary)
	}
	if plan.Changes[0].Reasons[0] != "Job is scheduled but no longer defined in tako.yaml" {
		t.Fatalf("reasons = %v", plan.Changes[0].Reasons)
	}
}

func TestSafeServiceConfigHashCoversJobFields(t *testing.T) {
	base := jobServiceFixture()
	baseHash, ok := SafeServiceConfigHash(base)
	if !ok {
		t.Fatal("hash failed")
	}
	for name, mutate := range map[string]func(*config.ServiceConfig){
		"schedule": func(s *config.ServiceConfig) { s.Schedule = "0 1 * * *" },
		"timezone": func(s *config.ServiceConfig) { s.Timezone = "Europe/Berlin" },
		"timeout":  func(s *config.ServiceConfig) { s.Timeout = "30m" },
		"kind":     func(s *config.ServiceConfig) { s.Kind = config.ServiceKindService },
	} {
		mutated := jobServiceFixture()
		mutate(&mutated)
		hash, ok := SafeServiceConfigHash(mutated)
		if !ok {
			t.Fatalf("%s: hash failed", name)
		}
		if hash == baseHash {
			t.Fatalf("%s change not reflected in config hash", name)
		}
	}
}
