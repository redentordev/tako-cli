// Package events defines Tako's canonical engine event schema. Engine
// operations emit a typed event stream that renderers consume: the CLI
// renders events as human output, and machine consumers read them as NDJSON.
// The schema follows the same additive versioning rules as the other takoapi
// documents.
package events

import (
	"time"
)

const (
	// APIVersionV1Alpha1 is the first additive event schema version.
	APIVersionV1Alpha1 = "tako.redentor.dev/v1alpha1"

	// APIVersionCurrent points at the current event schema version for new events.
	APIVersionCurrent = APIVersionV1Alpha1

	// KindEvent identifies a canonical engine event document.
	KindEvent = "Event"
)

// Level classifies event severity for renderers and filters.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Phases group events into the coarse stages a UI presents. They collapse the
// engine's internal pipeline steps into user-meaningful stages.
const (
	PhasePlan    = "plan"
	PhaseBuild   = "build"
	PhaseDeploy  = "deploy"
	PhaseState   = "state"
	PhaseCleanup = "cleanup"
	PhaseDomains = "domains"
	PhaseNotify  = "notify"
	PhaseLogs    = "logs"
	PhaseSetup   = "setup"
	// PhaseHooks groups local lifecycle hook events (hook.*).
	PhaseHooks = "hooks"
	// PhaseSummary carries the end-of-operation summary (operation.summary).
	PhaseSummary = "summary"
)

// Event types. Consumers must tolerate unknown types (additive schema).
const (
	TypePhaseStarted   = "phase.started"
	TypePhaseCompleted = "phase.completed"
	// TypeLogLine carries one line of operation or container output. For
	// `tako logs` the raw line is in data.data alongside data.service and
	// data.node, plus data.replica, data.container, data.revision,
	// data.stream ("stdout"/"stderr"), and data.time (RFC3339Nano) when the
	// node agent supplies them. Message is the human rendering and never
	// contains ANSI escapes.
	TypeLogLine = "log.line"

	TypePlanComputed             = "plan.computed"
	TypePlanConfirmationRequired = "plan.confirmation_required"
	TypePlanUpToDate             = "plan.up_to_date"

	TypeDeployStarted           = "deploy.started"
	TypeDeployServiceStarted    = "deploy.service.started"
	TypeDeployServiceReconciled = "deploy.service.reconciled"
	TypeDeployServiceWarmed     = "deploy.service.warmed"
	TypeDeployServiceFailed     = "deploy.service.failed"
	// Restore events follow deploy.service.failed when the previous
	// revision is restored (or was never cut over) after a failed rollout.
	TypeDeployServiceRestoreStarted = "deploy.service.restore.started"
	TypeDeployServiceRestored       = "deploy.service.restored"
	TypeDeployServiceRestoreFailed  = "deploy.service.restore_failed"
	TypeDeployServiceRemoved        = "deploy.service.removed"
	// TypeDeployServiceSkipped reports a service an operation left alone
	// (project rollback: skipped or unchanged); data.action and
	// data.reason say why.
	TypeDeployServiceSkipped = "deploy.service.skipped"
	TypeDeploySucceeded      = "deploy.succeeded"
	TypeDeployFailed         = "deploy.failed"
	TypeDeployCancelled      = "deploy.cancelled"

	TypeProxyReconciled    = "proxy.reconciled"
	TypeRevisionsPruned    = "revisions.pruned"
	TypeStatePersisted     = "state.persisted"
	TypeStateReplicated    = "state.replicated"
	TypeCleanupCompleted   = "cleanup.completed"
	TypeNotificationSent   = "notification.sent"
	TypeDomainStatus       = "domain.status"
	TypeCertificate        = "certificate.operation"
	TypeCertIssueStarted   = "cert.issue.started"
	TypeCertIssueCompleted = "cert.issue.completed"
	TypeCertIssueFailed    = "cert.issue.failed"
	TypeCertIssueSkipped   = "cert.issue.skipped"
	TypeCertRenewCompleted = "cert.renew.completed"
	TypeCertRenewFailed    = "cert.renew.failed"

	// TypeStatsSample carries one node's point-in-time container stats in
	// `tako stats --follow --events ndjson`; data holds server, host, and
	// containers (same shape as the StatsResult node samples).
	TypeStatsSample = "stats.sample"

	// TypeAccessLine carries one proxy access-log entry in
	// `tako access --events ndjson`; data holds node and the raw log line
	// (`data.data`), mirroring `log.line`.
	TypeAccessLine = "access.line"

	// Setup step lifecycle events carry `data.step` (stable step key) and
	// `data.title` (human step name); failed adds `data.error`. Node names
	// ride the top-level `node` field.
	TypeSetupStepStarted   = "setup.step.started"
	TypeSetupStepCompleted = "setup.step.completed"
	TypeSetupStepFailed    = "setup.step.failed"
	TypeSetupStepSkipped   = "setup.step.skipped"

	// Exec lifecycle events for `tako exec`: started carries the resolved
	// placement (`data.mode`, `data.container`), output carries one chunk of
	// combined remote output in `data.data`, completed carries
	// `data.exitCode` and `data.durationMs`.
	TypeExecStarted   = "exec.started"
	TypeExecOutput    = "exec.output"
	TypeExecCompleted = "exec.completed"

	// Release-command lifecycle events during deploy: started carries the
	// service and image, output lines ride log.line-style `data.data`,
	// completed/failed carry `data.exitCode` and `data.durationMs`.
	TypeDeployReleaseStarted   = "deploy.release.started"
	TypeDeployReleaseOutput    = "deploy.release.output"
	TypeDeployReleaseCompleted = "deploy.release.completed"
	TypeDeployReleaseFailed    = "deploy.release.failed"
	TypeDeployRunStarted       = "deploy.run.started"
	TypeDeployRunOutput        = "deploy.run.output"
	TypeDeployRunCompleted     = "deploy.run.completed"
	TypeDeployRunFailed        = "deploy.run.failed"

	// TypeDeployJobsApplied reports one node's declarative job-schedule
	// reconciliation during a deploy.
	TypeDeployJobsApplied = "deploy.jobs.applied"

	// TypeImagePullAuthFailed marks an image pull/build that failed due to
	// registry credentials, distinct from image-not-found, so control
	// planes can prompt for credential rotation.
	TypeImagePullAuthFailed = "image.pull.auth_failed"

	// Job command events mirror the exec stream for `tako jobs trigger`.
	TypeJobTriggerStarted   = "jobs.trigger.started"
	TypeJobTriggerOutput    = "jobs.trigger.output"
	TypeJobTriggerCompleted = "jobs.trigger.completed"

	// Local lifecycle hook events (phase "hooks"). Every event carries
	// `data.hook`. started adds `data.source` ("file"/"config"),
	// `data.path` or `data.argv`, and `data.timeoutSeconds`; output carries
	// one line in `data.data` with `data.stream` ("stdout"/"stderr");
	// completed/failed carry `data.exitCode` and `data.durationMs`, and
	// failed adds `data.error` and `data.timedOut`. skipped carries
	// `data.reason` (--skip-hooks).
	TypeHookStarted   = "hook.started"
	TypeHookOutput    = "hook.output"
	TypeHookCompleted = "hook.completed"
	TypeHookFailed    = "hook.failed"
	TypeHookSkipped   = "hook.skipped"

	// Preview lifecycle events. preview.destroyed is emitted once per preview
	// torn down by `tako preview destroy`; preview.pruned once per expired
	// preview destroyed by `tako preview prune` or by the opportunistic
	// prune of `tako deploy --preview`. Both carry `data.name`,
	// `data.environment` and `data.base`.
	TypePreviewDestroyed = "preview.destroyed"
	TypePreviewPruned    = "preview.pruned"

	TypeWarning = "warning"

	// TypeOperationSummary carries the end-of-operation summary for deploy,
	// rollback, promote, and scale. It is emitted once, immediately before
	// the result event. Message is the plain-text rendering; data.summary
	// equals the result document's summary field and data.operation names
	// the operation.
	TypeOperationSummary = "operation.summary"

	// TypeResult is the terminal event carrying the operation result document
	// in machine-output mode.
	TypeResult = "result"
)

// Event is one entry in an engine operation's event stream. Message is the
// human-readable rendering; Data carries the typed payload for machine
// consumers. Consumers must ignore unknown fields and unknown Data keys.
type Event struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Seq        int64          `json:"seq"`
	Time       time.Time      `json:"time"`
	Type       string         `json:"type"`
	Phase      string         `json:"phase,omitempty"`
	Level      Level          `json:"level"`
	Service    string         `json:"service,omitempty"`
	Node       string         `json:"node,omitempty"`
	Message    string         `json:"message,omitempty"`
	Data       map[string]any `json:"data,omitempty"`
}

// Sink consumes engine events. Implementations must be safe for concurrent
// use; engine operations may emit from multiple goroutines.
type Sink interface {
	Emit(Event)
}
