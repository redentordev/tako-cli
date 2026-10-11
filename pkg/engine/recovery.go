package engine

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

// RecoveryOutcome reports how a failed service rollout was recovered. It
// carries revision IDs, node names, container names and error text only.
type RecoveryOutcome struct {
	// Status is restored, failed, disabled or unsupported.
	Status string `json:"status"`
	// Mode is restore, not-cut-over, not-started, retain-failed or legacy.
	Mode string `json:"mode,omitempty"`
	// Revision is the previous revision that serves after recovery.
	Revision string `json:"revision,omitempty"`
	// FailedRevision is the revision whose rollout failed.
	FailedRevision string                `json:"failedRevision,omitempty"`
	Nodes          []RecoveryNodeOutcome `json:"nodes,omitempty"`
}

// RecoveryNodeOutcome is one node's part of a RecoveryOutcome.
type RecoveryNodeOutcome struct {
	Server     string   `json:"server"`
	Status     string   `json:"status"`
	Error      string   `json:"error,omitempty"`
	Containers []string `json:"containers,omitempty"`
}

// recoveryOutcomeFor converts the deployer's recovery report for a failed
// service into the result document. previousRevision is the revision that
// was serving before the deploy.
func recoveryOutcomeFor(d *deployer.Deployer, serviceName string, previousRevision string) *RecoveryOutcome {
	if d == nil {
		return nil
	}
	return recoveryOutcomeFromReport(d.RecoveryReportFor(serviceName), previousRevision)
}

func recoveryOutcomeFromReport(report *deployer.RecoveryReport, previousRevision string) *RecoveryOutcome {
	if report == nil {
		return nil
	}
	outcome := &RecoveryOutcome{
		Status:         report.Status,
		Mode:           report.Mode,
		Revision:       previousRevision,
		FailedRevision: report.Revision,
	}
	for _, node := range report.Nodes {
		outcome.Nodes = append(outcome.Nodes, RecoveryNodeOutcome{
			Server:     node.Server,
			Status:     node.Status,
			Error:      node.Error,
			Containers: append([]string(nil), node.Containers...),
		})
	}
	return outcome
}

// failedServiceOutcomeAction is "restored" when the previous revision serves
// again and "failed" otherwise.
func failedServiceOutcomeAction(recovery *RecoveryOutcome) string {
	if recovery != nil && recovery.Status == deployer.RecoveryRestored {
		return OutcomeRestored
	}
	return OutcomeFailed
}

// emitRecoveryEvents emits deploy.service.restore.started followed by
// deploy.service.restored or deploy.service.restore_failed after the
// deploy.service.failed event. disabled and unsupported recoveries emit
// nothing: no restore was attempted.
func (e *Engine) emitRecoveryEvents(serviceName string, recovery *RecoveryOutcome) {
	if recovery == nil || (recovery.Status != deployer.RecoveryRestored && recovery.Status != deployer.RecoveryFailed) {
		return
	}
	data := recoveryEventData(recovery)
	e.emit(events.Event{
		Type:    events.TypeDeployServiceRestoreStarted,
		Phase:   events.PhaseDeploy,
		Level:   events.LevelInfo,
		Service: serviceName,
		Message: fmt.Sprintf("  ↺ Restoring previous revision of %s\n", serviceName),
		Data:    data,
	})
	if recovery.Status == deployer.RecoveryRestored {
		e.emit(events.Event{
			Type:    events.TypeDeployServiceRestored,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: serviceName,
			Message: fmt.Sprintf("  ✓ %s\n", recoverySentence(serviceName, recovery)),
			Data:    data,
		})
		return
	}
	e.emit(events.Event{
		Type:    events.TypeDeployServiceRestoreFailed,
		Phase:   events.PhaseDeploy,
		Level:   events.LevelError,
		Service: serviceName,
		Message: fmt.Sprintf("  ✗ %s\n", recoverySentence(serviceName, recovery)),
		Data:    data,
	})
}

func recoveryEventData(recovery *RecoveryOutcome) map[string]any {
	nodes := make([]map[string]any, 0, len(recovery.Nodes))
	for _, node := range recovery.Nodes {
		entry := map[string]any{"server": node.Server, "status": node.Status}
		if node.Error != "" {
			entry["error"] = node.Error
		}
		if len(node.Containers) > 0 {
			entry["containers"] = append([]string(nil), node.Containers...)
		}
		nodes = append(nodes, entry)
	}
	data := map[string]any{"status": recovery.Status, "mode": recovery.Mode, "nodes": nodes}
	if recovery.Revision != "" {
		data["revision"] = recovery.Revision
	}
	if recovery.FailedRevision != "" {
		data["failedRevision"] = recovery.FailedRevision
	}
	return data
}

// recoverySentence describes the recovery for humans and notifications.
func recoverySentence(serviceName string, recovery *RecoveryOutcome) string {
	if recovery == nil {
		return ""
	}
	switch recovery.Status {
	case deployer.RecoveryRestored:
		switch recovery.Mode {
		case deployer.RecoveryModeNotCutOver:
			return fmt.Sprintf("Previous revision of %s kept serving; traffic never moved to the failed revision", serviceName)
		case deployer.RecoveryModeNotStarted:
			return fmt.Sprintf("Previous revision of %s left unchanged; the rollout failed before replacing containers", serviceName)
		}
		return fmt.Sprintf("Previous revision of %s restored", serviceName)
	case deployer.RecoveryFailed:
		var failed []string
		for _, node := range recovery.Nodes {
			if node.Status == deployer.RecoveryNodeFailed || node.Status == deployer.RecoveryNodeUnknown {
				failed = append(failed, node.Server)
			}
		}
		if len(failed) > 0 {
			return fmt.Sprintf("Restoring the previous revision of %s failed on %s; containers were left in place for inspection", serviceName, strings.Join(failed, ", "))
		}
		return fmt.Sprintf("Restoring the previous revision of %s failed; containers were left in place for inspection", serviceName)
	case deployer.RecoveryDisabled:
		return fmt.Sprintf("Automatic restore is disabled for %s (deploy.rollbackOnFailure: false); failed containers were kept stopped", serviceName)
	case deployer.RecoveryUnsupported:
		return fmt.Sprintf("Previous revision of %s could not be restored: a node lacks deploy.restore-on-failure-v1 (run `tako upgrade servers`)", serviceName)
	}
	return ""
}

// failureNotificationContent adds the failed service's recovery status to a
// deploy failure notification: a "recovery" detail and a sentence appended
// to the message.
func failureNotificationContent(message string, details map[string]string, serviceName string, recovery *RecoveryOutcome) (string, map[string]string) {
	if recovery == nil || serviceName == "" {
		return message, details
	}
	if details == nil {
		details = map[string]string{}
	}
	details["recovery"] = recovery.Status
	details["service"] = serviceName
	if sentence := recoverySentence(serviceName, recovery); sentence != "" {
		message += ". " + sentence + "."
	}
	return message, details
}

func recoveryHistoryState(recovery *RecoveryOutcome) *state.RecoveryState {
	if recovery == nil {
		return nil
	}
	out := &state.RecoveryState{Status: recovery.Status, Mode: recovery.Mode, Revision: recovery.Revision}
	for _, node := range recovery.Nodes {
		out.Nodes = append(out.Nodes, state.RecoveryNodeState{
			Server:     node.Server,
			Status:     node.Status,
			Error:      node.Error,
			Containers: append([]string(nil), node.Containers...),
		})
	}
	return out
}
