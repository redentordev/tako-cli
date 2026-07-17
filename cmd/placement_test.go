package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/scheduler"
)

func TestReadPlacementPlanUsesStrictSingleDocument(t *testing.T) {
	plan, err := scheduler.PlanMovement(scheduler.MovementModeDrain, "node-a", "rev-1", nil, []scheduler.MovementWorkload{
		{Service: "web", Current: []scheduler.Assignment{{Slot: 1, Node: "node-a"}}, Eligible: []string{"node-b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	decoded, err := readPlacementPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.PlanID != plan.PlanID {
		t.Fatalf("decoded plan ID = %s, want %s", decoded.PlanID, plan.PlanID)
	}
	if err := os.WriteFile(path, append(data, []byte(` {}`)...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPlacementPlan(path); err == nil || !strings.Contains(err.Error(), "multiple JSON documents") {
		t.Fatalf("trailing document error = %v", err)
	}
}
