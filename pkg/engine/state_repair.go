package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

const (
	// KindStateRepairResult identifies a serialized state repair result document.
	KindStateRepairResult = "StateRepairResult"

	StateRepairStatusSuccess = "success"
	StateRepairStatusFailed  = "failed"

	StateRepairLocalSyncStatusSkipped = "skipped"
	StateRepairLocalSyncStatusSynced  = "synced"
	StateRepairLocalSyncStatusFailed  = "failed"
)

// StateRepairHistoryManager is the deployment-history write subset needed by state repair.
type StateRepairHistoryManager interface {
	SaveHistory(*remotestate.DeploymentHistory) error
}

// StateRepairRuntimeManager is the runtime-state write subset needed by state repair.
type StateRepairRuntimeManager interface {
	WriteDesired(*takodstate.DesiredRevision) error
	ReadActual() (*takodstate.ActualSnapshot, error)
	WriteActual(*takodstate.ActualSnapshot) error
	WriteNodeActual(string, *takodstate.ActualSnapshot) error
	DeleteNodeActual(string) error
}

// StateRepairNode is one already-collected reachable node. Adapters own SSH dialing,
// inventory collection, and lease handling.
type StateRepairNode struct {
	Name           string                    `json:"name"`
	HistoryManager StateRepairHistoryManager `json:"-"`
	Runtime        StateRepairRuntimeManager `json:"-"`
}

// StateRepairHistoryCandidate is an adapter-collected deployment-history candidate.
type StateRepairHistoryCandidate struct {
	Source  string
	History *remotestate.DeploymentHistory
}

// StateRepairDesiredCandidate is an adapter-collected desired runtime candidate.
type StateRepairDesiredCandidate struct {
	Source  string
	Desired *takodstate.DesiredRevision
}

// StateRepairActualCandidate is an adapter-collected aggregate actual runtime candidate.
type StateRepairActualCandidate struct {
	Source string
	Actual *takodstate.ActualSnapshot
}

// StateRepairNodeActualCandidate is an adapter-collected per-node actual runtime candidate.
type StateRepairNodeActualCandidate struct {
	Source string
	Node   string
	Actual *takodstate.ActualSnapshot
}

// StateRepairBeforeWriteFunc is called after source selection and before any
// repair document writes. CLI adapters use this seam to render selected sources
// and acquire/release leases without making the engine own those protocols.
type StateRepairBeforeWriteFunc func(context.Context, *StateRepairResult) error

// StateRepairRequest describes one state repair operation over already-collected state.
type StateRepairRequest struct {
	Config      *config.Config `json:"-"`
	Environment string         `json:"environment"`
	Server      string         `json:"server,omitempty"`

	Nodes      []StateRepairNode                `json:"-"`
	Histories  []StateRepairHistoryCandidate    `json:"-"`
	Desired    []StateRepairDesiredCandidate    `json:"-"`
	Actual     []StateRepairActualCandidate     `json:"-"`
	NodeActual []StateRepairNodeActualCandidate `json:"-"`

	BeforeWrite StateRepairBeforeWriteFunc `json:"-"`
}

// StateRepairResult is the machine-readable outcome of state repair.
type StateRepairResult struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Server      string `json:"server,omitempty"`
	Status      string `json:"status"`

	Servers []string               `json:"servers"`
	Counts  StateRepairCounts      `json:"counts"`
	Sources StateRepairSources     `json:"selectedSources"`
	Writes  StateRepairWriteResult `json:"writes"`
	Local   StateRepairLocalSync   `json:"localSync"`

	Warnings []string `json:"warnings,omitempty"`
	Error    string   `json:"error,omitempty"`

	SelectedHistory *remotestate.DeploymentHistory `json:"-"`
}

// StateRepairCounts summarizes node reachability for repair.
type StateRepairCounts struct {
	ReachableNodes int `json:"reachableNodes"`
}

// StateRepairSources summarizes the selected repair source documents.
type StateRepairSources struct {
	HasHistory    bool                          `json:"hasHistory"`
	HasDesired    bool                          `json:"hasDesired"`
	HasActual     bool                          `json:"hasActual"`
	HasNodeActual bool                          `json:"hasNodeActual"`
	History       *StateRepairHistorySource     `json:"history,omitempty"`
	Desired       *StateRepairDesiredSource     `json:"desired,omitempty"`
	Actual        *StateRepairActualSource      `json:"actual,omitempty"`
	NodeActual    []StateRepairNodeActualSource `json:"nodeActual,omitempty"`
}

type StateRepairHistorySource struct {
	Source    string    `json:"source"`
	Count     int       `json:"count"`
	Freshness time.Time `json:"freshness"`
}

type StateRepairDesiredSource struct {
	Source     string    `json:"source"`
	RevisionID string    `json:"revisionId"`
	Freshness  time.Time `json:"freshness"`
}

type StateRepairActualSource struct {
	Source       string    `json:"source"`
	ServiceCount int       `json:"serviceCount"`
	Freshness    time.Time `json:"freshness"`
}

type StateRepairNodeActualSource struct {
	Node         string    `json:"node"`
	Source       string    `json:"source"`
	ServiceCount int       `json:"serviceCount"`
	Freshness    time.Time `json:"freshness"`
}

// StateRepairWriteResult summarizes all remote document writes.
type StateRepairWriteResult struct {
	Counts StateRepairWriteCounts       `json:"counts"`
	Nodes  []StateRepairNodeWriteResult `json:"nodes"`
}

// StateRepairWriteCounts aggregates writes by document type.
type StateRepairWriteCounts struct {
	History    int `json:"history"`
	Desired    int `json:"desired"`
	Actual     int `json:"actual"`
	NodeActual int `json:"nodeActual"`
}

// StateRepairNodeWriteResult summarizes writes attempted on one reachable node.
type StateRepairNodeWriteResult struct {
	Name     string                 `json:"name"`
	Counts   StateRepairWriteCounts `json:"counts"`
	Warnings []string               `json:"warnings,omitempty"`
	Error    string                 `json:"error,omitempty"`

	Err error `json:"-"`
}

// StateRepairLocalSync summarizes the optional local .tako refresh performed by adapters.
type StateRepairLocalSync struct {
	Status string `json:"status"`
	Count  int    `json:"count,omitempty"`
	Error  string `json:"error,omitempty"`
}

// StateRepair selects the freshest collected repair sources and writes them to
// already-collected reachable nodes. It never dials SSH, acquires leases, prompts,
// or renders output; adapters own those responsibilities.
func (e *Engine) StateRepair(ctx context.Context, req StateRepairRequest) (*StateRepairResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Config == nil {
		return nil, invalidRequestf("state repair request requires a loaded config")
	}
	envName := strings.TrimSpace(req.Environment)
	if envName == "" {
		return nil, invalidRequestf("state repair request requires an environment")
	}

	cfg := req.Config
	result := &StateRepairResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindStateRepairResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Server:      strings.TrimSpace(req.Server),
		Status:      StateRepairStatusSuccess,
		Servers:     stateRepairNodeNames(req.Nodes),
		Counts:      StateRepairCounts{ReachableNodes: len(req.Nodes)},
		Local:       StateRepairLocalSync{Status: StateRepairLocalSyncStatusSkipped},
	}
	if len(req.Nodes) == 0 {
		err := fmt.Errorf("no reachable environment nodes found")
		stateRepairMarkFailed(result, err)
		return result, err
	}

	selection := selectStateRepairSources(cfg.Project.Name, envName, req.Histories, req.Desired, req.Actual, req.NodeActual)
	result.Sources = summarizeStateRepairSources(selection)
	result.SelectedHistory = selection.history.History
	if !selection.hasHistory && !selection.hasDesired && !selection.hasActual && !selection.hasNodeActual {
		err := fmt.Errorf("no deployment history or runtime state found on reachable nodes")
		stateRepairMarkFailed(result, err)
		return result, err
	}

	if req.BeforeWrite != nil {
		if err := req.BeforeWrite(ctx, result); err != nil {
			stateRepairMarkFailed(result, err)
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		stateRepairMarkFailed(result, err)
		return result, err
	}

	writeResults, writeErr := writeStateRepairDocumentsContext(ctx, req.Nodes, selection)
	result.Writes.Nodes = writeResults
	result.Writes.Counts = SummarizeStateRepairWriteCounts(writeResults)
	result.Warnings = StateRepairWriteWarnings(writeResults)
	if writeErr != nil {
		stateRepairMarkFailed(result, writeErr)
		return result, writeErr
	}
	return result, nil
}

func stateRepairMarkFailed(result *StateRepairResult, err error) {
	if result == nil || err == nil {
		return
	}
	result.Status = StateRepairStatusFailed
	result.Error = err.Error()
}

type stateRepairSelection struct {
	history       StateRepairHistoryCandidate
	hasHistory    bool
	desired       StateRepairDesiredCandidate
	hasDesired    bool
	actual        StateRepairActualCandidate
	hasActual     bool
	nodeActual    map[string]StateRepairNodeActualCandidate
	hasNodeActual bool
}

func selectStateRepairSources(project string, envName string, histories []StateRepairHistoryCandidate, desired []StateRepairDesiredCandidate, actual []StateRepairActualCandidate, nodeActual []StateRepairNodeActualCandidate) stateRepairSelection {
	bestHistory, hasHistory := bestStateRepairHistory(histories)
	bestDesired, hasDesired := bestStateRepairDesired(desired)
	bestActual, hasActual := bestStateRepairActualSnapshot(actual)
	bestNodeActual := bestStateRepairNodeActualSnapshots(nodeActual)
	hasNodeActual := len(bestNodeActual) > 0
	if hasActual && hasNodeActual {
		bestActual.Actual = stateRepairActualSnapshotWithNodeSnapshots(bestActual.Actual, bestNodeActual)
	} else if !hasActual && hasNodeActual {
		bestActual = StateRepairActualCandidate{
			Source: "node actual snapshots",
			Actual: stateRepairAggregateActualSnapshotFromNodeSnapshots(project, envName, bestNodeActual),
		}
		hasActual = stateStatusActualSnapshotRepairable(bestActual.Actual)
	}
	return stateRepairSelection{
		history:       bestHistory,
		hasHistory:    hasHistory,
		desired:       bestDesired,
		hasDesired:    hasDesired,
		actual:        bestActual,
		hasActual:     hasActual,
		nodeActual:    bestNodeActual,
		hasNodeActual: hasNodeActual,
	}
}

func summarizeStateRepairSources(selection stateRepairSelection) StateRepairSources {
	sources := StateRepairSources{HasHistory: selection.hasHistory, HasDesired: selection.hasDesired, HasActual: selection.hasActual, HasNodeActual: selection.hasNodeActual}
	if selection.hasHistory {
		sources.History = &StateRepairHistorySource{Source: selection.history.Source, Count: stateStatusDeploymentHistoryCount(selection.history.History), Freshness: stateStatusDeploymentHistoryFreshness(selection.history.History)}
	}
	if selection.hasDesired {
		sources.Desired = &StateRepairDesiredSource{Source: selection.desired.Source, RevisionID: selection.desired.Desired.RevisionID, Freshness: stateStatusDesiredRevisionFreshness(selection.desired.Desired)}
	}
	if selection.hasActual {
		sources.Actual = &StateRepairActualSource{Source: selection.actual.Source, ServiceCount: stateStatusActualSnapshotServiceCount(selection.actual.Actual), Freshness: stateStatusActualSnapshotFreshness(selection.actual.Actual)}
	}
	for _, nodeName := range sortedStateRepairNodeActualNames(selection.nodeActual) {
		candidate := selection.nodeActual[nodeName]
		sources.NodeActual = append(sources.NodeActual, StateRepairNodeActualSource{Node: nodeName, Source: candidate.Source, ServiceCount: stateStatusActualSnapshotServiceCount(candidate.Actual), Freshness: stateStatusActualSnapshotFreshness(candidate.Actual)})
	}
	return sources
}

// writeStateRepairDocumentsContext writes selected repair documents to all nodes and
// returns the exact fatal/warning semantics used by the CLI repair command.
func writeStateRepairDocumentsContext(ctx context.Context, nodes []StateRepairNode, selection stateRepairSelection) ([]StateRepairNodeWriteResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	results := make([]StateRepairNodeWriteResult, len(nodes))
	resultCh := make(chan struct {
		index  int
		result StateRepairNodeWriteResult
	}, len(nodes))
	var wg sync.WaitGroup
	for index, node := range nodes {
		wg.Add(1)
		go func(index int, node StateRepairNode) {
			defer wg.Done()
			resultCh <- struct {
				index  int
				result StateRepairNodeWriteResult
			}{index: index, result: writeStateRepairDocumentsToNode(ctx, node, selection)}
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
	counts := SummarizeStateRepairWriteCounts(results)
	warnings := StateRepairWriteWarnings(results)
	if len(fatalErrors) > 0 {
		sort.Strings(fatalErrors)
		return results, fmt.Errorf("failed to prepare state repair documents: %s", strings.Join(fatalErrors, "; "))
	}
	if selection.hasHistory && counts.History == 0 {
		return results, fmt.Errorf("failed to write repaired deployment history to any reachable node")
	}
	if selection.hasDesired && counts.Desired == 0 {
		return results, fmt.Errorf("failed to write repaired desired runtime state to any reachable node")
	}
	if selection.hasActual && counts.Actual == 0 {
		return results, fmt.Errorf("failed to write repaired actual runtime state to any reachable node")
	}
	if len(selection.nodeActual) > 0 && counts.NodeActual == 0 {
		return results, fmt.Errorf("failed to write repaired node actual runtime state to any reachable node")
	}
	if len(warnings) > 0 {
		return results, fmt.Errorf("state repair incomplete: %s", strings.Join(warnings, "; "))
	}
	return results, nil
}

// writeStateRepairDocumentsToNode writes selected repair documents to one node.
func writeStateRepairDocumentsToNode(ctx context.Context, node StateRepairNode, selection stateRepairSelection) StateRepairNodeWriteResult {
	if ctx == nil {
		ctx = context.Background()
	}
	result := StateRepairNodeWriteResult{Name: node.Name}
	if err := ctx.Err(); err != nil {
		result.Err = err
		result.Error = err.Error()
		return result
	}
	var previousActual *takodstate.ActualSnapshot
	if selection.hasActual || len(selection.nodeActual) > 0 {
		if node.Runtime == nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("failed to read previous actual runtime state on %s before pruning stale node state: runtime state manager unavailable", node.Name))
		} else {
			var err error
			previousActual, err = node.Runtime.ReadActual()
			if err != nil && !errors.Is(err, takodstate.ErrNotFound) {
				result.Warnings = append(result.Warnings, fmt.Sprintf("failed to read previous actual runtime state on %s before pruning stale node state: %v", node.Name, err))
			}
		}
	}

	if selection.hasHistory {
		if node.HistoryManager == nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("failed to repair deployment history on %s: history state manager unavailable", node.Name))
		} else {
			historyCopy, err := cloneStateRepairHistory(selection.history.History)
			if err != nil {
				result.Err = fmt.Errorf("failed to prepare history for repair: %w", err)
				result.Error = result.Err.Error()
				return result
			}
			if err := node.HistoryManager.SaveHistory(historyCopy); err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("failed to repair deployment history on %s: %v", node.Name, err))
			} else {
				result.Counts.History++
			}
		}
	}

	if selection.hasDesired {
		if node.Runtime == nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("failed to repair desired runtime state on %s: runtime state manager unavailable", node.Name))
		} else {
			desiredCopy, err := cloneStateRepairDesired(selection.desired.Desired)
			if err != nil {
				result.Err = fmt.Errorf("failed to prepare desired runtime state for repair: %w", err)
				result.Error = result.Err.Error()
				return result
			}
			if err := node.Runtime.WriteDesired(desiredCopy); err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("failed to repair desired runtime state on %s: %v", node.Name, err))
			} else {
				result.Counts.Desired++
			}
		}
	}

	if selection.hasActual {
		if node.Runtime == nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("failed to repair actual runtime state on %s: runtime state manager unavailable", node.Name))
		} else {
			actualCopy, err := cloneStateRepairActual(selection.actual.Actual)
			if err != nil {
				result.Err = fmt.Errorf("failed to prepare actual runtime state for repair: %w", err)
				result.Error = result.Err.Error()
				return result
			}
			if err := node.Runtime.WriteActual(actualCopy); err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("failed to repair actual runtime state on %s: %v", node.Name, err))
			} else {
				result.Counts.Actual++
			}
		}
	}

	for _, nodeName := range sortedStateRepairNodeActualNames(selection.nodeActual) {
		candidate := selection.nodeActual[nodeName]
		if node.Runtime == nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("failed to repair node actual runtime state for %s on %s: runtime state manager unavailable", nodeName, node.Name))
			continue
		}
		actualCopy, err := cloneStateRepairActual(candidate.Actual)
		if err != nil {
			result.Err = fmt.Errorf("failed to prepare node actual runtime state for repair: %w", err)
			result.Error = result.Err.Error()
			return result
		}
		if err := node.Runtime.WriteNodeActual(nodeName, actualCopy); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("failed to repair node actual runtime state for %s on %s: %v", nodeName, node.Name, err))
		} else {
			result.Counts.NodeActual++
		}
	}

	if node.Runtime != nil {
		for _, staleNode := range takodstate.StaleNodeActualNames(previousActual, selection.actual.Actual, stateRepairNodeActualSnapshots(selection.nodeActual)) {
			if err := node.Runtime.DeleteNodeActual(staleNode); err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("failed to delete stale node actual runtime state for %s on %s: %v", staleNode, node.Name, err))
			}
		}
	}

	return result
}

// SummarizeStateRepairWriteCounts aggregates per-node write counts.
func SummarizeStateRepairWriteCounts(results []StateRepairNodeWriteResult) StateRepairWriteCounts {
	var counts StateRepairWriteCounts
	for _, result := range results {
		counts.History += result.Counts.History
		counts.Desired += result.Counts.Desired
		counts.Actual += result.Counts.Actual
		counts.NodeActual += result.Counts.NodeActual
	}
	return counts
}

// StateRepairWriteWarnings flattens and sorts per-node write warnings.
func StateRepairWriteWarnings(results []StateRepairNodeWriteResult) []string {
	var warnings []string
	for _, result := range results {
		warnings = append(warnings, result.Warnings...)
	}
	sort.Strings(warnings)
	return warnings
}

func bestStateRepairHistory(candidates []StateRepairHistoryCandidate) (StateRepairHistoryCandidate, bool) {
	var best StateRepairHistoryCandidate
	ok := false
	for _, candidate := range candidates {
		if !stateStatusHistoryHasDeployments(candidate.History) {
			continue
		}
		if !ok || stateStatusDeploymentHistoryBetter(candidate.History, best.History) {
			best = candidate
			ok = true
		}
	}
	return best, ok
}

func bestStateRepairDesired(candidates []StateRepairDesiredCandidate) (StateRepairDesiredCandidate, bool) {
	var best StateRepairDesiredCandidate
	ok := false
	for _, candidate := range candidates {
		if !stateStatusDesiredRevisionRepairable(candidate.Desired) {
			continue
		}
		if !ok || stateStatusDesiredRevisionBetter(candidate.Desired, best.Desired) {
			best = candidate
			ok = true
		}
	}
	return best, ok
}

func bestStateRepairActualSnapshot(candidates []StateRepairActualCandidate) (StateRepairActualCandidate, bool) {
	var best StateRepairActualCandidate
	ok := false
	for _, candidate := range candidates {
		if !stateStatusActualSnapshotRepairable(candidate.Actual) {
			continue
		}
		if !ok || stateStatusActualSnapshotBetter(candidate.Actual, best.Actual) {
			best = candidate
			ok = true
		}
	}
	return best, ok
}

func bestStateRepairNodeActualSnapshots(candidates []StateRepairNodeActualCandidate) map[string]StateRepairNodeActualCandidate {
	best := make(map[string]StateRepairNodeActualCandidate)
	for _, candidate := range candidates {
		if !stateStatusNodeActualSnapshotRepairable(candidate.Actual, candidate.Node) {
			continue
		}
		current, ok := best[candidate.Node]
		if !ok || stateStatusActualSnapshotBetter(candidate.Actual, current.Actual) {
			best[candidate.Node] = candidate
		}
	}
	return best
}

func stateRepairActualSnapshotWithNodeSnapshots(snapshot *takodstate.ActualSnapshot, nodeActual map[string]StateRepairNodeActualCandidate) *takodstate.ActualSnapshot {
	if snapshot == nil {
		return nil
	}
	if len(nodeActual) == 0 {
		return snapshot
	}
	return stateRepairAggregateActualSnapshotFromNodeSnapshots(snapshot.Project, snapshot.Environment, nodeActual)
}

func stateRepairAggregateActualSnapshotFromNodeSnapshots(project string, environment string, nodeActual map[string]StateRepairNodeActualCandidate) *takodstate.ActualSnapshot {
	converted := make(map[string]StateStatusNodeActualCandidate, len(nodeActual))
	for nodeName, candidate := range nodeActual {
		converted[nodeName] = StateStatusNodeActualCandidate{Source: candidate.Source, Node: candidate.Node, Actual: candidate.Actual}
	}
	return stateStatusAggregateActualSnapshotFromNodeSnapshots(project, environment, converted)
}

func sortedStateRepairNodeActualNames(nodeActual map[string]StateRepairNodeActualCandidate) []string {
	nodes := make([]string, 0, len(nodeActual))
	for node := range nodeActual {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	return nodes
}

func stateRepairNodeActualSnapshots(nodeActual map[string]StateRepairNodeActualCandidate) map[string]*takodstate.ActualSnapshot {
	if len(nodeActual) == 0 {
		return nil
	}
	out := make(map[string]*takodstate.ActualSnapshot, len(nodeActual))
	for nodeName, candidate := range nodeActual {
		if candidate.Actual != nil {
			out[nodeName] = candidate.Actual
		}
	}
	return out
}

func cloneStateRepairHistory(history *remotestate.DeploymentHistory) (*remotestate.DeploymentHistory, error) {
	if history == nil {
		return nil, fmt.Errorf("history is nil")
	}
	data, err := json.Marshal(history)
	if err != nil {
		return nil, err
	}
	var out remotestate.DeploymentHistory
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func cloneStateRepairDesired(revision *takodstate.DesiredRevision) (*takodstate.DesiredRevision, error) {
	if revision == nil {
		return nil, fmt.Errorf("desired revision is nil")
	}
	data, err := json.Marshal(revision)
	if err != nil {
		return nil, err
	}
	var out takodstate.DesiredRevision
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func cloneStateRepairActual(snapshot *takodstate.ActualSnapshot) (*takodstate.ActualSnapshot, error) {
	if snapshot == nil {
		return nil, fmt.Errorf("actual snapshot is nil")
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	var out takodstate.ActualSnapshot
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func stateRepairNodeNames(nodes []StateRepairNode) []string {
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		name := strings.TrimSpace(node.Name)
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}
