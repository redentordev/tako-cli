package scheduler

import (
	"encoding/json"
	"testing"
)

func TestPlanMovementDrainMovesOnlyTargetNodeAssignments(t *testing.T) {
	plan, err := PlanMovement(MovementModeDrain, "node-a", "rev-1", map[string]string{"node-a": "id-a", "node-b": "id-b"}, []MovementWorkload{
		{Service: "web", Current: []Assignment{{Slot: 1, Node: "node-a", NodeID: "id-a"}, {Slot: 2, Node: "node-b", NodeID: "id-b"}}, Eligible: []string{"node-b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Executable || len(plan.Moves) != 1 || plan.Moves[0].ToNode != "node-b" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if got := plan.Services[0].Proposed[0]; got.Node != "node-b" || got.NodeID != "id-b" || got.Slot != 1 {
		t.Fatalf("proposed assignment = %+v", got)
	}
	if err := ValidateMovementPlan(plan); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
}

func TestPlanMovementCordonReportsImpactWithoutMoving(t *testing.T) {
	plan, err := PlanMovement(MovementModeCordon, "node-a", "rev-1", nil, []MovementWorkload{
		{Service: "web", Current: []Assignment{{Slot: 1, Node: "node-a"}, {Slot: 2, Node: "node-b"}}, Eligible: []string{"node-a", "node-b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Executable || len(plan.Impacts) != 1 || plan.Impacts[0].Slot != 1 || len(plan.Moves) != 0 || len(plan.Blockers) != 0 {
		t.Fatalf("unexpected cordon plan: %+v", plan)
	}
	if plan.Services[0].Current[0] != plan.Services[0].Proposed[0] {
		t.Fatalf("cordon moved an assignment: %+v", plan.Services[0])
	}
}

func TestPlanMovementBlocksPersistentVolumeDrain(t *testing.T) {
	plan, err := PlanMovement(MovementModeDrain, "node-a", "rev-1", map[string]string{"node-a": "id-a", "node-b": "id-b"}, []MovementWorkload{
		{Service: "db", Volumes: []string{"data:/var/lib/db"}, Current: []Assignment{{Slot: 1, Node: "node-a", NodeID: "id-a"}}, Eligible: []string{"node-b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Executable || len(plan.Blockers) != 1 || len(plan.Moves) != 1 || !plan.Moves[0].RequiresVolumeMigration || plan.Moves[0].ToNode != "" {
		t.Fatalf("persistent movement was not blocked: %+v", plan)
	}
	if plan.Services[0].Proposed[0].Node != "node-a" {
		t.Fatalf("persistent assignment moved implicitly: %+v", plan.Services[0])
	}
}

func TestPlanMovementBlocksGlobalDrainInsteadOfDuplicatingDestination(t *testing.T) {
	plan, err := PlanMovement(MovementModeDrain, "node-a", "rev-1", nil, []MovementWorkload{
		{Service: "agent", Global: true, Current: []Assignment{{Slot: 1, Node: "node-a"}, {Slot: 2, Node: "node-b"}}, Eligible: []string{"node-b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Executable || len(plan.Blockers) != 1 || len(plan.Moves) != 0 {
		t.Fatalf("global drain was not blocked: %+v", plan)
	}
	if plan.Services[0].Proposed[0].Node != "node-a" || plan.Services[0].Proposed[1].Node != "node-b" {
		t.Fatalf("global drain duplicated an assignment: %+v", plan.Services[0])
	}
}

func TestPlanMovementRebalanceIsDeterministicAndConservative(t *testing.T) {
	workloads := []MovementWorkload{{
		Service: "web", Eligible: []string{"node-a", "node-b", "node-c"},
		Current: []Assignment{{Slot: 3, Node: "node-a"}, {Slot: 1, Node: "node-a"}, {Slot: 2, Node: "node-a"}, {Slot: 4, Node: "node-a"}, {Slot: 5, Node: "node-a"}},
	}}
	first, err := PlanMovement(MovementModeRebalance, "", "rev-1", nil, workloads)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PlanMovement(MovementModeRebalance, "", "rev-1", nil, workloads)
	if err != nil {
		t.Fatal(err)
	}
	if first.PlanID != second.PlanID {
		t.Fatalf("stable inputs produced different plan IDs: %s != %s", first.PlanID, second.PlanID)
	}
	loads := map[string]int{}
	for _, assignment := range first.Services[0].Proposed {
		loads[assignment.Node]++
	}
	if loads["node-a"] != 2 || loads["node-b"] != 2 || loads["node-c"] != 1 {
		t.Fatalf("unexpected balanced loads: %#v", loads)
	}
}

func TestValidateMovementPlanRejectsTampering(t *testing.T) {
	plan, err := PlanMovement(MovementModeDrain, "node-a", "rev-1", nil, []MovementWorkload{{Service: "web", Current: []Assignment{{Slot: 1, Node: "node-a"}}, Eligible: []string{"node-b"}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded MovementPlan
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded.Moves[0].ToNode = "node-c"
	if err := ValidateMovementPlan(decoded); err == nil {
		t.Fatal("tampered plan was accepted")
	}
}
