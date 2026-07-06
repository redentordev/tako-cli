package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

const (
	// KindStateForgetNodeResult identifies a serialized state forget-node result.
	KindStateForgetNodeResult = "StateForgetNodeResult"

	StateForgetNodeStatusSuccess = "success"
	StateForgetNodeStatusFailed  = "failed"
)

// StateForgetNodeRuntimeManager is the runtime-state subset needed to remove
// a retired node from replicated takod state.
type StateForgetNodeRuntimeManager interface {
	ReadActual() (*takodstate.ActualSnapshot, error)
	ReadNodeActual(string) (*takodstate.ActualSnapshot, error)
	WriteActual(*takodstate.ActualSnapshot) error
	DeleteNodeActual(string) error
	AppendEvent(takodstate.Event) error
}

// StateForgetNodeNode is one already-collected reachable node. The CLI builds
// these from the state-repair inventory seam so the engine does not own the
// broader repair inventory flow.
type StateForgetNodeNode struct {
	Name    string                        `json:"name"`
	Runtime StateForgetNodeRuntimeManager `json:"-"`
}

// StateForgetNodeRequest describes one runtime-state cleanup operation.
type StateForgetNodeRequest struct {
	Config      *config.Config        `json:"-"`
	Environment string                `json:"environment"`
	Server      string                `json:"server,omitempty"`
	NodeName    string                `json:"node"`
	Force       bool                  `json:"force,omitempty"`
	Nodes       []StateForgetNodeNode `json:"-"`
}

// StateForgetNodeResult is the serializable outcome of StateForgetNode.
type StateForgetNodeResult struct {
	APIVersion  string                       `json:"apiVersion"`
	Kind        string                       `json:"kind"`
	Project     string                       `json:"project"`
	Environment string                       `json:"environment"`
	Server      string                       `json:"server,omitempty"`
	Servers     []string                     `json:"servers"`
	RetiredNode string                       `json:"retiredNode"`
	Force       bool                         `json:"force,omitempty"`
	Status      string                       `json:"status"`
	Nodes       []StateForgetNodeNodeResult  `json:"nodes"`
	Summary     StateForgetNodeResultSummary `json:"summary"`
	Warnings    []string                     `json:"warnings,omitempty"`
	Error       string                       `json:"error,omitempty"`
}

// StateForgetNodeResultSummary aggregates per-node cleanup outcomes.
type StateForgetNodeResultSummary struct {
	ReachableNodes              int `json:"reachableNodes"`
	StandaloneSnapshotsFound    int `json:"standaloneSnapshotsFound"`
	AggregateActualStatesPruned int `json:"aggregateActualStatesPruned"`
}

// StateForgetNodeNodeResult is one node's cleanup outcome.
type StateForgetNodeNodeResult struct {
	Name              string   `json:"name"`
	NodeActualExisted bool     `json:"nodeActualExisted"`
	AggregatePruned   bool     `json:"aggregatePruned"`
	Warnings          []string `json:"warnings,omitempty"`
	Error             string   `json:"error,omitempty"`

	Err error `json:"-"`
}

// StateForgetNode removes a retired node from aggregate and per-node actual
// runtime state across already-collected reachable nodes. It never prompts or
// renders output; adapters own confirmation, lease acquisition, and rendering.
func (e *Engine) StateForgetNode(ctx context.Context, req StateForgetNodeRequest) (*StateForgetNodeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Config == nil {
		return nil, invalidRequestf("state forget-node request requires a loaded config")
	}
	envName := strings.TrimSpace(req.Environment)
	if envName == "" {
		return nil, invalidRequestf("state forget-node request requires an environment")
	}
	nodeName := strings.TrimSpace(req.NodeName)
	if err := ValidateStateForgetNodeName(nodeName); err != nil {
		return nil, &InvalidRequestError{Err: err}
	}

	cfg := req.Config
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if StateNodeNameInList(envServers, nodeName) && !req.Force {
		return nil, invalidRequestf("node %s is still listed in environment %s; remove it from tako.yaml first or rerun with --force", nodeName, envName)
	}
	if len(req.Nodes) == 0 {
		return nil, fmt.Errorf("no reachable environment nodes found")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	nodeResults, forgetErr := ForgetNodeFromRuntimeNodesContext(ctx, req.Nodes, cfg.Project.Name, envName, nodeName)
	result := &StateForgetNodeResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindStateForgetNodeResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Server:      strings.TrimSpace(req.Server),
		Servers:     stateForgetNodeNames(req.Nodes),
		RetiredNode: nodeName,
		Force:       req.Force,
		Status:      StateForgetNodeStatusSuccess,
		Nodes:       nodeResults,
	}
	result.Summary = SummarizeStateForgetNodeResults(nodeResults)
	result.Warnings = StateForgetNodeWarnings(nodeResults)
	if forgetErr != nil {
		result.Status = StateForgetNodeStatusFailed
		result.Error = forgetErr.Error()
	}
	return result, forgetErr
}

// ForgetNodeFromRuntimeNodes removes nodeName from already-collected runtime managers.
func ForgetNodeFromRuntimeNodes(nodes []StateForgetNodeNode, project string, envName string, nodeName string) ([]StateForgetNodeNodeResult, error) {
	return ForgetNodeFromRuntimeNodesContext(context.Background(), nodes, project, envName, nodeName)
}

// ForgetNodeFromRuntimeNodesContext removes nodeName from already-collected runtime managers bounded by ctx.
func ForgetNodeFromRuntimeNodesContext(ctx context.Context, nodes []StateForgetNodeNode, project string, envName string, nodeName string) ([]StateForgetNodeNodeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	results := make([]StateForgetNodeNodeResult, len(nodes))
	resultCh := make(chan struct {
		index  int
		result StateForgetNodeNodeResult
	}, len(nodes))
	var wg sync.WaitGroup
	for index, node := range nodes {
		wg.Add(1)
		go func(index int, node StateForgetNodeNode) {
			defer wg.Done()
			result := ForgetNodeOnRuntimeNode(ctx, node, project, envName, nodeName)
			result.Name = node.Name
			resultCh <- struct {
				index  int
				result StateForgetNodeNodeResult
			}{index: index, result: result}
		}(index, node)
	}
	wg.Wait()
	close(resultCh)

	var fatalErrors []string
	for indexed := range resultCh {
		result := indexed.result
		results[indexed.index] = result
		if result.Err != nil {
			fatalErrors = append(fatalErrors, fmt.Sprintf("%s: %v", result.Name, result.Err))
		}
	}
	if err := ctx.Err(); err != nil {
		return results, err
	}
	if len(fatalErrors) > 0 {
		sort.Strings(fatalErrors)
		return results, fmt.Errorf("failed to forget node %s: %s", nodeName, strings.Join(fatalErrors, "; "))
	}
	return results, nil
}

// ForgetNodeOnRuntimeNode removes nodeName from one runtime manager.
func ForgetNodeOnRuntimeNode(ctx context.Context, node StateForgetNodeNode, project string, envName string, nodeName string) StateForgetNodeNodeResult {
	if ctx == nil {
		ctx = context.Background()
	}
	result := StateForgetNodeNodeResult{Name: node.Name}
	if err := ctx.Err(); err != nil {
		result.Err = err
		result.Error = err.Error()
		return result
	}
	if node.Runtime == nil {
		result.Err = fmt.Errorf("runtime state manager unavailable")
		result.Error = result.Err.Error()
		return result
	}

	if _, err := node.Runtime.ReadNodeActual(nodeName); err == nil {
		result.NodeActualExisted = true
	} else if err != nil && !errors.Is(err, takodstate.ErrNotFound) {
		result.Warnings = append(result.Warnings, fmt.Sprintf("could not check existing node actual snapshot: %v", err))
	}

	if err := ctx.Err(); err != nil {
		result.Err = err
		result.Error = err.Error()
		return result
	}
	actual, err := node.Runtime.ReadActual()
	if err != nil && !errors.Is(err, takodstate.ErrNotFound) {
		result.Err = fmt.Errorf("failed to read aggregate actual state: %w", err)
		result.Error = result.Err.Error()
		return result
	}
	if err == nil {
		pruned, changed := ActualSnapshotWithoutNode(actual, nodeName)
		if changed {
			if err := node.Runtime.WriteActual(pruned); err != nil {
				result.Err = fmt.Errorf("failed to write pruned aggregate actual state: %w", err)
				result.Error = result.Err.Error()
				return result
			}
			result.AggregatePruned = true
		}
	}

	if err := ctx.Err(); err != nil {
		result.Err = err
		result.Error = err.Error()
		return result
	}
	if err := node.Runtime.DeleteNodeActual(nodeName); err != nil {
		result.Err = fmt.Errorf("failed to delete node actual snapshot: %w", err)
		result.Error = result.Err.Error()
		return result
	}

	event := takodstate.NewEvent(project, envName, "state_node_forgotten", "", fmt.Sprintf("Forgot node %s from replicated runtime state", nodeName), map[string]string{
		"node": nodeName,
	})
	if err := node.Runtime.AppendEvent(event); err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("failed to append state cleanup event: %v", err))
	}
	return result
}

// ActualSnapshotWithoutNode returns a copy of snapshot with nodeName removed from target and embedded node lists.
func ActualSnapshotWithoutNode(snapshot *takodstate.ActualSnapshot, nodeName string) (*takodstate.ActualSnapshot, bool) {
	if snapshot == nil {
		return nil, false
	}

	targetNodes, targetChanged := RemoveStateNodeName(snapshot.TargetNodes, nodeName)
	nodes, nodeChanged := RemoveActualEmbeddedNode(snapshot.Nodes, nodeName)
	if !targetChanged && !nodeChanged {
		return snapshot, false
	}

	pruned := *snapshot
	pruned.TargetNodes = targetNodes
	pruned.Nodes = nodes
	pruned.Services = CopyActualServices(snapshot.Services)
	pruned.CapturedAt = time.Now().UTC()
	return &pruned, true
}

// RemoveStateNodeName returns a copy of nodes without nodeName.
func RemoveStateNodeName(nodes []string, nodeName string) ([]string, bool) {
	if len(nodes) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(nodes))
	changed := false
	for _, node := range nodes {
		if node == nodeName {
			changed = true
			continue
		}
		out = append(out, node)
	}
	if !changed {
		return append([]string(nil), nodes...), false
	}
	return out, true
}

// RemoveActualEmbeddedNode returns a copy of nodes without nodeName.
func RemoveActualEmbeddedNode(nodes map[string]takodstate.ActualNodeSnapshot, nodeName string) (map[string]takodstate.ActualNodeSnapshot, bool) {
	if len(nodes) == 0 {
		return nil, false
	}
	if _, ok := nodes[nodeName]; !ok {
		return CopyActualNodeSnapshots(nodes), false
	}
	out := make(map[string]takodstate.ActualNodeSnapshot, len(nodes)-1)
	for node, snapshot := range nodes {
		if node == nodeName {
			continue
		}
		copied := snapshot
		copied.Services = CopyActualServices(snapshot.Services)
		out[node] = copied
	}
	if len(out) == 0 {
		return nil, true
	}
	return out, true
}

// CopyActualServices deep-copies service container slices.
func CopyActualServices(services map[string]takodstate.ActualService) map[string]takodstate.ActualService {
	if services == nil {
		return nil
	}
	out := make(map[string]takodstate.ActualService, len(services))
	for serviceName, service := range services {
		service.Containers = append([]string(nil), service.Containers...)
		out[serviceName] = service
	}
	return out
}

// CopyActualNodeSnapshots copies embedded node snapshots and service maps.
func CopyActualNodeSnapshots(nodes map[string]takodstate.ActualNodeSnapshot) map[string]takodstate.ActualNodeSnapshot {
	if nodes == nil {
		return nil
	}
	out := make(map[string]takodstate.ActualNodeSnapshot, len(nodes))
	for nodeName, snapshot := range nodes {
		copied := snapshot
		copied.Services = CopyActualServices(snapshot.Services)
		out[nodeName] = copied
	}
	return out
}

// ValidateStateForgetNodeName validates node names used in state document keys.
func ValidateStateForgetNodeName(nodeName string) error {
	if nodeName == "" {
		return fmt.Errorf("node name is required")
	}
	for _, ch := range nodeName {
		if (ch >= 'a' && ch <= 'z') ||
			(ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '-' ||
			ch == '_' ||
			ch == '.' {
			continue
		}
		return fmt.Errorf("node name %q contains unsupported characters", nodeName)
	}
	if nodeName == "." || nodeName == ".." || strings.Contains(nodeName, "..") {
		return fmt.Errorf("node name %q is not allowed", nodeName)
	}
	return nil
}

func stateForgetNodeNames(nodes []StateForgetNodeNode) []string {
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		name := strings.TrimSpace(node.Name)
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// StateNodeNameInList reports whether nodeName appears in nodes.
func StateNodeNameInList(nodes []string, nodeName string) bool {
	for _, node := range nodes {
		if node == nodeName {
			return true
		}
	}
	return false
}

// SummarizeStateForgetNodeResults aggregates per-node cleanup outcomes.
func SummarizeStateForgetNodeResults(results []StateForgetNodeNodeResult) StateForgetNodeResultSummary {
	summary := StateForgetNodeResultSummary{ReachableNodes: len(results)}
	for _, result := range results {
		if result.AggregatePruned {
			summary.AggregateActualStatesPruned++
		}
		if result.NodeActualExisted {
			summary.StandaloneSnapshotsFound++
		}
	}
	return summary
}

// StateForgetNodeWarnings flattens per-node warnings with node prefixes.
func StateForgetNodeWarnings(results []StateForgetNodeNodeResult) []string {
	var warnings []string
	for _, result := range results {
		for _, warning := range result.Warnings {
			warnings = append(warnings, fmt.Sprintf("%s: %s", result.Name, warning))
		}
	}
	return warnings
}
