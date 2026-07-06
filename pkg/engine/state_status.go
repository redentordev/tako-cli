package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

const (
	// KindStateStatusResult identifies a serialized state status result document.
	KindStateStatusResult = "StateStatusResult"

	StateStatusLocalMissing    = "missing"
	StateStatusLocalExists     = "exists"
	StateStatusNodeReachable   = "reachable"
	StateStatusNodeUnreachable = "unreachable"
)

// StateStatusRequest describes a state status summarization. Adapters own all
// I/O and pass already-collected local and remote state into the engine.
type StateStatusRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Server      string `json:"server,omitempty"`

	Local StateStatusLocalInput        `json:"-"`
	Nodes []StateStatusRemoteNodeInput `json:"-"`
}

// StateStatusLocalInput is the adapter-provided local .tako state summary.
type StateStatusLocalInput struct {
	Path    string                      `json:"path,omitempty"`
	Exists  bool                        `json:"exists"`
	Current *localstate.DeploymentState `json:"-"`
	Error   string                      `json:"error,omitempty"`
}

// StateStatusRemoteNodeInput is the adapter-provided remote node inventory.
type StateStatusRemoteNodeInput struct {
	Name       string
	Host       string
	EnvNodes   []string
	ConnectErr error

	History    *remotestate.DeploymentHistory
	HistoryErr error
	Desired    *takodstate.DesiredRevision
	DesiredErr error
	Actual     *takodstate.ActualSnapshot
	ActualErr  error
	NodeActual []StateStatusNodeActualCandidate

	Agent    *StateStatusAgentSummary
	AgentErr error
	Mesh     *StateStatusMeshSummary
	MeshErr  error
	Lease    *remotestate.LeaseInfo
	LeaseErr error
}

// StateStatusAgentSummary is a JSON-friendly takod agent status.
type StateStatusAgentSummary struct {
	Runtime   string    `json:"runtime,omitempty"`
	Version   string    `json:"version,omitempty"`
	Hostname  string    `json:"hostname,omitempty"`
	Socket    string    `json:"socket,omitempty"`
	DataDir   string    `json:"dataDir,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	Now       time.Time `json:"now,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// StateStatusMeshSummary is a JSON-friendly WireGuard mesh status.
type StateStatusMeshSummary struct {
	Interface  string `json:"interface,omitempty"`
	Up         bool   `json:"up"`
	ListenPort string `json:"listenPort,omitempty"`
	Peers      int    `json:"peers"`
	PublicKey  string `json:"publicKey,omitempty"`
	Error      string `json:"error,omitempty"`
}

// StateStatusResult is the machine-readable result for `tako state status`.
type StateStatusResult struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Server      string `json:"server,omitempty"`

	Local     StateStatusLocalSummary  `json:"local"`
	Remote    StateStatusRemoteSummary `json:"remote"`
	BestKnown StateStatusBestKnown     `json:"bestKnown"`
	Sync      StateStatusSyncSummary   `json:"sync"`
	Counts    StateStatusCounts        `json:"counts"`
	Error     string                   `json:"error,omitempty"`
}

type StateStatusLocalSummary struct {
	Path           string                      `json:"path"`
	Exists         bool                        `json:"exists"`
	Status         string                      `json:"status"`
	Error          string                      `json:"error,omitempty"`
	LastDeployment *StateStatusLocalDeployment `json:"lastDeployment,omitempty"`
}

type StateStatusLocalDeployment struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Status    string    `json:"status,omitempty"`
	Commit    string    `json:"commit,omitempty"`
}

type StateStatusRemoteSummary struct {
	Title               string                  `json:"title"`
	Nodes               []StateStatusNodeResult `json:"nodes"`
	UnreachableGuidance []string                `json:"unreachableGuidance,omitempty"`
}

type StateStatusNodeResult struct {
	Name      string `json:"name"`
	Host      string `json:"host,omitempty"`
	Status    string `json:"status"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`

	Agent   *StateStatusAgentSummary  `json:"agent,omitempty"`
	Mesh    *StateStatusMeshSummary   `json:"mesh,omitempty"`
	History StateStatusHistorySummary `json:"history"`
	Desired StateStatusDesiredSummary `json:"desired"`
	Actual  StateStatusActualSummary  `json:"actual"`
	Lease   StateStatusLeaseSummary   `json:"lease"`
}

type StateStatusHistorySummary struct {
	Recorded  bool                         `json:"recorded"`
	Missing   bool                         `json:"missing,omitempty"`
	Count     int                          `json:"count,omitempty"`
	Freshness time.Time                    `json:"freshness,omitempty"`
	Latest    *StateStatusRemoteDeployment `json:"latest,omitempty"`
	Error     string                       `json:"error,omitempty"`
}

type StateStatusRemoteDeployment struct {
	ID        string                       `json:"id"`
	DisplayID string                       `json:"displayId,omitempty"`
	Status    remotestate.DeploymentStatus `json:"status,omitempty"`
	Timestamp time.Time                    `json:"timestamp"`
	User      string                       `json:"user,omitempty"`
	Commit    string                       `json:"commit,omitempty"`
}

type StateStatusDesiredSummary struct {
	Recorded     bool      `json:"recorded"`
	RevisionID   string    `json:"revisionId,omitempty"`
	ServiceCount int       `json:"serviceCount,omitempty"`
	Freshness    time.Time `json:"freshness,omitempty"`
	Error        string    `json:"error,omitempty"`
}

type StateStatusActualSummary struct {
	Recorded        bool      `json:"recorded"`
	ServiceCount    int       `json:"serviceCount,omitempty"`
	Freshness       time.Time `json:"freshness,omitempty"`
	NodeActualCount int       `json:"nodeActualCount,omitempty"`
	Error           string    `json:"error,omitempty"`
}

type StateStatusLeaseSummary struct {
	Status string                 `json:"status"`
	Lease  *remotestate.LeaseInfo `json:"lease,omitempty"`
	Error  string                 `json:"error,omitempty"`
}

type StateStatusBestKnown struct {
	History    *StateStatusBestHistory     `json:"history,omitempty"`
	Desired    *StateStatusBestDesired     `json:"desired,omitempty"`
	Actual     *StateStatusBestActual      `json:"actual,omitempty"`
	NodeActual []StateStatusBestNodeActual `json:"nodeActual,omitempty"`
}

type StateStatusBestHistory struct {
	Source    string                       `json:"source"`
	Count     int                          `json:"count"`
	Freshness time.Time                    `json:"freshness"`
	Latest    *StateStatusRemoteDeployment `json:"latest,omitempty"`
}

type StateStatusBestDesired struct {
	Source     string    `json:"source"`
	RevisionID string    `json:"revisionId"`
	Freshness  time.Time `json:"freshness"`
}

type StateStatusBestActual struct {
	Source       string    `json:"source"`
	ServiceCount int       `json:"serviceCount"`
	Freshness    time.Time `json:"freshness"`
}

type StateStatusBestNodeActual struct {
	Node      string    `json:"node"`
	Source    string    `json:"source"`
	Freshness time.Time `json:"freshness"`
}

type StateStatusSyncSummary struct {
	Recommendations []string `json:"recommendations"`
}

type StateStatusCounts struct {
	ConfiguredNodes    int `json:"configuredNodes"`
	ReachableNodes     int `json:"reachableNodes"`
	UnreachableNodes   int `json:"unreachableNodes"`
	RemoteHistoryNodes int `json:"remoteHistoryNodes"`
	DesiredNodes       int `json:"desiredNodes"`
	ActualNodes        int `json:"actualNodes"`
}

// StateStatusHistoryCandidate is a candidate remote deployment history source.
type StateStatusHistoryCandidate struct {
	Source  string
	History *remotestate.DeploymentHistory
}

type stateStatusDesiredCandidate struct {
	source  string
	desired *takodstate.DesiredRevision
}

type stateStatusActualCandidate struct {
	source string
	actual *takodstate.ActualSnapshot
}

// StateStatusNodeActualCandidate is a candidate per-node actual state source.
type StateStatusNodeActualCandidate struct {
	Source string
	Node   string
	Actual *takodstate.ActualSnapshot
}

// StateStatus builds a machine-readable status result from adapter-collected
// local and remote state. It never prints or performs network I/O.
func (e *Engine) StateStatus(ctx context.Context, req StateStatusRequest) (*StateStatusResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	project := strings.TrimSpace(req.Project)
	if project == "" {
		return nil, invalidRequestf("state status request requires a project")
	}
	envName := strings.TrimSpace(req.Environment)
	if envName == "" {
		return nil, invalidRequestf("state status request requires an environment")
	}

	histories, desiredCandidates, actualCandidates, nodeActualCandidates := stateStatusCandidates(req.Nodes)
	bestHistory, hasRemoteHistory := bestStateStatusHistory(histories)
	bestDesired, hasDesired := bestStateStatusDesired(desiredCandidates)
	bestActual, hasActual, bestNodeActual := bestStateStatusActual(project, envName, actualCandidates, nodeActualCandidates)

	result := &StateStatusResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindStateStatusResult,
		Project:     project,
		Environment: envName,
		Server:      strings.TrimSpace(req.Server),
		Local:       summarizeStateStatusLocal(req.Local),
		Remote: StateStatusRemoteSummary{
			Title:               stateStatusRemoteTitle(req.Server),
			Nodes:               summarizeStateStatusNodes(req.Nodes),
			UnreachableGuidance: StateStatusUnreachableGuidance(req.Nodes),
		},
		BestKnown: summarizeStateStatusBestKnown(bestHistory, hasRemoteHistory, bestDesired, hasDesired, bestActual, hasActual, bestNodeActual),
		Sync: StateStatusSyncSummary{
			Recommendations: StateStatusSyncRecommendation(req.Local.Exists, req.Local.Current, bestHistory, hasRemoteHistory, StateStatusUnreachableCount(req.Nodes)),
		},
		Counts: StateStatusCounts{
			ConfiguredNodes:    len(req.Nodes),
			ReachableNodes:     StateStatusReachableCount(req.Nodes),
			UnreachableNodes:   StateStatusUnreachableCount(req.Nodes),
			RemoteHistoryNodes: len(histories),
			DesiredNodes:       len(desiredCandidates),
			ActualNodes:        len(actualCandidates),
		},
	}
	if result.Counts.ReachableNodes == 0 {
		result.Error = StateStatusNoReachableMessage(envName, req.Nodes)
		return result, errors.New(result.Error)
	}
	return result, nil
}

func stateStatusRemoteTitle(server string) string {
	if strings.TrimSpace(server) == "" {
		return "Remote Mesh State"
	}
	return "Remote State"
}

func summarizeStateStatusLocal(local StateStatusLocalInput) StateStatusLocalSummary {
	path := local.Path
	if path == "" {
		path = ".tako"
	}
	status := StateStatusLocalMissing
	if local.Exists {
		status = StateStatusLocalExists
	}
	summary := StateStatusLocalSummary{Path: path, Exists: local.Exists, Status: status, Error: local.Error}
	if local.Current != nil {
		summary.LastDeployment = &StateStatusLocalDeployment{
			ID:        local.Current.DeploymentID,
			Timestamp: local.Current.Timestamp,
			Status:    local.Current.Status,
			Commit:    local.Current.GitCommit,
		}
	}
	return summary
}

func summarizeStateStatusNodes(nodes []StateStatusRemoteNodeInput) []StateStatusNodeResult {
	out := make([]StateStatusNodeResult, 0, len(nodes))
	for _, node := range nodes {
		result := StateStatusNodeResult{Name: node.Name, Host: node.Host}
		if node.ConnectErr != nil {
			result.Status = StateStatusNodeUnreachable
			result.Error = node.ConnectErr.Error()
			out = append(out, result)
			continue
		}
		result.Status = StateStatusNodeReachable
		result.Reachable = true
		result.Agent = summarizeAgent(node.Agent, node.AgentErr)
		result.Mesh = summarizeMesh(node.Mesh, node.MeshErr)
		result.History = summarizeHistory(node.History, node.HistoryErr)
		result.Desired = summarizeDesired(node.Desired, node.DesiredErr)
		result.Actual = summarizeActual(node.Actual, node.ActualErr, len(node.NodeActual))
		result.Lease = summarizeLease(node.Lease, node.LeaseErr)
		out = append(out, result)
	}
	return out
}

func summarizeAgent(agent *StateStatusAgentSummary, err error) *StateStatusAgentSummary {
	if agent == nil {
		if err == nil {
			return &StateStatusAgentSummary{Error: "<nil>"}
		}
		return &StateStatusAgentSummary{Error: err.Error()}
	}
	out := *agent
	if err != nil {
		out.Error = err.Error()
	}
	return &out
}

func summarizeMesh(mesh *StateStatusMeshSummary, err error) *StateStatusMeshSummary {
	if mesh == nil && err == nil {
		return nil
	}
	if mesh == nil {
		return &StateStatusMeshSummary{Error: err.Error()}
	}
	out := *mesh
	if err != nil {
		out.Error = err.Error()
	}
	return &out
}

func summarizeHistory(history *remotestate.DeploymentHistory, err error) StateStatusHistorySummary {
	if !stateStatusHistoryHasDeployments(history) {
		summary := StateStatusHistorySummary{}
		if errors.Is(err, remotestate.ErrNotFound) {
			summary.Missing = true
		} else if err != nil {
			summary.Error = err.Error()
		}
		return summary
	}
	latest := stateStatusLatestDeploymentByTimestamp(history.Deployments)
	return StateStatusHistorySummary{Recorded: true, Count: stateStatusDeploymentHistoryCount(history), Freshness: stateStatusDeploymentHistoryFreshness(history), Latest: summarizeRemoteDeployment(latest)}
}

func summarizeDesired(desired *takodstate.DesiredRevision, err error) StateStatusDesiredSummary {
	if !stateStatusDesiredRevisionRepairable(desired) {
		if err != nil && !errors.Is(err, takodstate.ErrNotFound) {
			return StateStatusDesiredSummary{Error: err.Error()}
		}
		return StateStatusDesiredSummary{}
	}
	return StateStatusDesiredSummary{Recorded: true, RevisionID: desired.RevisionID, ServiceCount: len(desired.Services), Freshness: stateStatusDesiredRevisionFreshness(desired)}
}

func summarizeActual(actual *takodstate.ActualSnapshot, err error, nodeActualCount int) StateStatusActualSummary {
	summary := StateStatusActualSummary{NodeActualCount: nodeActualCount}
	if !stateStatusActualSnapshotRepairable(actual) {
		if err != nil && !errors.Is(err, takodstate.ErrNotFound) {
			summary.Error = err.Error()
		}
		return summary
	}
	summary.Recorded = true
	summary.ServiceCount = stateStatusActualSnapshotServiceCount(actual)
	summary.Freshness = stateStatusActualSnapshotFreshness(actual)
	return summary
}

func summarizeLease(lease *remotestate.LeaseInfo, err error) StateStatusLeaseSummary {
	if err != nil {
		return StateStatusLeaseSummary{Status: "error", Error: err.Error()}
	}
	if lease == nil {
		return StateStatusLeaseSummary{Status: "free"}
	}
	return StateStatusLeaseSummary{Status: "held", Lease: lease}
}

func summarizeStateStatusBestKnown(history StateStatusHistoryCandidate, hasHistory bool, desired stateStatusDesiredCandidate, hasDesired bool, actual stateStatusActualCandidate, hasActual bool, nodeActual map[string]StateStatusNodeActualCandidate) StateStatusBestKnown {
	best := StateStatusBestKnown{}
	if hasHistory {
		best.History = &StateStatusBestHistory{Source: history.Source, Count: stateStatusDeploymentHistoryCount(history.History), Freshness: stateStatusDeploymentHistoryFreshness(history.History), Latest: summarizeRemoteDeployment(stateStatusLatestDeploymentByTimestamp(history.History.Deployments))}
	}
	if hasDesired {
		best.Desired = &StateStatusBestDesired{Source: desired.source, RevisionID: desired.desired.RevisionID, Freshness: stateStatusDesiredRevisionFreshness(desired.desired)}
	}
	if hasActual {
		best.Actual = &StateStatusBestActual{Source: actual.source, ServiceCount: stateStatusActualSnapshotServiceCount(actual.actual), Freshness: stateStatusActualSnapshotFreshness(actual.actual)}
	}
	if len(nodeActual) > 0 {
		for _, nodeName := range sortedStateStatusNodeActualNames(nodeActual) {
			candidate := nodeActual[nodeName]
			best.NodeActual = append(best.NodeActual, StateStatusBestNodeActual{Node: nodeName, Source: candidate.Source, Freshness: stateStatusActualSnapshotFreshness(candidate.Actual)})
		}
	}
	return best
}

func summarizeRemoteDeployment(deployment *remotestate.DeploymentState) *StateStatusRemoteDeployment {
	if deployment == nil {
		return nil
	}
	commit := deployment.GitCommitShort
	if commit == "" {
		commit = deployment.GitCommit
	}
	return &StateStatusRemoteDeployment{ID: deployment.ID, DisplayID: remotestate.FormatDeploymentID(deployment.ID), Status: deployment.Status, Timestamp: deployment.Timestamp, User: deployment.User, Commit: commit}
}

func stateStatusCandidates(nodes []StateStatusRemoteNodeInput) ([]StateStatusHistoryCandidate, []stateStatusDesiredCandidate, []stateStatusActualCandidate, []StateStatusNodeActualCandidate) {
	histories := make([]StateStatusHistoryCandidate, 0, len(nodes))
	desired := make([]stateStatusDesiredCandidate, 0, len(nodes))
	actual := make([]stateStatusActualCandidate, 0, len(nodes))
	nodeActual := make([]StateStatusNodeActualCandidate, 0, len(nodes))
	configuredNodes := stateStatusConfiguredNodeSet(nodes)

	for _, node := range nodes {
		if stateStatusHistoryHasDeployments(node.History) {
			histories = append(histories, StateStatusHistoryCandidate{Source: node.Name, History: node.History})
		}
		if stateStatusDesiredRevisionRepairable(node.Desired) {
			desired = append(desired, stateStatusDesiredCandidate{source: node.Name, desired: node.Desired})
		}
		if stateStatusActualSnapshotRepairable(node.Actual) {
			actual = append(actual, stateStatusActualCandidate{source: node.Name, actual: node.Actual})
			for nodeName, embedded := range node.Actual.Nodes {
				if !stateStatusNodeConfigured(configuredNodes, nodeName) {
					continue
				}
				nodeActual = append(nodeActual, StateStatusNodeActualCandidate{Source: node.Name + " aggregate", Node: nodeName, Actual: stateStatusActualSnapshotFromEmbeddedNode(node.Actual.Project, node.Actual.Environment, embedded)})
			}
		}
		nodeActual = append(nodeActual, node.NodeActual...)
	}
	return histories, desired, actual, nodeActual
}

func stateStatusConfiguredNodeSet(nodes []StateStatusRemoteNodeInput) map[string]struct{} {
	configured := make(map[string]struct{})
	for _, node := range nodes {
		for _, nodeName := range node.EnvNodes {
			if nodeName != "" {
				configured[nodeName] = struct{}{}
			}
		}
	}
	if len(configured) > 0 {
		return configured
	}
	for _, node := range nodes {
		if node.Name != "" {
			configured[node.Name] = struct{}{}
		}
	}
	return configured
}

func stateStatusNodeConfigured(configured map[string]struct{}, nodeName string) bool {
	if len(configured) == 0 {
		return true
	}
	_, ok := configured[nodeName]
	return ok
}

func bestStateStatusHistory(candidates []StateStatusHistoryCandidate) (StateStatusHistoryCandidate, bool) {
	var best StateStatusHistoryCandidate
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

func bestStateStatusDesired(candidates []stateStatusDesiredCandidate) (stateStatusDesiredCandidate, bool) {
	var best stateStatusDesiredCandidate
	ok := false
	for _, candidate := range candidates {
		if !stateStatusDesiredRevisionRepairable(candidate.desired) {
			continue
		}
		if !ok || stateStatusDesiredRevisionBetter(candidate.desired, best.desired) {
			best = candidate
			ok = true
		}
	}
	return best, ok
}

func bestStateStatusActual(project string, envName string, actualCandidates []stateStatusActualCandidate, nodeActualCandidates []StateStatusNodeActualCandidate) (stateStatusActualCandidate, bool, map[string]StateStatusNodeActualCandidate) {
	bestActual, hasActual := bestStateStatusActualSnapshot(actualCandidates)
	bestNodeActual := bestStateStatusNodeActualSnapshots(nodeActualCandidates)
	if hasActual && len(bestNodeActual) > 0 {
		bestActual.actual = stateStatusActualSnapshotWithNodeSnapshots(bestActual.actual, bestNodeActual)
	} else if !hasActual && len(bestNodeActual) > 0 {
		bestActual = stateStatusActualCandidate{source: "node actual snapshots", actual: stateStatusAggregateActualSnapshotFromNodeSnapshots(project, envName, bestNodeActual)}
		hasActual = stateStatusActualSnapshotRepairable(bestActual.actual)
	}
	return bestActual, hasActual, bestNodeActual
}

func bestStateStatusActualSnapshot(candidates []stateStatusActualCandidate) (stateStatusActualCandidate, bool) {
	var best stateStatusActualCandidate
	ok := false
	for _, candidate := range candidates {
		if !stateStatusActualSnapshotRepairable(candidate.actual) {
			continue
		}
		if !ok || stateStatusActualSnapshotBetter(candidate.actual, best.actual) {
			best = candidate
			ok = true
		}
	}
	return best, ok
}

func bestStateStatusNodeActualSnapshots(candidates []StateStatusNodeActualCandidate) map[string]StateStatusNodeActualCandidate {
	best := make(map[string]StateStatusNodeActualCandidate)
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

func stateStatusActualSnapshotWithNodeSnapshots(snapshot *takodstate.ActualSnapshot, nodeActual map[string]StateStatusNodeActualCandidate) *takodstate.ActualSnapshot {
	if snapshot == nil {
		return nil
	}
	if len(nodeActual) == 0 {
		return snapshot
	}
	return stateStatusAggregateActualSnapshotFromNodeSnapshots(snapshot.Project, snapshot.Environment, nodeActual)
}

func stateStatusAggregateActualSnapshotFromNodeSnapshots(project string, environment string, nodeActual map[string]StateStatusNodeActualCandidate) *takodstate.ActualSnapshot {
	nodes := sortedStateStatusNodeActualNames(nodeActual)
	snapshot := &takodstate.ActualSnapshot{SchemaVersion: takodstate.SchemaVersion, Project: project, Environment: environment, TargetNodes: nodes, Services: make(map[string]takodstate.ActualService), Nodes: stateStatusActualNodeSnapshotMap(nodeActual)}
	var newest time.Time
	for _, nodeName := range nodes {
		candidate := nodeActual[nodeName]
		if candidate.Actual == nil {
			continue
		}
		if candidate.Actual.CapturedAt.After(newest) {
			newest = candidate.Actual.CapturedAt
		}
		for serviceName, service := range candidate.Actual.Services {
			if existing, ok := snapshot.Services[serviceName]; ok {
				existing.Replicas += service.Replicas
				existing.Containers = append(existing.Containers, service.Containers...)
				if existing.Image == "" {
					existing.Image = service.Image
				}
				if existing.ConfigHash == "" {
					existing.ConfigHash = service.ConfigHash
				} else if service.ConfigHash != "" && existing.ConfigHash != service.ConfigHash {
					existing.ConfigHash = ""
				}
				existing.RuntimeID = stateStatusMergeActualRuntimeID(existing.RuntimeID, service.RuntimeID)
				existing.Persistent = existing.Persistent || service.Persistent
				existing.CurrentRevision = stateStatusMergeActualOptionalLabel(existing.CurrentRevision, service.CurrentRevision)
				existing.PreviousRevision = stateStatusMergeActualOptionalLabel(existing.PreviousRevision, service.PreviousRevision)
				existing.DeployStrategy = stateStatusMergeActualOptionalLabel(existing.DeployStrategy, service.DeployStrategy)
				existing.ActiveContainers = append(existing.ActiveContainers, service.ActiveContainers...)
				existing.WarmingContainers = append(existing.WarmingContainers, service.WarmingContainers...)
				snapshot.Services[serviceName] = existing
				continue
			}
			snapshot.Services[serviceName] = service
		}
	}
	if newest.IsZero() {
		newest = time.Now().UTC()
	}
	snapshot.CapturedAt = newest
	return snapshot
}

func stateStatusActualNodeSnapshotMap(nodeActual map[string]StateStatusNodeActualCandidate) map[string]takodstate.ActualNodeSnapshot {
	if len(nodeActual) == 0 {
		return nil
	}
	out := make(map[string]takodstate.ActualNodeSnapshot, len(nodeActual))
	for _, nodeName := range sortedStateStatusNodeActualNames(nodeActual) {
		candidate := nodeActual[nodeName]
		if candidate.Actual == nil {
			continue
		}
		out[nodeName] = takodstate.ActualNodeSnapshot{Node: nodeName, Services: stateStatusCloneActualServices(candidate.Actual.Services), CapturedAt: candidate.Actual.CapturedAt}
	}
	return out
}

func stateStatusActualSnapshotFromEmbeddedNode(project string, environment string, snapshot takodstate.ActualNodeSnapshot) *takodstate.ActualSnapshot {
	return &takodstate.ActualSnapshot{SchemaVersion: takodstate.SchemaVersion, Project: project, Environment: environment, Node: snapshot.Node, Services: stateStatusCloneActualServices(snapshot.Services), CapturedAt: snapshot.CapturedAt}
}

func stateStatusCloneActualServices(services map[string]takodstate.ActualService) map[string]takodstate.ActualService {
	if len(services) == 0 {
		return nil
	}
	out := make(map[string]takodstate.ActualService, len(services))
	for name, service := range services {
		service.Containers = append([]string(nil), service.Containers...)
		service.ActiveContainers = append([]string(nil), service.ActiveContainers...)
		service.WarmingContainers = append([]string(nil), service.WarmingContainers...)
		out[name] = service
	}
	return out
}

func sortedStateStatusNodeActualNames(nodeActual map[string]StateStatusNodeActualCandidate) []string {
	nodes := make([]string, 0, len(nodeActual))
	for node := range nodeActual {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	return nodes
}

func stateStatusMergeActualRuntimeID(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" || a == b {
		return a
	}
	return ""
}

func stateStatusMergeActualOptionalLabel(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" || a == b {
		return a
	}
	return "mixed"
}

func stateStatusDeploymentHistoryBetter(candidate *remotestate.DeploymentHistory, current *remotestate.DeploymentHistory) bool {
	candidateFreshness := stateStatusDeploymentHistoryFreshness(candidate)
	currentFreshness := stateStatusDeploymentHistoryFreshness(current)
	if !candidateFreshness.Equal(currentFreshness) {
		return candidateFreshness.After(currentFreshness)
	}
	return stateStatusDeploymentHistoryCount(candidate) > stateStatusDeploymentHistoryCount(current)
}

func stateStatusDesiredRevisionBetter(candidate *takodstate.DesiredRevision, current *takodstate.DesiredRevision) bool {
	candidateFreshness := stateStatusDesiredRevisionFreshness(candidate)
	currentFreshness := stateStatusDesiredRevisionFreshness(current)
	if !candidateFreshness.Equal(currentFreshness) {
		return candidateFreshness.After(currentFreshness)
	}
	if candidate == nil || current == nil {
		return candidate != nil
	}
	return candidate.RevisionID > current.RevisionID
}

func stateStatusActualSnapshotBetter(candidate *takodstate.ActualSnapshot, current *takodstate.ActualSnapshot) bool {
	candidateFreshness := stateStatusActualSnapshotFreshness(candidate)
	currentFreshness := stateStatusActualSnapshotFreshness(current)
	if !candidateFreshness.Equal(currentFreshness) {
		return candidateFreshness.After(currentFreshness)
	}
	return stateStatusActualSnapshotServiceCount(candidate) > stateStatusActualSnapshotServiceCount(current)
}

func stateStatusHistoryHasDeployments(history *remotestate.DeploymentHistory) bool {
	return stateStatusDeploymentHistoryCount(history) > 0
}

func stateStatusDesiredRevisionRepairable(revision *takodstate.DesiredRevision) bool {
	return revision != nil && revision.RevisionID != "" && !revision.CreatedAt.IsZero()
}

func stateStatusActualSnapshotRepairable(snapshot *takodstate.ActualSnapshot) bool {
	return snapshot != nil && !snapshot.CapturedAt.IsZero()
}

func stateStatusNodeActualSnapshotRepairable(snapshot *takodstate.ActualSnapshot, node string) bool {
	if !stateStatusActualSnapshotRepairable(snapshot) {
		return false
	}
	return snapshot.Node == "" || snapshot.Node == node
}

func stateStatusDeploymentHistoryCount(history *remotestate.DeploymentHistory) int {
	if history == nil {
		return 0
	}
	count := 0
	for _, deployment := range history.Deployments {
		if deployment != nil {
			count++
		}
	}
	return count
}

func stateStatusDeploymentHistoryFreshness(history *remotestate.DeploymentHistory) time.Time {
	if history == nil {
		return time.Time{}
	}
	freshness := history.LastUpdated
	for _, deployment := range history.Deployments {
		if deployment != nil && deployment.Timestamp.After(freshness) {
			freshness = deployment.Timestamp
		}
	}
	return freshness
}

func stateStatusDesiredRevisionFreshness(revision *takodstate.DesiredRevision) time.Time {
	if revision == nil {
		return time.Time{}
	}
	return revision.CreatedAt
}

func stateStatusActualSnapshotFreshness(snapshot *takodstate.ActualSnapshot) time.Time {
	if snapshot == nil {
		return time.Time{}
	}
	return snapshot.CapturedAt
}

func stateStatusActualSnapshotServiceCount(snapshot *takodstate.ActualSnapshot) int {
	if snapshot == nil {
		return 0
	}
	return len(snapshot.Services)
}

func stateStatusLatestDeploymentByTimestamp(deployments []*remotestate.DeploymentState) *remotestate.DeploymentState {
	var latest *remotestate.DeploymentState
	for _, deployment := range deployments {
		if deployment == nil {
			continue
		}
		if latest == nil || deployment.Timestamp.After(latest.Timestamp) {
			latest = deployment
		}
	}
	return latest
}

// StateStatusSyncRecommendation returns human-readable sync guidance used by
// both text rendering and machine result documents.
func StateStatusSyncRecommendation(localExists bool, localCurrent *localstate.DeploymentState, bestHistory StateStatusHistoryCandidate, hasRemoteHistory bool, unreachableCount int) []string {
	lines := make([]string, 0, 4)
	if !localExists {
		lines = append(lines, "Local state is missing.")
		if hasRemoteHistory {
			lines = append(lines, fmt.Sprintf("Remote deployment history is available from %s.", bestHistory.Source))
			lines = append(lines, "Run 'tako state pull' to sync from the freshest reachable node.")
			return lines
		}
		lines = append(lines, "No remote deployment history was found on reachable nodes.")
		return lines
	}

	lines = append(lines, "Local state exists.")
	if localCurrent == nil {
		lines = append(lines, "No current local deployment is recorded.")
		if hasRemoteHistory {
			lines = append(lines, fmt.Sprintf("Remote deployment history is available from %s.", bestHistory.Source))
			lines = append(lines, "Run 'tako state pull' to sync from the freshest reachable node.")
		} else {
			lines = append(lines, "No remote deployment history was found on reachable nodes.")
		}
		return lines
	}

	if !hasRemoteHistory {
		lines = append(lines, "No remote deployment history was found on reachable nodes; local deployment records are the best known copy.")
		if unreachableCount > 0 {
			lines = append(lines, "Some checked nodes are unreachable; restore reachability or remove destroyed nodes from config before pulling state.")
		} else {
			lines = append(lines, "Run 'tako deploy --yes' to reconcile the reachable mesh and publish fresh deployment state.")
		}
		return lines
	}

	remoteLatest := stateStatusLatestDeploymentByTimestamp(bestHistory.History.Deployments)
	if remoteLatest == nil {
		lines = append(lines, "No remote deployment history was found on reachable nodes; local deployment records are the best known copy.")
		if unreachableCount > 0 {
			lines = append(lines, "Some checked nodes are unreachable; restore reachability or remove destroyed nodes from config before pulling state.")
		} else {
			lines = append(lines, "Run 'tako deploy --yes' to reconcile the reachable mesh and publish fresh deployment state.")
		}
		return lines
	}

	if localCurrent.DeploymentID == remoteLatest.ID {
		lines = append(lines, fmt.Sprintf("Local deployment records match the freshest reachable remote deployment from %s.", bestHistory.Source))
		lines = append(lines, "No state pull needed.")
		return lines
	}
	if stateStatusDeploymentsEquivalentExceptID(localCurrent, remoteLatest) {
		lines = append(lines, fmt.Sprintf("Local and remote deployment records describe the same deployment from %s, but use different ID formats.", bestHistory.Source))
		lines = append(lines, "No state pull needed.")
		return lines
	}

	if !localCurrent.Timestamp.IsZero() && !remoteLatest.Timestamp.IsZero() {
		if localCurrent.Timestamp.Before(remoteLatest.Timestamp) {
			lines = append(lines, fmt.Sprintf("Remote deployment history from %s is newer than local state.", bestHistory.Source))
			lines = append(lines, "Run 'tako state pull' to refresh local deployment records.")
			return lines
		}
		if localCurrent.Timestamp.After(remoteLatest.Timestamp) {
			lines = append(lines, fmt.Sprintf("Local deployment records are newer than the freshest reachable remote history from %s.", bestHistory.Source))
			if unreachableCount > 0 {
				lines = append(lines, "Some checked nodes are unreachable; restore reachability or remove destroyed nodes from config before pulling state.")
			} else {
				lines = append(lines, "All checked nodes are reachable, so remote deployment history appears stale.")
				lines = append(lines, "Run 'tako deploy --yes' to reconcile the mesh and publish fresh state; avoid 'tako state pull' unless you intend to replace local records.")
			}
			return lines
		}
	}

	lines = append(lines, fmt.Sprintf("Local and remote deployment timestamps match, but deployment IDs differ from %s.", bestHistory.Source))
	lines = append(lines, "Run 'tako state pull' to normalize local deployment records.")
	return lines
}

func stateStatusDeploymentsEquivalentExceptID(localCurrent *localstate.DeploymentState, remoteLatest *remotestate.DeploymentState) bool {
	if localCurrent == nil || remoteLatest == nil {
		return false
	}
	if !stateStatusDeploymentTimestampsEquivalent(localCurrent.Timestamp, remoteLatest.Timestamp) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(localCurrent.Status), strings.TrimSpace(string(remoteLatest.Status))) {
		return false
	}
	if !stateStatusDeploymentCommitsEquivalent(localCurrent.GitCommit, remoteLatest.GitCommit, remoteLatest.GitCommitShort) {
		return false
	}
	return stateStatusDeploymentServicesEquivalent(localCurrent.Services, remoteLatest.Services)
}

func stateStatusDeploymentTimestampsEquivalent(localTime time.Time, remoteTime time.Time) bool {
	if localTime.IsZero() || remoteTime.IsZero() {
		return false
	}
	return localTime.UTC().Truncate(time.Second).Equal(remoteTime.UTC().Truncate(time.Second))
}

func stateStatusDeploymentCommitsEquivalent(localCommit string, remoteCommit string, remoteShort string) bool {
	localCommit = strings.TrimSpace(localCommit)
	remoteCommit = strings.TrimSpace(remoteCommit)
	remoteShort = strings.TrimSpace(remoteShort)
	if localCommit == "" || (remoteCommit == "" && remoteShort == "") {
		return true
	}
	if remoteCommit != "" && localCommit == remoteCommit {
		return true
	}
	if remoteShort != "" && strings.HasPrefix(localCommit, remoteShort) {
		return true
	}
	if remoteCommit != "" && strings.HasPrefix(remoteCommit, localCommit) {
		return true
	}
	return false
}

func stateStatusDeploymentServicesEquivalent(localServices map[string]*localstate.ServiceDeploy, remoteServices map[string]remotestate.ServiceState) bool {
	if len(localServices) == 0 || len(remoteServices) == 0 {
		return true
	}
	if len(localServices) != len(remoteServices) {
		return false
	}
	for name, localService := range localServices {
		remoteService, ok := remoteServices[name]
		if !ok || localService == nil {
			return false
		}
		if strings.TrimSpace(localService.Image) != "" && strings.TrimSpace(remoteService.Image) != "" && localService.Image != remoteService.Image {
			return false
		}
		if localService.Replicas != 0 && remoteService.Replicas != 0 && localService.Replicas != remoteService.Replicas {
			return false
		}
	}
	return true
}

// StateStatusUnreachableGuidance returns operator guidance for unreachable nodes.
func StateStatusUnreachableGuidance(nodes []StateStatusRemoteNodeInput) []string {
	names := make([]string, 0)
	for _, node := range nodes {
		if node.ConnectErr != nil {
			names = append(names, node.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	nodeList := strings.Join(names, ", ")
	if len(names) == 1 {
		return []string{
			fmt.Sprintf("Unreachable node: %s", nodeList),
			fmt.Sprintf("  Destroyed node: remove %s from tako.yaml, then run 'tako state forget-node %s --yes' and 'tako deploy --yes'.", names[0], names[0]),
			fmt.Sprintf("  Rebuilt same-name node: keep %s in tako.yaml, then run 'tako setup --server %s', 'tako upgrade servers --server %s', 'tako state repair', and 'tako deploy --yes'.", names[0], names[0], names[0]),
		}
	}
	return []string{
		fmt.Sprintf("Unreachable nodes: %s", nodeList),
		"  Destroyed nodes: remove them from tako.yaml, then run 'tako state forget-node <node> --yes' for each removed node and 'tako deploy --yes'.",
		"  Rebuilt same-name nodes: keep them in tako.yaml, then run 'tako setup --server <node>', 'tako upgrade servers --server <node>', 'tako state repair', and 'tako deploy --yes'.",
	}
}

// StateStatusReachableCount returns the number of reachable remote nodes.
func StateStatusReachableCount(nodes []StateStatusRemoteNodeInput) int {
	reachable := 0
	for _, node := range nodes {
		if node.ConnectErr == nil {
			reachable++
		}
	}
	return reachable
}

// StateStatusUnreachableCount returns the number of unreachable remote nodes.
func StateStatusUnreachableCount(nodes []StateStatusRemoteNodeInput) int {
	return len(nodes) - StateStatusReachableCount(nodes)
}

// StateStatusNoReachableMessage formats the fail-closed no-reachable-nodes error.
func StateStatusNoReachableMessage(envName string, nodes []StateStatusRemoteNodeInput) string {
	details := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.ConnectErr != nil {
			details = append(details, fmt.Sprintf("%s: %v", node.Name, node.ConnectErr))
		}
	}
	sort.Strings(details)
	message := fmt.Sprintf("no reachable environment nodes for %s; deploy will fail closed until SSH/network is restored or the environment config is updated", envName)
	if len(details) == 0 {
		return message
	}
	return fmt.Sprintf("%s: %s", message, strings.Join(details, "; "))
}
