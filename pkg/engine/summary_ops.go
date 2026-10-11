package engine

import (
	"context"
	"fmt"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
)

// summaryHealthProbe runs the post-operation health read for a summary.
// Nil skips the read (health unknown).
type summaryHealthProbe func() (map[string]serviceHealth, error)

// rollbackSummaryInput carries a rollback's summary inputs, for both scopes.
type rollbackSummaryInput struct {
	Project     string
	Environment string
	StartedAt   time.Time
	Duration    float64
	// DeploymentID is the history record the rollback wrote, when written.
	DeploymentID string
	Commit       string
	// Service is set for a single-service rollback.
	Service     string
	Services    map[string]config.ServiceConfig
	Outcomes    []RollbackServiceOutcome
	Failed      bool
	Error       string
	Notes       []string
	Target      *remotestate.DeploymentState
	RollForward *remotestate.DeploymentState
	Health      summaryHealthProbe
}

// buildRollbackSummary builds a rollback OperationSummary. The undo rolls
// forward to the stable record that served before the rollback.
func buildRollbackSummary(redact func(string) string, in rollbackSummaryInput) *OperationSummary {
	builder := newSummaryBuilder(redact, SummaryOperationRollback, in.Project, in.Environment, in.StartedAt)
	builder.setRecord(in.DeploymentID)
	builder.setDuration(in.Duration)
	revision := ""
	if in.Target != nil {
		revision = in.Target.Version
	}
	builder.setRevision(revision, in.Commit)
	for _, outcome := range in.Outcomes {
		service, known := in.Services[outcome.Name]
		row := SummaryService{
			Name:      outcome.Name,
			Change:    outcome.Action,
			FromImage: outcome.FromImage,
			ToImage:   outcome.ToImage,
			Reason:    outcome.Reason,
			Error:     outcome.Error,
		}
		if known {
			row.Strategy, _ = serviceStrategyReport(&service)
			if service.IsRun() || service.IsJob() {
				row.Health = SummaryHealthNA
			}
		}
		switch outcome.Action {
		case RollbackActionNotAttempted:
			row.Health = SummaryHealthNA
		case RollbackActionUnchanged:
			if row.FromImage == "" {
				row.FromImage = row.ToImage
			}
		case RollbackActionFailed:
			builder.addFailedService(row, outcome.Recovery != nil && outcome.Recovery.Status != deployer.RecoveryRestored)
			continue
		}
		builder.addService(row)
	}
	if in.Failed {
		builder.setFailed(in.Error)
	}
	for _, note := range in.Notes {
		builder.addNote(note)
	}
	if in.Health != nil && builder.changedSomething() {
		builder.applyHealth(in.Health())
	}
	builder.setUndo(undoForRollback(builder.environment(), builder.outcome(), in.Service, in.RollForward, in.Target))
	return builder.build()
}

// promoteSummaryInput carries a promotion's summary inputs.
type promoteSummaryInput struct {
	Project      string
	Environment  string
	StartedAt    time.Time
	Duration     float64
	DeploymentID string
	Service      string
	Strategy     string
	FromImage    string
	ToImage      string
	FromRevision string
	ToRevision   string
	// Retained reports whether the post-promotion state still retains
	// FromRevision as a promotable revision.
	Retained   bool
	Fallback   *remotestate.DeploymentState
	HistoryErr error
	Health     summaryHealthProbe
}

func buildPromoteSummary(redact func(string) string, in promoteSummaryInput) *OperationSummary {
	builder := newSummaryBuilder(redact, SummaryOperationPromote, in.Project, in.Environment, in.StartedAt)
	builder.setRecord(in.DeploymentID)
	builder.setDuration(in.Duration)
	builder.setRevision(in.ToRevision, "")
	builder.addService(SummaryService{
		Name:         in.Service,
		Change:       SummaryChangePromoted,
		Strategy:     in.Strategy,
		FromImage:    in.FromImage,
		ToImage:      in.ToImage,
		FromRevision: in.FromRevision,
		ToRevision:   in.ToRevision,
	})
	if in.Health != nil {
		builder.applyHealth(in.Health())
	}
	undo := undoForPromote(builder.environment(), in.Service, in.FromRevision, in.Retained, in.Fallback, in.HistoryErr)
	builder.setUndo(undo)
	if undo.Unavailable == UndoUnavailableHistory {
		builder.addNextStep("deployment history could not be read", ShellJoin(append([]string{"tako", "history"}, summaryEnvironmentArgs(builder.environment())...)))
	}
	return builder.build()
}

// scaleSummaryInput carries a scale's summary inputs.
type scaleSummaryInput struct {
	Project      string
	Environment  string
	StartedAt    time.Time
	Duration     float64
	DeploymentID string
	Services     map[string]config.ServiceConfig
	Outcomes     []ServiceOutcome
	Previous     map[string]int
	Failed       bool
	Error        string
	Health       summaryHealthProbe
}

func buildScaleSummary(redact func(string) string, in scaleSummaryInput) *OperationSummary {
	builder := newSummaryBuilder(redact, SummaryOperationScale, in.Project, in.Environment, in.StartedAt)
	builder.setRecord(in.DeploymentID)
	builder.setDuration(in.Duration)
	var changed []string
	for _, outcome := range in.Outcomes {
		service := in.Services[outcome.Name]
		row := SummaryService{Name: outcome.Name, FromImage: outcome.Image, ToImage: outcome.Image, Error: outcome.Error}
		row.Strategy, _ = serviceStrategyReport(&service)
		previous := in.Previous[outcome.Name]
		if outcome.Action == OutcomeFailed {
			changed = append(changed, outcome.Name)
			row.Reason = fmt.Sprintf("scaling %d → %d replicas failed", previous, outcome.Replicas)
			builder.addFailedService(row, true)
			continue
		}
		if previous == outcome.Replicas {
			row.Change = SummaryChangeUnchanged
		} else {
			row.Change = SummaryChangeScaled
			row.Reason = fmt.Sprintf("%d → %d replicas", previous, outcome.Replicas)
			changed = append(changed, outcome.Name)
		}
		builder.addService(row)
	}
	if in.Failed {
		builder.setFailed(in.Error)
	}
	if in.Health != nil && builder.changedSomething() {
		builder.applyHealth(in.Health())
	}
	builder.setUndo(undoForScale(builder.environment(), in.Previous, changed))
	return builder.build()
}

// healthProbe adapts Engine.summaryHealth to a summaryHealthProbe with the
// read error redacted.
func (e *Engine) healthProbe(ctx context.Context, factory *nodeclient.Factory, cfg *config.Config, envName string, selected []string, services map[string]config.ServiceConfig) summaryHealthProbe {
	return func() (map[string]serviceHealth, error) {
		health, err := e.summaryHealth(ctx, factory, cfg, envName, selected, services)
		if err != nil {
			return nil, fmt.Errorf("%s", e.RedactSensitive(err.Error()))
		}
		return health, nil
	}
}
