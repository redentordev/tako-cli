package reconcile

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

// A selfHealing-only change must plan no container update: the policy lives
// in desired state, not in container identity.
func TestComputePlanSelfHealingOnlyChangeIsNoOp(t *testing.T) {
	deployed := config.ServiceConfig{Image: "web:1", Port: 3000, Replicas: 2, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling}}
	hash, ok := SafeServiceConfigHash(deployed)
	if !ok {
		t.Fatal("hash unavailable")
	}
	enabled := true
	desired := deployed
	desired.SelfHealing = &config.SelfHealingConfig{Enabled: &enabled, UnhealthyThreshold: 5, Window: "1h"}
	snapshot := deployed
	actual := map[string]*ActualService{"web": {
		Name: "web", Image: "web:1", Replicas: 2, ConfigHash: hash,
		RuntimeID:      runtimeid.ServiceIdentity("demo", "production", "web"),
		ConfigSnapshot: &snapshot,
	}}
	plan := ComputePlan("demo", "production", map[string]config.ServiceConfig{"web": desired}, actual)
	if plan.Summary.Updates != 0 || plan.Summary.Adds != 0 || plan.Summary.NoOps != 1 {
		t.Fatalf("selfHealing-only change planned %+v: %+v", plan.Summary, plan.Changes)
	}
}
