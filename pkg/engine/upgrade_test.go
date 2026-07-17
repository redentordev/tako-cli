package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestPlanNodeUpgradeWorkerFirstControllerLast(t *testing.T) {
	plan, err := PlanNodeUpgrade([]UpgradeNode{
		{Name: "controller", Roles: []string{nodeidentity.RoleControlPlane, nodeidentity.RoleWorker}},
		{Name: "worker-b", Roles: []string{nodeidentity.RoleWorker}},
		{Name: "worker-a", Roles: []string{nodeidentity.RoleWorker}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []UpgradePlanNode{{Name: "worker-a", Stage: UpgradeStageCanary}, {Name: "worker-b", Stage: UpgradeStageWorkers}, {Name: "controller", Stage: UpgradeStageController}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan = %#v, want %#v", plan, want)
	}
}

func TestPlanNodeUpgradeRejectsUnsafeTopology(t *testing.T) {
	_, err := PlanNodeUpgrade([]UpgradeNode{{Name: "controller-a", Roles: []string{nodeidentity.RoleControlPlane}}, {Name: "controller-b", Roles: []string{nodeidentity.RoleControlPlane}}})
	if err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("error = %v", err)
	}
	_, err = PlanNodeUpgrade([]UpgradeNode{{Name: "controller", Roles: []string{nodeidentity.RoleControlPlane}}, {Name: "legacy"}})
	if err == nil || !strings.Contains(err.Error(), "mix") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateUpgradeCompatibilityNNMinusOne(t *testing.T) {
	for _, version := range []string{"v0.8.3", "0.9.3"} {
		if err := ValidateUpgradeCompatibility(version, 0, 0); err != nil {
			t.Fatalf("legacy bridge %s rejected: %v", version, err)
		}
	}
	if err := ValidateUpgradeCompatibility("v0.9.4", UpgradeProtocolCurrent, UpgradeProtocolCurrent); err != nil {
		t.Fatalf("current range rejected: %v", err)
	}
	for _, test := range []struct {
		version      string
		maximum, min int
	}{{"v0.7.0", 0, 0}, {"v0.9.4", 0, 0}, {"v0.9.4", 1, 2}, {"v0.9.4", 2, 2}} {
		if err := ValidateUpgradeCompatibility(test.version, test.maximum, test.min); err == nil {
			t.Fatalf("invalid protocol status %#v accepted", test)
		}
	}
}
