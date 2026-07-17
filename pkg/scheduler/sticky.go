package scheduler

import (
	"fmt"
	"sort"
)

// Assignment binds one stable replica slot to an immutable platform node.
// Slots are one-based and are never renumbered during ordinary scale changes.
type Assignment struct {
	Slot   int    `json:"slot"`
	NodeID string `json:"nodeId"`
	Node   string `json:"node"`
}

// PlanSticky preserves every valid prior slot, removes only slots above the
// desired replica count, and assigns only new slots. It never silently moves a
// retained slot because membership order changed or a new node joined.
func PlanSticky(replicas int, permitted, eligible []string, prior []Assignment) ([]Assignment, error) {
	if replicas < 0 {
		return nil, fmt.Errorf("replicas cannot be negative")
	}
	permittedSet := stringSet(permitted)
	eligibleSet := stringSet(eligible)
	if replicas > 0 && len(eligible) == 0 && len(prior) == 0 {
		return nil, fmt.Errorf("no schedulable nodes are eligible for new assignments")
	}

	bySlot := make(map[int]Assignment, len(prior))
	load := make(map[string]int, len(permitted))
	for _, assignment := range prior {
		if assignment.Slot < 1 || assignment.Node == "" {
			return nil, fmt.Errorf("prior assignment has an invalid slot or node")
		}
		if _, duplicate := bySlot[assignment.Slot]; duplicate {
			return nil, fmt.Errorf("prior assignment slot %d is duplicated", assignment.Slot)
		}
		if _, ok := permittedSet[assignment.Node]; !ok {
			return nil, fmt.Errorf("replica slot %d is assigned to node %s outside current placement; create and approve an explicit movement plan", assignment.Slot, assignment.Node)
		}
		bySlot[assignment.Slot] = assignment
		if assignment.Slot <= replicas {
			load[assignment.Node]++
		}
	}

	planned := make([]Assignment, 0, replicas)
	for slot := 1; slot <= replicas; slot++ {
		if assignment, ok := bySlot[slot]; ok {
			planned = append(planned, assignment)
			continue
		}
		if len(eligibleSet) == 0 {
			return nil, fmt.Errorf("replica slot %d needs a new assignment but no schedulable node is eligible", slot)
		}
		node := leastLoaded(eligible, load)
		planned = append(planned, Assignment{Slot: slot, Node: node})
		load[node]++
	}
	return planned, nil
}

// PlanGlobal retains one assignment for every previously assigned permitted
// node and adds one for each newly eligible node. Node order never renumbers or
// moves existing slots; cordoned nodes remain represented until an explicit
// movement/removal operation is approved.
func PlanGlobal(permitted, eligible []string, prior []Assignment) ([]Assignment, error) {
	if err := ValidateAssignments(prior); err != nil {
		return nil, err
	}
	permittedSet := stringSet(permitted)
	usedNodes := make(map[string]struct{}, len(prior))
	maxSlot := 0
	planned := Stable(prior)
	for _, assignment := range planned {
		if _, ok := permittedSet[assignment.Node]; !ok {
			return nil, fmt.Errorf("global replica slot %d is assigned to node %s outside current placement; create and approve an explicit movement plan", assignment.Slot, assignment.Node)
		}
		if _, duplicate := usedNodes[assignment.Node]; duplicate {
			return nil, fmt.Errorf("global assignments contain duplicate node %s", assignment.Node)
		}
		usedNodes[assignment.Node] = struct{}{}
		if assignment.Slot > maxSlot {
			maxSlot = assignment.Slot
		}
	}
	for _, node := range eligible {
		if _, exists := usedNodes[node]; exists {
			continue
		}
		maxSlot++
		planned = append(planned, Assignment{Slot: maxSlot, Node: node})
		usedNodes[node] = struct{}{}
	}
	if len(planned) == 0 {
		return nil, fmt.Errorf("global service has no schedulable nodes eligible for assignments")
	}
	return Stable(planned), nil
}

// ValidateAssignments checks persisted assignment structure before it is used
// or copied into a new desired revision.
func ValidateAssignments(assignments []Assignment) error {
	seenSlots := make(map[int]struct{}, len(assignments))
	for _, assignment := range assignments {
		if assignment.Slot < 1 || assignment.Node == "" {
			return fmt.Errorf("assignment has an invalid slot or node")
		}
		if _, duplicate := seenSlots[assignment.Slot]; duplicate {
			return fmt.Errorf("assignment slot %d is duplicated", assignment.Slot)
		}
		seenSlots[assignment.Slot] = struct{}{}
	}
	return nil
}

func leastLoaded(ordered []string, load map[string]int) string {
	selected := ordered[0]
	for _, node := range ordered[1:] {
		if load[node] < load[selected] {
			selected = node
		}
	}
	return selected
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

// Stable returns a slot-ordered copy suitable for state persistence.
func Stable(assignments []Assignment) []Assignment {
	out := append([]Assignment(nil), assignments...)
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}
