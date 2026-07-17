package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	MovementModeCordon    = "cordon"
	MovementModeDrain     = "drain"
	MovementModeRebalance = "rebalance"
)

// MovementWorkload is the scheduler input for one service. Eligible contains
// only schedulable nodes allowed by that service's placement constraints.
type MovementWorkload struct {
	Service    string
	Global     bool
	Persistent bool
	Volumes    []string
	Current    []Assignment
	Eligible   []string
}

// MovementPlan is an immutable, reviewable proposal. PlanID is a digest of
// the state revision and all proposed decisions; CreatedAt is informational
// and deliberately excluded from the digest.
type MovementPlan struct {
	APIVersion      string                    `json:"apiVersion"`
	Kind            string                    `json:"kind"`
	PlanID          string                    `json:"planId"`
	Mode            string                    `json:"mode"`
	InputRevisionID string                    `json:"inputRevisionId"`
	TargetNode      string                    `json:"targetNode,omitempty"`
	TargetNodeID    string                    `json:"targetNodeId,omitempty"`
	Services        []MovementServiceProposal `json:"services"`
	Impacts         []MovementImpact          `json:"impacts,omitempty"`
	Moves           []Movement                `json:"moves,omitempty"`
	Blockers        []MovementBlocker         `json:"blockers,omitempty"`
	Executable      bool                      `json:"executable"`
	RequiresReview  bool                      `json:"requiresReview"`
	CreatedAt       time.Time                 `json:"createdAt"`
}

type MovementServiceProposal struct {
	Service  string       `json:"service"`
	Current  []Assignment `json:"current"`
	Proposed []Assignment `json:"proposed"`
}

type Movement struct {
	Service                 string   `json:"service"`
	Slot                    int      `json:"slot"`
	FromNode                string   `json:"fromNode"`
	FromNodeID              string   `json:"fromNodeId,omitempty"`
	ToNode                  string   `json:"toNode,omitempty"`
	ToNodeID                string   `json:"toNodeId,omitempty"`
	Persistent              bool     `json:"persistent,omitempty"`
	Volumes                 []string `json:"volumes,omitempty"`
	RequiresVolumeMigration bool     `json:"requiresVolumeMigration,omitempty"`
}

// MovementImpact identifies a replica that will remain in place when a node
// is cordoned and excluded from future assignments.
type MovementImpact struct {
	Service string `json:"service"`
	Slot    int    `json:"slot"`
	Node    string `json:"node"`
	NodeID  string `json:"nodeId,omitempty"`
}

type MovementBlocker struct {
	Service string `json:"service"`
	Slot    int    `json:"slot"`
	Node    string `json:"node"`
	Reason  string `json:"reason"`
}

// PlanMovement proposes a drain or a conservative per-service rebalance. It
// never proposes moving a persistent service or a service with volume mounts.
func PlanMovement(mode, targetNode, inputRevisionID string, nodeIDs map[string]string, workloads []MovementWorkload) (MovementPlan, error) {
	mode = strings.TrimSpace(mode)
	if mode != MovementModeCordon && mode != MovementModeDrain && mode != MovementModeRebalance {
		return MovementPlan{}, fmt.Errorf("movement mode must be cordon, drain, or rebalance")
	}
	if strings.TrimSpace(inputRevisionID) == "" {
		return MovementPlan{}, fmt.Errorf("input desired revision ID is required")
	}
	if mode != MovementModeRebalance && strings.TrimSpace(targetNode) == "" {
		return MovementPlan{}, fmt.Errorf("%s target node is required", mode)
	}

	ordered := append([]MovementWorkload(nil), workloads...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Service < ordered[j].Service })
	plan := MovementPlan{
		APIVersion: "tako.redentor.dev/v1alpha1", Kind: "PlacementMovementPlan",
		Mode: mode, InputRevisionID: inputRevisionID, TargetNode: targetNode,
		TargetNodeID: nodeIDs[targetNode], RequiresReview: true, CreatedAt: time.Now().UTC(),
	}
	for _, workload := range ordered {
		if strings.TrimSpace(workload.Service) == "" {
			return MovementPlan{}, fmt.Errorf("movement workload service name is required")
		}
		proposal, impacts, moves, blockers, err := planWorkloadMovement(mode, targetNode, nodeIDs, workload)
		if err != nil {
			return MovementPlan{}, err
		}
		plan.Services = append(plan.Services, proposal)
		plan.Impacts = append(plan.Impacts, impacts...)
		plan.Moves = append(plan.Moves, moves...)
		plan.Blockers = append(plan.Blockers, blockers...)
	}
	plan.Executable = len(plan.Blockers) == 0
	plan.PlanID = movementPlanDigest(plan)
	if err := ValidateMovementPlan(plan); err != nil {
		return MovementPlan{}, fmt.Errorf("generated invalid movement plan: %w", err)
	}
	return plan, nil
}

func planWorkloadMovement(mode, targetNode string, nodeIDs map[string]string, workload MovementWorkload) (MovementServiceProposal, []MovementImpact, []Movement, []MovementBlocker, error) {
	current := Stable(workload.Current)
	proposed := append([]Assignment(nil), current...)
	eligible := uniqueStrings(workload.Eligible)
	persistent := workload.Persistent || len(workload.Volumes) > 0
	loads := make(map[string]int, len(eligible))
	for _, assignment := range current {
		loads[assignment.Node]++
	}

	var moves []Movement
	var blockers []MovementBlocker
	var impacts []MovementImpact
	if mode == MovementModeCordon {
		for _, assignment := range current {
			if assignment.Node == targetNode {
				impacts = append(impacts, MovementImpact{Service: workload.Service, Slot: assignment.Slot, Node: assignment.Node, NodeID: assignment.NodeID})
			}
		}
		return MovementServiceProposal{Service: workload.Service, Current: current, Proposed: Stable(proposed)}, impacts, nil, nil, nil
	}
	if workload.Global {
		if mode == MovementModeDrain {
			for _, assignment := range current {
				if assignment.Node == targetNode {
					blockers = append(blockers, MovementBlocker{Service: workload.Service, Slot: assignment.Slot, Node: assignment.Node, Reason: "global service drain requires an explicit membership/removal operation; duplicating the replica on another node would violate global placement"})
				}
			}
		}
		return MovementServiceProposal{Service: workload.Service, Current: current, Proposed: Stable(proposed)}, nil, nil, blockers, nil
	}
	for index, assignment := range current {
		mustMove := mode == MovementModeDrain && assignment.Node == targetNode
		if mode == MovementModeRebalance {
			mustMove = !containsString(eligible, assignment.Node) || overloadedNode(assignment.Node, eligible, loads)
		}
		if !mustMove {
			continue
		}
		if persistent {
			moves = append(moves, Movement{Service: workload.Service, Slot: assignment.Slot, FromNode: assignment.Node, FromNodeID: assignment.NodeID, Persistent: true, Volumes: append([]string(nil), workload.Volumes...), RequiresVolumeMigration: true})
			blockers = append(blockers, MovementBlocker{Service: workload.Service, Slot: assignment.Slot, Node: assignment.Node, Reason: "persistent workload movement requires a separately reviewed backup, restore, and cutover strategy"})
			continue
		}
		destinations := withoutString(eligible, assignment.Node)
		if mode == MovementModeDrain {
			destinations = withoutString(destinations, targetNode)
		}
		if len(destinations) == 0 {
			blockers = append(blockers, MovementBlocker{Service: workload.Service, Slot: assignment.Slot, Node: assignment.Node, Reason: "no schedulable placement-compatible destination is available"})
			continue
		}
		destination := leastLoaded(destinations, loads)
		if mode == MovementModeRebalance && loads[assignment.Node] <= loads[destination]+1 {
			continue
		}
		proposed[index].Node = destination
		proposed[index].NodeID = nodeIDs[destination]
		loads[assignment.Node]--
		loads[destination]++
		moves = append(moves, Movement{Service: workload.Service, Slot: assignment.Slot, FromNode: assignment.Node, FromNodeID: assignment.NodeID, ToNode: destination, ToNodeID: nodeIDs[destination]})
	}
	return MovementServiceProposal{Service: workload.Service, Current: current, Proposed: Stable(proposed)}, impacts, moves, blockers, nil
}

func overloadedNode(node string, eligible []string, loads map[string]int) bool {
	if !containsString(eligible, node) || len(eligible) == 0 {
		return true
	}
	minimum := loads[eligible[0]]
	for _, candidate := range eligible[1:] {
		if loads[candidate] < minimum {
			minimum = loads[candidate]
		}
	}
	return loads[node] > minimum+1
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	var out []string
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func withoutString(values []string, excluded string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != excluded {
			out = append(out, value)
		}
	}
	return out
}

// ValidateMovementPlan detects edits to a reviewed plan document.
func ValidateMovementPlan(plan MovementPlan) error {
	if plan.APIVersion != "tako.redentor.dev/v1alpha1" || plan.Kind != "PlacementMovementPlan" {
		return fmt.Errorf("unsupported placement movement plan version or kind")
	}
	if plan.Mode != MovementModeCordon && plan.Mode != MovementModeDrain && plan.Mode != MovementModeRebalance {
		return fmt.Errorf("placement movement plan mode is invalid")
	}
	if plan.InputRevisionID == "" || (plan.Mode != MovementModeRebalance && plan.TargetNode == "") {
		return fmt.Errorf("placement movement plan is missing its input revision or drain target")
	}
	if plan.PlanID == "" || plan.PlanID != movementPlanDigest(plan) {
		return fmt.Errorf("placement movement plan digest does not match its contents")
	}
	if !plan.RequiresReview {
		return fmt.Errorf("placement movement plan must require review")
	}
	if plan.Executable != (len(plan.Blockers) == 0) {
		return fmt.Errorf("placement movement plan executable flag does not match its blockers")
	}
	proposals := make(map[string]MovementServiceProposal, len(plan.Services))
	for _, service := range plan.Services {
		if service.Service == "" {
			return fmt.Errorf("placement movement plan has an unnamed service")
		}
		if _, duplicate := proposals[service.Service]; duplicate {
			return fmt.Errorf("placement movement plan duplicates service %s", service.Service)
		}
		if err := ValidateAssignments(service.Current); err != nil {
			return fmt.Errorf("service %s current assignments are invalid: %w", service.Service, err)
		}
		if err := ValidateAssignments(service.Proposed); err != nil {
			return fmt.Errorf("service %s proposed assignments are invalid: %w", service.Service, err)
		}
		proposals[service.Service] = service
	}
	seenMoves := make(map[string]struct{}, len(plan.Moves))
	seenImpacts := make(map[string]struct{}, len(plan.Impacts))
	for _, impact := range plan.Impacts {
		key := fmt.Sprintf("%s/%d", impact.Service, impact.Slot)
		if plan.Mode != MovementModeCordon || impact.Node != plan.TargetNode {
			return fmt.Errorf("placement movement plan impact %s is invalid for mode %s", key, plan.Mode)
		}
		if _, duplicate := seenImpacts[key]; duplicate {
			return fmt.Errorf("placement movement plan duplicates impact %s", key)
		}
		seenImpacts[key] = struct{}{}
		proposal, ok := proposals[impact.Service]
		assignment, assignmentOK := assignmentForSlot(proposal.Current, impact.Slot)
		if !ok || !assignmentOK || assignment.Node != impact.Node || assignment.NodeID != impact.NodeID {
			return fmt.Errorf("placement movement plan impact %s does not match its current assignment", key)
		}
	}
	if plan.Mode == MovementModeCordon && (len(plan.Moves) > 0 || len(plan.Blockers) > 0) {
		return fmt.Errorf("cordon impact plans cannot move or block replicas")
	}
	for _, move := range plan.Moves {
		key := fmt.Sprintf("%s/%d", move.Service, move.Slot)
		if _, duplicate := seenMoves[key]; duplicate {
			return fmt.Errorf("placement movement plan duplicates move %s", key)
		}
		seenMoves[key] = struct{}{}
		proposal, ok := proposals[move.Service]
		if !ok {
			return fmt.Errorf("placement movement plan move %s has no service proposal", key)
		}
		current, currentOK := assignmentForSlot(proposal.Current, move.Slot)
		proposed, proposedOK := assignmentForSlot(proposal.Proposed, move.Slot)
		if !currentOK || !proposedOK || current.Node != move.FromNode || current.NodeID != move.FromNodeID {
			return fmt.Errorf("placement movement plan move %s does not match its current assignment", key)
		}
		if move.RequiresVolumeMigration {
			if !move.Persistent || move.ToNode != "" || proposed != current {
				return fmt.Errorf("placement movement plan persistent move %s must remain blocked in place", key)
			}
			continue
		}
		if move.Persistent || move.ToNode == "" || move.ToNode == move.FromNode || proposed.Node != move.ToNode || proposed.NodeID != move.ToNodeID {
			return fmt.Errorf("placement movement plan move %s does not match its proposed assignment", key)
		}
	}
	return nil
}

func assignmentForSlot(assignments []Assignment, slot int) (Assignment, bool) {
	for _, assignment := range assignments {
		if assignment.Slot == slot {
			return assignment, true
		}
	}
	return Assignment{}, false
}

func movementPlanDigest(plan MovementPlan) string {
	plan.PlanID = ""
	plan.CreatedAt = time.Time{}
	data, _ := json.Marshal(plan)
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}
