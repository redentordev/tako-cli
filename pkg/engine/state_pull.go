package engine

import (
	"context"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
)

const (
	// KindStatePullResult identifies a serialized state pull result document.
	KindStatePullResult = "StatePullResult"

	StatePullStatusSyncedHistory    = "synced_history"
	StatePullStatusRecoveredActual  = "recovered_mesh_actual"
	StatePullStatusRecoveredRunning = "recovered_running_mesh"
	StatePullStatusNoneFound        = "none_found"
)

// StatePullHistorySourceFunc supplies the selected deployment history source.
type StatePullHistorySourceFunc func() (source string, history *remotestate.DeploymentHistory, err error)

// StatePullSyncDeploymentsFunc persists remote deployment records locally.
type StatePullSyncDeploymentsFunc func([]*remotestate.DeploymentState) (int, error)

// StatePullRecoverFunc attempts one runtime-state recovery path and returns
// the number of services in the recovered local deployment when available.
type StatePullRecoverFunc func() (StatePullRecoveryResult, error)

// StatePullRecoveryResult summarizes a successful recovery seam.
type StatePullRecoveryResult struct {
	ServiceCount int `json:"serviceCount,omitempty"`
}

// StatePullRequest describes one state pull orchestration. Config must be
// loaded and Environment must be resolved by the adapter.
type StatePullRequest struct {
	Config      *config.Config `json:"-"`
	Environment string         `json:"environment"`
	// Server is the optional requested server filter from --server.
	Server string `json:"server,omitempty"`

	HistorySource          StatePullHistorySourceFunc   `json:"-"`
	SyncDeployments        StatePullSyncDeploymentsFunc `json:"-"`
	RecoverFromMeshActual  StatePullRecoverFunc         `json:"-"`
	RecoverFromRunningMesh StatePullRecoverFunc         `json:"-"`
}

// StatePullResult is the serializable outcome of StatePull.
type StatePullResult struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Server      string `json:"server,omitempty"`

	Status       string                     `json:"status"`
	SourceServer string                     `json:"sourceServer,omitempty"`
	SyncedCount  int                        `json:"syncedCount,omitempty"`
	Recovered    *StatePullRecoveryResult   `json:"recovered,omitempty"`
	Latest       *StatePullLatestDeployment `json:"latestDeployment,omitempty"`

	Warnings         []string `json:"warnings,omitempty"`
	MeshActualError  string   `json:"meshActualError,omitempty"`
	RunningMeshError string   `json:"runningMeshError,omitempty"`
}

// StatePullLatestDeployment is the JSON-friendly latest deployment summary.
type StatePullLatestDeployment struct {
	ID        string                       `json:"id"`
	DisplayID string                       `json:"displayId,omitempty"`
	Status    remotestate.DeploymentStatus `json:"status"`
	Timestamp time.Time                    `json:"timestamp"`
	User      string                       `json:"user,omitempty"`
	Commit    string                       `json:"commit,omitempty"`
}

// StatePull refreshes local deployment state from the best remote deployment
// history, falling back to runtime-state recovery. It never renders output;
// adapters provide all I/O through seams and render the returned result.
func (e *Engine) StatePull(ctx context.Context, req StatePullRequest) (*StatePullResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Config == nil {
		return nil, invalidRequestf("state pull request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("state pull request requires an environment")
	}
	if req.HistorySource == nil {
		return nil, invalidRequestf("state pull request requires a history source")
	}
	if req.SyncDeployments == nil {
		return nil, invalidRequestf("state pull request requires a deployment sync function")
	}
	if req.RecoverFromMeshActual == nil {
		return nil, invalidRequestf("state pull request requires a mesh-actual recovery function")
	}
	if req.RecoverFromRunningMesh == nil {
		return nil, invalidRequestf("state pull request requires a running-mesh recovery function")
	}

	cfg := req.Config
	result := &StatePullResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindStatePullResult,
		Project:     cfg.Project.Name,
		Environment: strings.TrimSpace(req.Environment),
		Server:      strings.TrimSpace(req.Server),
	}

	source, history, err := req.HistorySource()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if historyHasDeployments(history) {
		result.SourceServer = source
		synced, err := req.SyncDeployments(history.Deployments)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result.Status = StatePullStatusSyncedHistory
		result.SyncedCount = synced
		result.Latest = statePullLatestDeployment(history.Deployments)
		return result, nil
	}

	meshRecovery, meshErr := req.RecoverFromMeshActual()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if meshErr == nil {
		result.Status = StatePullStatusRecoveredActual
		result.Recovered = &meshRecovery
		return result, nil
	}
	result.MeshActualError = meshErr.Error()
	result.Warnings = append(result.Warnings, "mesh runtime state recovery failed: "+meshErr.Error())

	runningRecovery, runningErr := req.RecoverFromRunningMesh()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runningErr == nil {
		result.Status = StatePullStatusRecoveredRunning
		result.Recovered = &runningRecovery
		return result, nil
	}
	result.RunningMeshError = runningErr.Error()
	result.Warnings = append(result.Warnings, "running service recovery failed: "+runningErr.Error())
	result.Status = StatePullStatusNoneFound
	return result, nil
}

func historyHasDeployments(history *remotestate.DeploymentHistory) bool {
	if history == nil {
		return false
	}
	for _, deployment := range history.Deployments {
		if deployment != nil {
			return true
		}
	}
	return false
}

func statePullLatestDeployment(deployments []*remotestate.DeploymentState) *StatePullLatestDeployment {
	var latest *remotestate.DeploymentState
	for _, deployment := range deployments {
		if deployment == nil {
			continue
		}
		if latest == nil || deployment.Timestamp.After(latest.Timestamp) {
			latest = deployment
		}
	}
	if latest == nil {
		return nil
	}
	return &StatePullLatestDeployment{
		ID:        latest.ID,
		DisplayID: remotestate.FormatDeploymentID(latest.ID),
		Status:    latest.Status,
		Timestamp: latest.Timestamp,
		User:      latest.User,
		Commit:    latest.GitCommitShort,
	}
}
