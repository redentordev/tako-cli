package engine

import (
	"fmt"
	"sort"
	"strings"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/takoapi"
)

// Rollback scopes reported in RollbackResult.Scope.
const (
	RollbackScopeService = "service"
	RollbackScopeProject = "project"
)

// RollbackStatusUpToDate is the project rollback status when every service
// already matches the target; nothing was mutated or recorded.
const RollbackStatusUpToDate takoapi.DeploymentStatus = "up_to_date"

// Per-service rollback actions reported in RollbackServiceOutcome.Action.
const (
	RollbackActionRolledBack   = "rolled_back"
	RollbackActionUnchanged    = "unchanged"
	RollbackActionSkipped      = "skipped"
	RollbackActionFailed       = "failed"
	RollbackActionNotAttempted = "not_attempted"
	// rollbackActionUnresolvable marks a service whose pre-target state was
	// pruned from history. It never reaches a document: planning fails.
	rollbackActionUnresolvable = "unresolvable"
)

// Image sources chosen by the rollback image preflight.
const (
	RollbackImageRetained = "retained"
	RollbackImageRebuild  = "rebuild"
	RollbackImagePull     = "pull"
)

// Skip reasons reported for project rollback services.
const (
	RollbackReasonKindRun    = "kind: run"
	RollbackReasonKindJob    = "kind: job"
	RollbackReasonPersistent = "persistent"
	RollbackReasonNotPresent = "not present at target"
)

// minRollbackCommitPrefix is the shortest git commit prefix accepted as a
// rollback target.
const minRollbackCommitPrefix = 7

// SelectProjectRollbackTarget resolves the target deployment of a project
// rollback. With an empty argument it returns the newest stable record that
// is strictly older than the newest record, whatever that record's status.
// Otherwise the argument resolves as an exact deployment ID, then a unique
// ID prefix (the display ID `tako history` prints), then a git commit. Every
// rejection is an InvalidRequestError.
func SelectProjectRollbackTarget(history *remotestate.DeploymentHistory, argument string, listDeployments ListDeploymentsFunc) (*remotestate.DeploymentState, error) {
	if listDeployments == nil {
		return nil, invalidRequestf("deployment lister is required")
	}
	deployments := listDeployments(history, &remotestate.HistoryOptions{Limit: 0, IncludeFailed: true})
	argument = strings.TrimSpace(argument)
	if argument == "" {
		for index, deployment := range deployments {
			if index == 0 || deployment == nil {
				continue
			}
			if isRollbackStableStatus(deployment.Status) {
				return deployment, nil
			}
		}
		return nil, invalidRequestf("no previous stable deployment found to roll back to; run 'tako history' to review deployments")
	}
	deployment, err := resolveRollbackTargetArgument(deployments, argument, nil)
	if err != nil {
		return nil, &InvalidRequestError{Err: err}
	}
	if !isRollbackStableStatus(deployment.Status) {
		return nil, invalidRequestf("deployment %s has status %s and is not a stable rollback target", deployment.ID, deployment.Status)
	}
	return deployment, nil
}

// resolveRollbackTargetArgument resolves an explicit rollback target among
// deployments ordered newest first: exact ID, unique ID prefix, then git
// commit. accept, when set, limits which records a commit may match.
func resolveRollbackTargetArgument(deployments []*remotestate.DeploymentState, argument string, accept func(*remotestate.DeploymentState) bool) (*remotestate.DeploymentState, error) {
	for _, deployment := range deployments {
		if deployment != nil && deployment.ID == argument {
			return deployment, nil
		}
	}

	var prefixed []*remotestate.DeploymentState
	for _, deployment := range deployments {
		if deployment != nil && strings.HasPrefix(deployment.ID, argument) {
			prefixed = append(prefixed, deployment)
		}
	}
	switch {
	case len(prefixed) == 1:
		return prefixed[0], nil
	case len(prefixed) > 1:
		ids := make([]string, 0, len(prefixed))
		for _, deployment := range prefixed {
			ids = append(ids, deployment.ID)
		}
		return nil, fmt.Errorf("deployment ID prefix %s is ambiguous; it matches %s", argument, strings.Join(ids, ", "))
	}

	var commitMatches []*remotestate.DeploymentState
	for _, deployment := range deployments {
		if deployment == nil || !rollbackCommitMatches(deployment, argument) {
			continue
		}
		if accept != nil && !accept(deployment) {
			continue
		}
		commitMatches = append(commitMatches, deployment)
	}
	for _, deployment := range commitMatches {
		if isRollbackStableStatus(deployment.Status) {
			return deployment, nil
		}
	}
	if len(commitMatches) > 0 {
		newest := commitMatches[0]
		return nil, fmt.Errorf("no stable deployment found for commit %s; newest match %s has status %s and is not a stable rollback target", argument, newest.ID, newest.Status)
	}
	return nil, fmt.Errorf("deployment %s not found (no deployment ID, ID prefix, or git commit matches)", argument)
}

func rollbackCommitMatches(deployment *remotestate.DeploymentState, argument string) bool {
	full := strings.TrimSpace(deployment.GitCommit)
	short := strings.TrimSpace(deployment.GitCommitShort)
	if full != "" && full == argument {
		return true
	}
	if len(argument) < minRollbackCommitPrefix {
		return false
	}
	return (full != "" && strings.HasPrefix(full, argument)) || (short != "" && strings.HasPrefix(short, argument))
}

// RollbackServicePlan is one service's reconstructed target in a project
// rollback before the image preflight runs.
type RollbackServicePlan struct {
	Name   string
	Action string
	Reason string
	// State is the target ServiceState; set for rolled_back and unchanged.
	State *remotestate.ServiceState
	// SourceDeploymentID is the record State was taken from.
	SourceDeploymentID string
}

// ReconstructServiceTargets derives each configured service's target state
// at the target deployment. Records hold only the services an operation
// touched, so a service's target is its entry in the newest stable record at
// or before the target. Planned services start as rolled_back; unchanged
// detection runs afterwards against actual state.
func ReconstructServiceTargets(history *remotestate.DeploymentHistory, target *remotestate.DeploymentState, services map[string]config.ServiceConfig, includePersistent bool) []RollbackServicePlan {
	var records []*remotestate.DeploymentState
	if history != nil {
		for _, deployment := range history.Deployments {
			if deployment != nil {
				records = append(records, deployment)
			}
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].Timestamp.After(records[j].Timestamp)
	})
	pruned := len(records) >= remotestate.MaxHistoryEntries

	names := sortedServiceNames(services)
	plans := make([]RollbackServicePlan, 0, len(names))
	for _, name := range names {
		service := services[name]
		plan := RollbackServicePlan{Name: name}
		switch {
		case service.IsRun():
			plan.Action, plan.Reason = RollbackActionSkipped, RollbackReasonKindRun
		case service.IsJob():
			plan.Action, plan.Reason = RollbackActionSkipped, RollbackReasonKindJob
		case service.Persistent && !includePersistent:
			plan.Action, plan.Reason = RollbackActionSkipped, RollbackReasonPersistent
		default:
			reconstructServiceTarget(&plan, records, target, pruned)
		}
		plans = append(plans, plan)
	}
	return plans
}

func reconstructServiceTarget(plan *RollbackServicePlan, records []*remotestate.DeploymentState, target *remotestate.DeploymentState, pruned bool) {
	newer := false
	for _, record := range records {
		state, ok := record.Services[plan.Name]
		if !ok {
			continue
		}
		if record.ID != target.ID && record.Timestamp.After(target.Timestamp) {
			newer = true
			continue
		}
		if !isRollbackStableStatus(record.Status) || state.Kind == string(config.ServiceKindRun) {
			continue
		}
		stateCopy := state
		plan.Action = RollbackActionRolledBack
		plan.State = &stateCopy
		plan.SourceDeploymentID = record.ID
		return
	}
	switch {
	case newer && pruned:
		plan.Action = rollbackActionUnresolvable
		plan.Reason = "state before the target was pruned from history"
	case newer:
		plan.Action, plan.Reason = RollbackActionSkipped, RollbackReasonNotPresent
	default:
		plan.Action = RollbackActionUnchanged
		plan.Reason = "no recorded state in retained history"
	}
}

// unresolvableRollbackError reports services whose pre-target state was
// pruned from history.
func unresolvableRollbackError(plans []RollbackServicePlan) error {
	var names []string
	for _, plan := range plans {
		if plan.Action == rollbackActionUnresolvable {
			names = append(names, plan.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return invalidRequestf("cannot reconstruct the target state of %s: history is at its %d-entry retention limit and only holds newer records; roll back individually with 'tako rollback --service <name> <deployment-id>'", strings.Join(names, ", "), remotestate.MaxHistoryEntries)
}

// CurrentServiceRecord returns the newest stable history entry for a
// service, used as the "current" side of unchanged detection.
func CurrentServiceRecord(history *remotestate.DeploymentHistory, serviceName string) *remotestate.ServiceState {
	if history == nil {
		return nil
	}
	var newest *remotestate.DeploymentState
	for _, deployment := range history.Deployments {
		if deployment == nil || !isRollbackStableStatus(deployment.Status) {
			continue
		}
		if _, ok := deployment.Services[serviceName]; !ok {
			continue
		}
		if newest == nil || deployment.Timestamp.After(newest.Timestamp) {
			newest = deployment
		}
	}
	if newest == nil {
		return nil
	}
	state := newest.Services[serviceName]
	return &state
}

// RollbackServiceUnchanged reports whether a service already runs its target
// state: the actual image and replica count match, and the newest recorded
// operator-file and shared-build hashes equal the target's.
func RollbackServiceUnchanged(target remotestate.ServiceState, actual *reconcile.ActualService, current *remotestate.ServiceState) bool {
	if actual == nil || strings.TrimSpace(actual.Image) == "" {
		return false
	}
	if actual.Image != target.Image || actual.Replicas != target.Replicas {
		return false
	}
	currentFiles, currentShared := "", ""
	if current != nil {
		currentFiles, currentShared = current.FilesContentHash, current.SharedBuildHash
	}
	return currentFiles == target.FilesContentHash && currentShared == target.SharedBuildHash
}
