package reconcile

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func recreateDeployedActual(t *testing.T) *ActualService {
	t.Helper()
	previous := config.ServiceConfig{Image: "nginx:1.27", Port: 8080, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRecreate}}
	hash, ok := SafeServiceConfigHash(previous)
	if !ok {
		t.Fatal("expected safe service hash")
	}
	return &ActualService{
		Name:           "web",
		Image:          "nginx:1.27",
		Replicas:       1,
		ConfigHash:     hash,
		DeployStrategy: config.DeployStrategyRecreate,
		RuntimeID:      runtimeid.ServiceIdentity("demo", "production", "web"),
		ConfigSnapshot: &previous,
	}
}

func TestDetectChangesExplainsDefaultStrategyChange(t *testing.T) {
	desired := config.ServiceConfig{Image: "nginx:1.27", Port: 8080, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling, StrategyDefaulted: true}}
	reasons := detectChanges("demo", "production", "web", desired, recreateDeployedActual(t))
	if len(reasons) == 0 || reasons[0] != "Deploy strategy default changed: recreate → rolling" {
		t.Fatalf("reasons = %#v", reasons)
	}
}

func TestDetectChangesOmitsDefaultStrategyReasonForExplicitRolling(t *testing.T) {
	desired := config.ServiceConfig{Image: "nginx:1.27", Port: 8080, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling}}
	reasons := detectChanges("demo", "production", "web", desired, recreateDeployedActual(t))
	for _, reason := range reasons {
		if strings.Contains(reason, "default changed") {
			t.Fatalf("explicit rolling must not report a default change: %#v", reasons)
		}
	}
	if len(reasons) == 0 {
		t.Fatal("strategy change must still be detected")
	}
}

func TestFormatPlanVerboseShowsStrategyColumn(t *testing.T) {
	rolling := config.ServiceConfig{Image: "nginx:1.27", Port: 8080, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling, StrategyDefaulted: true}}
	recreate := config.ServiceConfig{Image: "postgres:16", Deploy: config.DeployConfig{Strategy: config.DeployStrategyRecreate}}
	plan := &ReconciliationPlan{
		ProjectName: "demo",
		Environment: "production",
		Changes: []ServiceChange{
			{Type: ChangeUpdate, ServiceName: "web", NewConfig: &rolling, Reasons: []string{"Deploy strategy default changed: recreate → rolling"}},
			{Type: ChangeAdd, ServiceName: "db", NewConfig: &recreate},
			{Type: ChangeRemove, ServiceName: "old"},
		},
		Summary: ReconciliationSummary{Total: 3, Updates: 1, Adds: 1, Removes: 1},
	}
	text := plan.FormatPlanVerbose(false)
	for _, want := range []string{"Strategy", "rolling (default)", "│ recreate ", "│ -  "} {
		if !strings.Contains(text, want) {
			t.Fatalf("plan text missing %q:\n%s", want, text)
		}
	}
}
