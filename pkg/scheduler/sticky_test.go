package scheduler

import (
	"strings"
	"testing"
)

func TestPlanStickyPreservesSingletonAcrossJoinAndReorder(t *testing.T) {
	prior := []Assignment{{Slot: 1, Node: "node-a", NodeID: "id-a"}}
	got, err := PlanSticky(1, []string{"node-b", "node-a", "node-c"}, []string{"node-b", "node-a", "node-c"}, prior)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != prior[0] {
		t.Fatalf("sticky assignments = %#v", got)
	}
}

func TestPlanStickyScaleAddsWithoutMovingAndScaleDownDropsHighestSlots(t *testing.T) {
	prior := []Assignment{{Slot: 1, Node: "node-a"}, {Slot: 2, Node: "node-b"}}
	scaled, err := PlanSticky(4, []string{"node-a", "node-b", "node-c"}, []string{"node-a", "node-b", "node-c"}, prior)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"node-a", "node-b", "node-c", "node-a"}
	for index, assignment := range scaled {
		if assignment.Slot != index+1 || assignment.Node != want[index] {
			t.Fatalf("scaled assignments = %#v", scaled)
		}
	}
	down, err := PlanSticky(1, []string{"node-c", "node-b", "node-a"}, []string{"node-c", "node-b", "node-a"}, scaled)
	if err != nil || len(down) != 1 || down[0].Node != "node-a" {
		t.Fatalf("scaled-down assignments = %#v, %v", down, err)
	}
}

func TestPlanStickyRequiresExplicitMovementForPlacementChange(t *testing.T) {
	_, err := PlanSticky(1, []string{"node-b"}, []string{"node-b"}, []Assignment{{Slot: 1, Node: "node-a"}})
	if err == nil || !strings.Contains(err.Error(), "explicit movement plan") {
		t.Fatalf("placement movement error = %v", err)
	}
}

func TestPlanStickyRetainsCordonedAssignmentButDoesNotUseItForScaleOut(t *testing.T) {
	got, err := PlanSticky(2, []string{"node-a", "node-b"}, []string{"node-b"}, []Assignment{{Slot: 1, Node: "node-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Node != "node-a" || got[1].Node != "node-b" {
		t.Fatalf("cordoned retention assignments = %#v", got)
	}
}

func TestPlanGlobalRetainsSlotsAcrossReorderJoinAndCordon(t *testing.T) {
	prior := []Assignment{{Slot: 1, Node: "node-b", NodeID: "id-b"}, {Slot: 2, Node: "node-a", NodeID: "id-a"}}
	planned, err := PlanGlobal([]string{"node-c", "node-a", "node-b"}, []string{"node-c", "node-a"}, prior)
	if err != nil {
		t.Fatal(err)
	}
	if len(planned) != 3 || planned[0] != prior[0] || planned[1] != prior[1] || planned[2].Slot != 3 || planned[2].Node != "node-c" {
		t.Fatalf("global plan = %#v", planned)
	}
}
