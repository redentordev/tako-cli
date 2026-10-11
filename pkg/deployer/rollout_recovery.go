package deployer

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// Recovery statuses reported for a failed service rollout.
const (
	// RecoveryRestored means the previous revision is serving again (or was
	// never taken out of service).
	RecoveryRestored = "restored"
	// RecoveryFailed means restoring the previous revision failed on at least
	// one node; remaining containers are left in place for inspection.
	RecoveryFailed = "failed"
	// RecoveryDisabled means deploy.rollbackOnFailure is false.
	RecoveryDisabled = "disabled"
	// RecoveryUnsupported means at least one node lacks
	// deploy.restore-on-failure-v1, so the service used legacy replacement.
	RecoveryUnsupported = "unsupported"
)

// Recovery modes describe how the outcome was reached.
const (
	// RecoveryModeRestore restarts the retained previous containers.
	RecoveryModeRestore = "restore"
	// RecoveryModeNotCutOver means traffic never moved to the failed revision.
	RecoveryModeNotCutOver = "not-cut-over"
	// RecoveryModeNotStarted means the rollout failed before any node
	// replaced containers (preflight, build or release).
	RecoveryModeNotStarted = "not-started"
	// RecoveryModeRetainFailed keeps failed new containers stopped.
	RecoveryModeRetainFailed = "retain-failed"
	// RecoveryModeLegacy is the pre-restore remove-then-start replacement.
	RecoveryModeLegacy = "legacy"
)

// Per-node recovery statuses.
const (
	RecoveryNodeRestored   = "restored"
	RecoveryNodeUnchanged  = "unchanged"
	RecoveryNodeFailed     = "failed"
	RecoveryNodeNotCutOver = "not-cut-over"
	RecoveryNodeKept       = "kept-failed"
	RecoveryNodeUnknown    = "unknown"
)

// restoreOnFailureFeature names the capability in upgrade errors and warnings.
const restoreOnFailureFeature = "restore-on-failure (" + takod.CapabilityDeployRestoreOnFailureV1 + ")"

// RecoveryReport describes how a failed service rollout was recovered. It
// carries revision IDs, node names, container names and error text only.
type RecoveryReport struct {
	Status   string
	Mode     string
	Strategy string
	Revision string
	Nodes    []RecoveryNodeReport
}

// RecoveryNodeReport is one node's part of a RecoveryReport.
type RecoveryNodeReport struct {
	Server     string
	Status     string
	Error      string
	Containers []string
}

// takodFailurePlan is the failure handling derived for one service rollout.
type takodFailurePlan struct {
	// Policy is the FailurePolicy sent to takod; empty means the field is
	// omitted and the node receives exactly the legacy request.
	Policy string
	// NeedsCapability is true when Policy differs from legacy behavior.
	NeedsCapability bool
	// Explicit is true when tako.yaml sets deploy.rollbackOnFailure.
	Explicit bool
	// Mode is the recovery mode reported when the rollout fails.
	Mode string
	// Unsupported marks a defaulted plan downgraded to legacy because a node
	// lacks the capability.
	Unsupported bool
	Strategy    string
}

// deriveTakodFailurePlan maps strategy and deploy.rollbackOnFailure to the
// takod failure policy (design D6):
//
//	recreate, unset/true  -> restore        (capability required)
//	recreate, false       -> retain-failed  (capability required)
//	rolling|blue_green, unset/true -> legacy request; the old revision keeps
//	                                  serving because traffic never shifted
//	rolling|blue_green, false      -> retain-failed (capability required)
func deriveTakodFailurePlan(service *config.ServiceConfig) takodFailurePlan {
	strategy := effectiveDeployStrategy(service)
	plan := takodFailurePlan{Strategy: strategy}
	if service == nil {
		plan.Mode = RecoveryModeLegacy
		return plan
	}
	plan.Explicit = service.Deploy.RollbackOnFailureExplicit()
	if !service.Deploy.RollbackOnFailureEnabled() {
		plan.Policy = takod.FailurePolicyRetainFailed
		plan.NeedsCapability = true
		plan.Mode = RecoveryModeRetainFailed
		return plan
	}
	if strategy == config.DeployStrategyRecreate {
		plan.Policy = takod.FailurePolicyRestore
		plan.NeedsCapability = true
		plan.Mode = RecoveryModeRestore
		return plan
	}
	plan.Mode = RecoveryModeNotCutOver
	return plan
}

// downgradeToLegacy is the defaulted-policy fallback for older agents: the
// whole service uses remove-then-start and no node receives a policy.
func (p takodFailurePlan) downgradeToLegacy() takodFailurePlan {
	p.Policy = ""
	p.NeedsCapability = false
	p.Unsupported = true
	p.Mode = RecoveryModeLegacy
	return p
}

// resolveTakodFailurePlan applies the deploy.restore-on-failure-v1 gate on
// every assigned node. An explicit rollbackOnFailure fails closed with the
// upgrade error; a defaulted one downgrades the service to legacy on all of
// its nodes when any node lacks the capability, and returns those nodes for
// the warning. Transport errors always fail.
func resolveTakodFailurePlan(serviceName string, plan takodFailurePlan, servers []string, check func(server string) error) (takodFailurePlan, []string, error) {
	if !plan.NeedsCapability || len(servers) == 0 {
		return plan, nil, nil
	}
	err := runTakodNodeActions(servers, check)
	if err == nil {
		return plan, nil, nil
	}
	var missing []string
	var other []error
	var nodeErrs *takodNodeActionsError
	if errors.As(err, &nodeErrs) {
		for _, failure := range nodeErrs.failures {
			var capabilityErr *takodclient.CapabilityRequiredError
			if errors.As(failure.err, &capabilityErr) {
				missing = append(missing, failure.serverName)
				continue
			}
			other = append(other, fmt.Errorf("%s: %w", failure.serverName, failure.err))
		}
	} else {
		other = append(other, err)
	}
	if len(other) > 0 {
		return plan, nil, errors.Join(other...)
	}
	sort.Strings(missing)
	if plan.Explicit {
		return plan, missing, fmt.Errorf("service %s sets deploy.rollbackOnFailure: %w", serviceName, err)
	}
	return plan.downgradeToLegacy(), missing, nil
}

func restoreOnFailureDowngradeWarning(serviceName string, nodes []string) string {
	label := "node"
	if len(nodes) > 1 {
		label = "nodes"
	}
	return fmt.Sprintf("%s %s lack %s; previous revision of %s cannot be restored on failure; run `tako upgrade servers`", label, strings.Join(nodes, ", "), takod.CapabilityDeployRestoreOnFailureV1, serviceName)
}

// takodRecoveryOperations are the per-node calls the coordinator drives.
type takodRecoveryOperations struct {
	Reconcile func(server string) (*takod.ReconcileServiceResponse, error)
	Finalize  func(server string, response *takod.ReconcileServiceResponse) error
	Restore   func(server string, response *takod.ReconcileServiceResponse) error
	// Warn reports non-fatal problems (finalize failures).
	Warn func(message string)
}

type takodNodeReconcileResult struct {
	server   string
	response *takod.ReconcileServiceResponse
	err      error
}

// coordinateTakodServiceReconcile reconciles one service on every node in
// parallel and then resolves the outcome across nodes (design D5): all
// nodes succeeded -> finalize retained containers everywhere (errors are
// warnings); any node failed -> restore the nodes that succeeded (nodes that
// failed restored themselves) and build the RecoveryReport. The returned
// error is the same per-node aggregate the legacy path returned.
func coordinateTakodServiceReconcile(servers []string, plan takodFailurePlan, ops takodRecoveryOperations) (*RecoveryReport, error) {
	results := make([]takodNodeReconcileResult, 0, len(servers))
	var resultsMu sync.Mutex
	err := runTakodNodeActions(servers, func(server string) error {
		response, err := ops.Reconcile(server)
		resultsMu.Lock()
		results = append(results, takodNodeReconcileResult{server: server, response: response, err: err})
		resultsMu.Unlock()
		return err
	})
	sort.Slice(results, func(i, j int) bool { return results[i].server < results[j].server })

	if err == nil {
		if plan.Policy == takod.FailurePolicyRestore && ops.Finalize != nil {
			var retained []takodNodeReconcileResult
			for _, result := range results {
				if result.response != nil && result.response.RestoreToken != "" {
					retained = append(retained, result)
				}
			}
			finalizeErr := runTakodNodeActions(resultServers(retained), func(server string) error {
				return ops.Finalize(server, resultFor(retained, server).response)
			})
			if finalizeErr != nil && ops.Warn != nil {
				ops.Warn(fmt.Sprintf("deploy succeeded but removing retained previous containers failed (the next deploy removes them): %v", finalizeErr))
			}
		}
		return nil, nil
	}

	report := &RecoveryReport{Mode: plan.Mode, Strategy: plan.Strategy}
	switch {
	case plan.Unsupported:
		report.Status = RecoveryUnsupported
		for _, result := range results {
			if result.err != nil {
				report.Nodes = append(report.Nodes, RecoveryNodeReport{Server: result.server, Status: RecoveryNodeFailed, Error: result.err.Error()})
			}
		}
	case plan.Policy == takod.FailurePolicyRetainFailed:
		report.Status = RecoveryDisabled
		for _, result := range results {
			if result.err == nil {
				continue
			}
			node := RecoveryNodeReport{Server: result.server, Status: RecoveryNodeFailed, Error: result.err.Error()}
			if failure := reconcileFailureBody(result.err); failure != nil {
				node.Status = RecoveryNodeKept
				node.Containers = append([]string(nil), failure.KeptContainers...)
			}
			report.Nodes = append(report.Nodes, node)
		}
	case plan.Policy == takod.FailurePolicyRestore:
		report.Status = RecoveryRestored
		var succeeded []takodNodeReconcileResult
		for _, result := range results {
			if result.response != nil && result.response.RestoreToken != "" {
				// The node replaced its containers (and may have failed
				// afterwards, e.g. on backup schedule sync): revert it.
				succeeded = append(succeeded, result)
				continue
			}
			if result.err == nil {
				continue
			}
			node := RecoveryNodeReport{Server: result.server}
			if failure := reconcileFailureBody(result.err); failure != nil {
				if failure.Restored {
					node.Status = RecoveryNodeRestored
				} else {
					node.Status = RecoveryNodeFailed
					node.Error = failure.RestoreError
					report.Status = RecoveryFailed
				}
			} else if takodResponded(result.err) {
				// takod rejected the rollout before touching any container.
				node.Status = RecoveryNodeUnchanged
			} else {
				node.Status = RecoveryNodeUnknown
				node.Error = result.err.Error()
				report.Status = RecoveryFailed
			}
			report.Nodes = append(report.Nodes, node)
		}
		var restoreMu sync.Mutex
		restoreErrors := make(map[string]error)
		_ = runTakodNodeActions(resultServers(succeeded), func(server string) error {
			err := ops.Restore(server, resultFor(succeeded, server).response)
			restoreMu.Lock()
			restoreErrors[server] = err
			restoreMu.Unlock()
			return err
		})
		for _, result := range succeeded {
			node := RecoveryNodeReport{Server: result.server, Status: RecoveryNodeRestored}
			if result.err != nil {
				node.Error = result.err.Error()
			}
			if err := restoreErrors[result.server]; err != nil {
				node.Status = RecoveryNodeFailed
				node.Error = err.Error()
				report.Status = RecoveryFailed
			}
			report.Nodes = append(report.Nodes, node)
		}
		sort.Slice(report.Nodes, func(i, j int) bool { return report.Nodes[i].Server < report.Nodes[j].Server })
	default:
		// Revision-scoped strategies: the previous revision was never
		// stopped and proxy routes never moved.
		report.Status = RecoveryRestored
		for _, result := range results {
			node := RecoveryNodeReport{Server: result.server, Status: RecoveryNodeNotCutOver}
			if result.err != nil {
				node.Error = result.err.Error()
			}
			report.Nodes = append(report.Nodes, node)
		}
	}
	return report, err
}

func resultServers(results []takodNodeReconcileResult) []string {
	servers := make([]string, 0, len(results))
	for _, result := range results {
		servers = append(servers, result.server)
	}
	return servers
}

func resultFor(results []takodNodeReconcileResult, server string) takodNodeReconcileResult {
	for _, result := range results {
		if result.server == server {
			return result
		}
	}
	return takodNodeReconcileResult{server: server}
}

// TakodReconcileFailureError is a node reconcile failure that carries
// takod's typed recovery result.
type TakodReconcileFailureError struct {
	Failure takod.ReconcileFailureBody
	Err     error
}

func (e *TakodReconcileFailureError) Error() string {
	message := "takod service reconciliation failed: " + e.Failure.Error
	switch {
	case e.Failure.FailurePolicy == takod.FailurePolicyRestore && e.Failure.Restored:
		message += "; previous containers restored"
	case e.Failure.FailurePolicy == takod.FailurePolicyRestore && e.Failure.RestoreError != "":
		message += "; restoring previous containers failed: " + e.Failure.RestoreError
	case e.Failure.FailurePolicy == takod.FailurePolicyRetainFailed && len(e.Failure.KeptContainers) > 0:
		message += "; failed containers kept stopped for debugging: " + strings.Join(e.Failure.KeptContainers, ", ")
	}
	return message
}

func (e *TakodReconcileFailureError) Unwrap() error { return e.Err }

func reconcileFailureBody(err error) *takod.ReconcileFailureBody {
	var failure *TakodReconcileFailureError
	if errors.As(err, &failure) {
		return &failure.Failure
	}
	return nil
}

// takodResponded reports whether the node agent answered with an HTTP error,
// as opposed to a transport failure whose effect on the node is unknown.
func takodResponded(err error) bool {
	var httpErr *takodclient.HTTPError
	return errors.As(err, &httpErr)
}

// parseTakodReconcileFailure turns takod's typed failure document into a
// TakodReconcileFailureError; other errors are returned unchanged.
func parseTakodReconcileFailure(err error) error {
	var httpErr *takodclient.HTTPError
	if !errors.As(err, &httpErr) {
		return err
	}
	var body takod.ReconcileFailureBody
	if decodeErr := json.Unmarshal([]byte(strings.TrimSpace(httpErr.Body)), &body); decodeErr != nil || body.FailurePolicy == "" || body.Error == "" {
		return err
	}
	return &TakodReconcileFailureError{Failure: body, Err: err}
}

// RecoveryReportFor returns the recovery report recorded for a failed
// service rollout in this deploy, or nil.
func (d *Deployer) RecoveryReportFor(serviceName string) *RecoveryReport {
	d.recoveryMu.Lock()
	defer d.recoveryMu.Unlock()
	return d.recoveryReports[serviceName]
}

func (d *Deployer) recordRecoveryReport(serviceName string, report *RecoveryReport) {
	d.recoveryMu.Lock()
	defer d.recoveryMu.Unlock()
	if d.recoveryReports == nil {
		d.recoveryReports = make(map[string]*RecoveryReport)
	}
	if report == nil {
		delete(d.recoveryReports, serviceName)
		return
	}
	d.recoveryReports[serviceName] = report
}

func (d *Deployer) warnService(serviceName string, message string, data map[string]any) {
	d.emitEvent(events.Event{
		Type:    events.TypeWarning,
		Phase:   events.PhaseDeploy,
		Level:   events.LevelWarn,
		Service: serviceName,
		Message: fmt.Sprintf("  ⚠ %s\n", message),
		Data:    data,
	})
}

func (d *Deployer) finalizeRetainedServiceViaTakod(client any, request takod.ServiceRecoveryRequest) error {
	if _, err := takodclient.RequestJSONWithTimeoutContext(d.baseContext(), client, d.takodSocket(), "POST", "/v1/services/finalize", request, retainedRecoveryTimeout); err != nil {
		return fmt.Errorf("takod retained container finalize failed: %w", err)
	}
	return nil
}

func (d *Deployer) restoreRetainedServiceViaTakod(client any, request takod.ServiceRecoveryRequest) error {
	if _, err := takodclient.RequestJSONWithTimeoutContext(d.baseContext(), client, d.takodSocket(), "POST", "/v1/services/restore", request, retainedRecoveryTimeout); err != nil {
		return fmt.Errorf("takod previous revision restore failed: %w", err)
	}
	return nil
}

// retainedRecoveryTimeout covers graceful stop of the new containers (bounded
// by the validated maximum stop grace period) plus restarts.
const retainedRecoveryTimeout = 24*time.Hour + 5*time.Minute
