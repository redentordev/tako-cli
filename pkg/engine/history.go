package engine

import (
	"context"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
)

// KindHistoryResult identifies a serialized deployment history result document.
const KindHistoryResult = "HistoryResult"

// HistoryRequest describes one deployment-history read operation. Config must
// be loaded and Environment must be resolved by the adapter.
type HistoryRequest struct {
	Config      *config.Config
	Environment string
	// Server is the optional requested server filter from --server.
	Server string
	Limit  int
	// Status is the optional requested deployment status filter from --status.
	Status        string
	IncludeFailed bool

	// HistorySource and ListDeployments are seams for history helpers shared
	// with cmd/state.go and rollback; see the type docs in rollback.go.
	HistorySource   RollbackHistorySourceFunc
	ListDeployments ListDeploymentsFunc
}

// HistoryResult is the serializable outcome of History.
type HistoryResult struct {
	APIVersion    string              `json:"apiVersion"`
	Kind          string              `json:"kind"`
	Project       string              `json:"project"`
	Environment   string              `json:"environment"`
	SourceServer  string              `json:"sourceServer,omitempty"`
	Server        string              `json:"server,omitempty"`
	Status        string              `json:"status,omitempty"`
	Limit         int                 `json:"limit,omitempty"`
	IncludeFailed bool                `json:"includeFailed"`
	Deployments   []HistoryDeployment `json:"deployments"`
}

// HistoryDeployment is one deployment row in a HistoryResult.
type HistoryDeployment struct {
	ID              string                       `json:"id"`
	DisplayID       string                       `json:"displayId,omitempty"`
	Commit          string                       `json:"commit,omitempty"`
	Timestamp       time.Time                    `json:"timestamp"`
	Version         string                       `json:"version,omitempty"`
	Status          remotestate.DeploymentStatus `json:"status"`
	DurationSeconds float64                      `json:"durationSeconds"`
	Duration        string                       `json:"duration,omitempty"`
	Message         string                       `json:"message,omitempty"`
	Error           string                       `json:"error,omitempty"`
}

// History returns deployment history rows selected from the freshest reachable
// mesh history source. It performs no rendering or prompting.
func (e *Engine) History(ctx context.Context, req HistoryRequest) (*HistoryResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Config == nil {
		return nil, invalidRequestf("history request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("history request requires an environment")
	}
	if req.HistorySource == nil {
		return nil, invalidRequestf("history request requires a history source")
	}
	if req.ListDeployments == nil {
		return nil, invalidRequestf("history request requires a deployment lister")
	}

	cfg := req.Config
	serverFilter := strings.TrimSpace(req.Server)
	statusFilter := strings.TrimSpace(req.Status)
	result := &HistoryResult{
		APIVersion:    takoapi.APIVersionCurrent,
		Kind:          KindHistoryResult,
		Project:       cfg.Project.Name,
		Environment:   strings.TrimSpace(req.Environment),
		Server:        serverFilter,
		Status:        statusFilter,
		Limit:         req.Limit,
		IncludeFailed: req.IncludeFailed,
		Deployments:   []HistoryDeployment{},
	}

	opts := &remotestate.HistoryOptions{
		Limit:         req.Limit,
		IncludeFailed: req.IncludeFailed,
	}
	if statusFilter != "" {
		opts.Status = remotestate.DeploymentStatus(statusFilter)
	}

	source, history, err := req.HistorySource()
	if err != nil {
		return nil, err
	}
	result.SourceServer = source
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if history == nil {
		return result, nil
	}

	deployments := req.ListDeployments(history, opts)
	result.Deployments = make([]HistoryDeployment, 0, len(deployments))
	for _, dep := range deployments {
		if dep == nil {
			continue
		}
		result.Deployments = append(result.Deployments, HistoryDeployment{
			ID:              dep.ID,
			DisplayID:       remotestate.FormatDeploymentID(dep.ID),
			Commit:          dep.GitCommitShort,
			Timestamp:       dep.Timestamp,
			Version:         dep.Version,
			Status:          dep.Status,
			DurationSeconds: dep.Duration.Seconds(),
			Duration:        remotestate.FormatDuration(dep.Duration),
			Message:         dep.GitCommitMsg,
			Error:           dep.Error,
		})
	}
	return result, nil
}
