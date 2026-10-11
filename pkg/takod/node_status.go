package takod

// Scheduled read-only node conditions (add-server-status-history design D5)
// and their publication as alert kind "node" (design D6). Conditions are
// persisted in <data-dir>/node-status.json so `since` survives restarts and
// alert windows are measured from it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CapabilityNodeStatusV1 means the node serves GET /v1/node/status and adds
// the optional status object to GET /v1/metrics.
const CapabilityNodeStatusV1 = "node.status-v1"

// Node condition types.
const (
	NodeConditionOSUpdates        = "os-updates"
	NodeConditionRebootRequired   = "reboot-required"
	NodeConditionClockSync        = "clock-sync"
	NodeConditionDockerDaemon     = "docker-daemon"
	NodeConditionMetricsCollector = "metrics-collector"
)

// Node condition statuses.
const (
	NodeConditionOK      = "ok"
	NodeConditionWarn    = "warn"
	NodeConditionFail    = "fail"
	NodeConditionUnknown = "unknown"
)

const (
	nodeStatusFileName          = "node-status.json"
	nodeStatusSchemaVersion     = 1
	nodeStatusLoopInterval      = 15 * time.Second
	nodeStatusAlertInterval     = time.Minute
	osUpdatesInterval           = 6 * time.Hour
	osUpdatesFirstMin           = 2 * time.Minute
	osUpdatesFirstMax           = 5 * time.Minute
	rebootProbeInterval         = 15 * time.Minute
	clockProbeInterval          = 15 * time.Minute
	dockerProbeInterval         = time.Minute
	nodeProbeJitter             = 30 * time.Second
	clockFailConsecutive        = 2
	collectorStaleIntervals     = 3
	nodeAlertRebootWindow       = 24 * time.Hour
	nodeAlertSecurityWindow     = 72 * time.Hour
	nodeAlertDockerWindow       = 3 * time.Minute
	nodeReasonRebootRequired    = "reboot_required"
	nodeReasonSecurityUpdates   = "security_updates_pending"
	nodeReasonClockUnsync       = "clock_unsynchronized"
	nodeReasonDockerUnreachable = "docker_unreachable"
	nodeEventRebootRequired     = "node_reboot_required"
	nodeEventSecurityUpdates    = "node_security_updates"
	nodeEventClockUnsync        = "node_clock_unsynchronized"
	nodeEventDockerUnreachable  = "node_docker_unreachable"
	nodeEventConditionCleared   = "node_condition_cleared"
)

// NodeCondition is one read-only node check.
type NodeCondition struct {
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	Since     time.Time `json:"since"`
	CheckedAt time.Time `json:"checkedAt"`
	Stale     bool      `json:"stale,omitempty"`
}

// NodeStatusResponse is the GET /v1/node/status document and the optional
// status object of GET /v1/metrics.
type NodeStatusResponse struct {
	Node        string          `json:"node,omitempty"`
	EvaluatedAt time.Time       `json:"evaluatedAt"`
	OS          *NodeOSStatus   `json:"os,omitempty"`
	Conditions  []NodeCondition `json:"conditions"`
}

// Condition returns the condition of a type, if present.
func (r *NodeStatusResponse) Condition(conditionType string) (NodeCondition, bool) {
	if r == nil {
		return NodeCondition{}, false
	}
	for _, condition := range r.Conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return NodeCondition{}, false
}

type nodeStatusDocument struct {
	SchemaVersion int                      `json:"schemaVersion"`
	OS            *NodeOSStatus            `json:"os,omitempty"`
	Conditions    map[string]NodeCondition `json:"conditions"`
	ClockNoCount  int                      `json:"clockNoCount,omitempty"`
	ClockFirstNo  time.Time                `json:"clockFirstNo,omitempty"`
}

// nodeCollectorState reports the native collector to node status.
type nodeCollectorState func() (latest time.Time, hasSample bool, readErr, historyErr error, warnings []string)

// nodeStatusManager owns node conditions, their schedule, persistence and
// alert publication.
type nodeStatusManager struct {
	path      string
	node      func() string
	probe     *osProbe
	docker    dockerPinger
	collector nodeCollectorState
	interval  time.Duration
	alerts    *SourceAlertManager
	now       func() time.Time
	jitter    func(time.Duration) time.Duration
	startedAt time.Time

	mu          sync.Mutex
	doc         nodeStatusDocument
	evaluatedAt time.Time
	next        map[string]time.Time
	lastAlerts  time.Time
}

func newNodeStatusManager(dataDir string, node func() string, alerts *SourceAlertManager, collector nodeCollectorState, interval time.Duration) *nodeStatusManager {
	return &nodeStatusManager{
		path: filepath.Join(dataDir, nodeStatusFileName), node: node, probe: newOSProbe(), docker: pingDockerDaemon,
		collector: collector, interval: interval, alerts: alerts,
		now:    func() time.Time { return time.Now().UTC() },
		jitter: func(max time.Duration) time.Duration { return time.Duration(rand.Int63n(int64(max) + 1)) },
		doc:    nodeStatusDocument{SchemaVersion: nodeStatusSchemaVersion, Conditions: map[string]NodeCondition{}},
		next:   map[string]time.Time{},
	}
}

func nodeAlertSourceSpec() AlertSourceSpec {
	return AlertSourceSpec{
		Kind:       AlertKindNode,
		NodeScoped: true,
		Reasons: map[string]Severity{
			nodeReasonRebootRequired: SeverityWarning, nodeReasonSecurityUpdates: SeverityWarning,
			nodeReasonClockUnsync: SeverityCritical, nodeReasonDockerUnreachable: SeverityCritical,
		},
		DetailKeys: []string{"since", "pendingUpdates", "securityUpdates", "packageCount", "status"},
		Events: map[string]string{
			nodeReasonRebootRequired: nodeEventRebootRequired, nodeReasonSecurityUpdates: nodeEventSecurityUpdates,
			nodeReasonClockUnsync: nodeEventClockUnsync, nodeReasonDockerUnreachable: nodeEventDockerUnreachable,
		},
		Recoveries: map[string]string{
			nodeReasonRebootRequired: nodeEventConditionCleared, nodeReasonSecurityUpdates: nodeEventConditionCleared,
			nodeReasonClockUnsync: nodeEventConditionCleared, nodeReasonDockerUnreachable: nodeEventConditionCleared,
		},
	}
}

// Load restores persisted conditions. A missing or unreadable file starts
// empty.
func (m *nodeStatusManager) Load() {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var doc nodeStatusDocument
	if err := json.Unmarshal(data, &doc); err != nil || doc.SchemaVersion != nodeStatusSchemaVersion {
		return
	}
	if doc.Conditions == nil {
		doc.Conditions = map[string]NodeCondition{}
	}
	m.mu.Lock()
	m.doc = doc
	m.mu.Unlock()
}

func (m *nodeStatusManager) persistLocked() {
	data, err := json.MarshalIndent(m.doc, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o750); err != nil {
		return
	}
	_ = writeFileAtomic(m.path, append(data, '\n'), 0o640)
}

// setConditionLocked updates a condition, keeping since while the status
// is unchanged. sinceOverride, when non-zero, sets since explicitly.
func (m *nodeStatusManager) setConditionLocked(conditionType, status, reason, detail string, now, sinceOverride time.Time) {
	previous, exists := m.doc.Conditions[conditionType]
	condition := NodeCondition{Type: conditionType, Status: status, Reason: reason, Detail: detail, CheckedAt: now}
	switch {
	case !sinceOverride.IsZero():
		condition.Since = sinceOverride.UTC()
	case exists && previous.Status == status && !previous.Since.IsZero():
		condition.Since = previous.Since
	default:
		condition.Since = now
	}
	m.doc.Conditions[conditionType] = condition
}

// markStaleLocked keeps the previous result and marks it stale, or records
// unknown when there is none.
func (m *nodeStatusManager) markStaleLocked(conditionType, detail string, now time.Time) {
	previous, exists := m.doc.Conditions[conditionType]
	if !exists || previous.Status == NodeConditionUnknown {
		m.setConditionLocked(conditionType, NodeConditionUnknown, "probe-failed", detail, now, time.Time{})
		return
	}
	previous.Stale = true
	previous.Detail = detail
	previous.CheckedAt = now
	m.doc.Conditions[conditionType] = previous
}

// RunOSUpdates runs the package-update probe.
func (m *nodeStatusManager) RunOSUpdates(ctx context.Context) {
	status, err := m.probe.Updates(ctx)
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.persistLocked()
	if errors.Is(err, errUnsupportedPackageManager) {
		m.setConditionLocked(NodeConditionOSUpdates, NodeConditionUnknown, "unsupported-package-manager", "package manager is not apt, dnf or yum", now, time.Time{})
		return
	}
	previous := m.doc.OS
	if err != nil {
		if previous != nil {
			previous.Stale = true
		}
		m.markStaleLocked(NodeConditionOSUpdates, truncateDetail(err.Error()), now)
		return
	}
	if previous != nil {
		status.RebootRequired, status.RebootRequiredSince, status.RebootPackages, status.RebootCheckedAt = previous.RebootRequired, previous.RebootRequiredSince, previous.RebootPackages, previous.RebootCheckedAt
	}
	m.doc.OS = &status
	detail := fmt.Sprintf("%d updates (%d security)", status.PendingUpdates, status.SecurityUpdates)
	if status.SecurityUpdates > 0 {
		m.setConditionLocked(NodeConditionOSUpdates, NodeConditionWarn, "security-updates-pending", detail, now, time.Time{})
		return
	}
	m.setConditionLocked(NodeConditionOSUpdates, NodeConditionOK, "", detail, now, time.Time{})
}

// RunReboot runs the reboot-required probe.
func (m *nodeStatusManager) RunReboot(ctx context.Context) {
	state, err := m.probe.Reboot(ctx)
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.persistLocked()
	if errors.Is(err, errUnsupportedPackageManager) {
		m.setConditionLocked(NodeConditionRebootRequired, NodeConditionUnknown, "unsupported-package-manager", "", now, time.Time{})
		return
	}
	if err != nil {
		m.markStaleLocked(NodeConditionRebootRequired, truncateDetail(err.Error()), now)
		return
	}
	if m.doc.OS == nil {
		m.doc.OS = &NodeOSStatus{PackageManager: m.probe.packageManager()}
	}
	m.doc.OS.RebootRequired = state.Required
	m.doc.OS.RebootPackages = state.Packages
	m.doc.OS.RebootCheckedAt = now
	if !state.Required {
		m.doc.OS.RebootRequiredSince = nil
		m.setConditionLocked(NodeConditionRebootRequired, NodeConditionOK, "", "", now, time.Time{})
		return
	}
	// A reboot stays required since the earliest observation: a later
	// package rewriting reboot-required must not restart the alert window.
	since := state.Since
	if previous, ok := m.doc.Conditions[NodeConditionRebootRequired]; ok && previous.Status == NodeConditionWarn && !previous.Since.IsZero() && (since.IsZero() || previous.Since.Before(since)) {
		since = previous.Since
	}
	if since.IsZero() {
		since = now
	}
	m.doc.OS.RebootRequiredSince = &since
	detail := "reboot required"
	if len(state.Packages) > 0 {
		detail = "reboot required by " + strings.Join(state.Packages, ", ")
	}
	m.setConditionLocked(NodeConditionRebootRequired, NodeConditionWarn, "reboot-required", truncateDetail(detail), now, since)
}

// RunClock runs the clock-sync probe: fail after two consecutive
// unsynchronized checks, with since at the first failing check.
func (m *nodeStatusManager) RunClock(ctx context.Context) {
	synchronized, available, err := m.probe.ClockSynchronized(ctx)
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.persistLocked()
	switch {
	case !available:
		m.setConditionLocked(NodeConditionClockSync, NodeConditionUnknown, "timedatectl-unavailable", "", now, time.Time{})
	case err != nil:
		m.markStaleLocked(NodeConditionClockSync, truncateDetail(err.Error()), now)
	case synchronized:
		m.doc.ClockNoCount, m.doc.ClockFirstNo = 0, time.Time{}
		m.setConditionLocked(NodeConditionClockSync, NodeConditionOK, "", "NTP synchronized", now, time.Time{})
	default:
		if m.doc.ClockNoCount == 0 {
			m.doc.ClockFirstNo = now
		}
		m.doc.ClockNoCount++
		if m.doc.ClockNoCount >= clockFailConsecutive {
			m.setConditionLocked(NodeConditionClockSync, NodeConditionFail, "ntp-unsynchronized", "NTP not synchronized", now, m.doc.ClockFirstNo)
			return
		}
		if previous, ok := m.doc.Conditions[NodeConditionClockSync]; ok {
			previous.CheckedAt = now
			m.doc.Conditions[NodeConditionClockSync] = previous
		}
	}
}

// RunDocker pings the Docker daemon.
func (m *nodeStatusManager) RunDocker(ctx context.Context) {
	err := m.docker(ctx)
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.persistLocked()
	if err != nil {
		m.setConditionLocked(NodeConditionDockerDaemon, NodeConditionFail, "ping-failed", truncateDetail(err.Error()), now, time.Time{})
		return
	}
	m.setConditionLocked(NodeConditionDockerDaemon, NodeConditionOK, "", "", now, time.Time{})
}

// UpdateCollector evaluates the metrics-collector condition. It is absent
// when native collection is disabled.
func (m *nodeStatusManager) UpdateCollector() {
	if m.collector == nil || m.interval <= 0 {
		return
	}
	latest, hasSample, readErr, historyErr, warnings := m.collector()
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	staleAfter := time.Duration(collectorStaleIntervals) * m.interval
	before := m.doc.Conditions[NodeConditionMetricsCollector]
	switch {
	case !hasSample && now.Sub(m.startedAt) < staleAfter:
		m.setConditionLocked(NodeConditionMetricsCollector, NodeConditionUnknown, "starting", "", now, time.Time{})
	case !hasSample || now.Sub(latest) > staleAfter:
		detail := "no sample for 3 intervals"
		if readErr != nil {
			detail = truncateDetail(readErr.Error())
		}
		m.setConditionLocked(NodeConditionMetricsCollector, NodeConditionFail, "no-recent-sample", detail, now, time.Time{})
	case historyErr != nil:
		m.setConditionLocked(NodeConditionMetricsCollector, NodeConditionWarn, metricsHistoryReasonWriteError, truncateDetail(historyErr.Error()), now, time.Time{})
	case len(warnings) > 0:
		m.setConditionLocked(NodeConditionMetricsCollector, NodeConditionWarn, "source-unavailable", truncateDetail(strings.Join(warnings, "; ")), now, time.Time{})
	default:
		m.setConditionLocked(NodeConditionMetricsCollector, NodeConditionOK, "", "", now, time.Time{})
	}
	if after := m.doc.Conditions[NodeConditionMetricsCollector]; after.Status != before.Status || after.Reason != before.Reason {
		m.persistLocked()
	}
}

func truncateDetail(detail string) string {
	detail = strings.TrimSpace(strings.ReplaceAll(detail, "\n", " "))
	if len(detail) > 240 {
		detail = detail[:240]
	}
	return detail
}

// Status returns the current node status document; ok is false before any
// condition has been evaluated.
func (m *nodeStatusManager) Status() (*NodeStatusResponse, bool) {
	m.UpdateCollector()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.doc.Conditions) == 0 {
		return nil, false
	}
	response := &NodeStatusResponse{EvaluatedAt: m.evaluatedAt, Conditions: make([]NodeCondition, 0, len(m.doc.Conditions))}
	if m.node != nil {
		response.Node = m.node()
	}
	if response.EvaluatedAt.IsZero() {
		response.EvaluatedAt = m.now()
	}
	if m.doc.OS != nil {
		osStatus := *m.doc.OS
		osStatus.SecurityPackages = append([]string(nil), m.doc.OS.SecurityPackages...)
		osStatus.RebootPackages = append([]string(nil), m.doc.OS.RebootPackages...)
		response.OS = &osStatus
	}
	for _, condition := range m.doc.Conditions {
		response.Conditions = append(response.Conditions, condition)
	}
	sort.Slice(response.Conditions, func(i, j int) bool { return response.Conditions[i].Type < response.Conditions[j].Type })
	return response, true
}

// schedule sets the first due times: os-updates 2-5 minutes after start,
// the others shortly after start with jitter.
func (m *nodeStatusManager) schedule(now time.Time) {
	m.next[NodeConditionOSUpdates] = now.Add(osUpdatesFirstMin + m.jitter(osUpdatesFirstMax-osUpdatesFirstMin))
	m.next[NodeConditionRebootRequired] = now.Add(m.jitter(nodeProbeJitter))
	m.next[NodeConditionClockSync] = now.Add(m.jitter(nodeProbeJitter))
	m.next[NodeConditionDockerDaemon] = now.Add(m.jitter(nodeProbeJitter))
}

// RunDue runs every probe that is due at now and publishes alerts at most
// once per alert interval.
func (m *nodeStatusManager) RunDue(ctx context.Context) {
	now := m.now()
	type probe struct {
		conditionType string
		every         time.Duration
		run           func(context.Context)
	}
	for _, p := range []probe{
		{NodeConditionDockerDaemon, dockerProbeInterval, m.RunDocker},
		{NodeConditionRebootRequired, rebootProbeInterval, m.RunReboot},
		{NodeConditionClockSync, clockProbeInterval, m.RunClock},
		{NodeConditionOSUpdates, osUpdatesInterval, m.RunOSUpdates},
	} {
		if ctx.Err() != nil {
			return
		}
		due, scheduled := m.next[p.conditionType]
		if scheduled && now.Before(due) {
			continue
		}
		p.run(ctx)
		m.next[p.conditionType] = now.Add(p.every + m.jitter(nodeProbeJitter))
	}
	m.UpdateCollector()
	m.mu.Lock()
	m.evaluatedAt = now
	m.mu.Unlock()
	if m.lastAlerts.IsZero() || now.Sub(m.lastAlerts) >= nodeStatusAlertInterval {
		m.lastAlerts = now
		m.PublishAlerts()
	}
}

// Run evaluates conditions on their schedules until ctx is done.
func (m *nodeStatusManager) Run(ctx context.Context) {
	m.startedAt = m.now()
	m.schedule(m.startedAt)
	ticker := time.NewTicker(nodeStatusLoopInterval)
	defer ticker.Stop()
	for {
		m.RunDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type nodeAlertRule struct {
	conditionType string
	failing       string // condition status that counts as failing
	reason        string
	window        time.Duration
}

var nodeAlertRules = []nodeAlertRule{
	{NodeConditionRebootRequired, NodeConditionWarn, nodeReasonRebootRequired, nodeAlertRebootWindow},
	{NodeConditionOSUpdates, NodeConditionWarn, nodeReasonSecurityUpdates, nodeAlertSecurityWindow},
	{NodeConditionClockSync, NodeConditionFail, nodeReasonClockUnsync, 0},
	{NodeConditionDockerDaemon, NodeConditionFail, nodeReasonDockerUnreachable, nodeAlertDockerWindow},
}

// PublishAlerts reports node conditions as level observations of kind
// node. A condition is failing only once its window, measured from the
// persisted since, has elapsed; ok reports a recovery; unknown and stale
// results report nothing. metrics-collector is never published.
func (m *nodeStatusManager) PublishAlerts() {
	if m.alerts == nil || !m.alerts.AnySubscribed(AlertKindNode) {
		return
	}
	now := m.now()
	m.mu.Lock()
	conditions := make(map[string]NodeCondition, len(m.doc.Conditions))
	for key, condition := range m.doc.Conditions {
		conditions[key] = condition
	}
	var osStatus NodeOSStatus
	if m.doc.OS != nil {
		osStatus = *m.doc.OS
	}
	m.mu.Unlock()
	for _, rule := range nodeAlertRules {
		condition, ok := conditions[rule.conditionType]
		if !ok || condition.Stale || condition.Status == NodeConditionUnknown {
			continue
		}
		observation := LevelObservation{
			Kind: AlertKindNode, Subject: rule.conditionType, Interval: nodeStatusAlertInterval, ObservedAt: now,
			Details: map[string]string{"status": condition.Status},
		}
		switch {
		case condition.Status == NodeConditionOK:
		case condition.Status == rule.failing && now.Sub(condition.Since) >= rule.window:
			observation.Failing = []string{rule.reason}
			observation.Details["since"] = condition.Since.UTC().Format(time.RFC3339)
			switch rule.conditionType {
			case NodeConditionOSUpdates:
				observation.Details["pendingUpdates"] = strconv.Itoa(osStatus.PendingUpdates)
				observation.Details["securityUpdates"] = strconv.Itoa(osStatus.SecurityUpdates)
			case NodeConditionRebootRequired:
				observation.Details["packageCount"] = strconv.Itoa(len(osStatus.RebootPackages))
			}
		default:
			// Inside the window: reported as passing, which never opens and
			// cannot recover an episode that the window has not opened.
		}
		_ = m.alerts.ReportLevel(observation)
	}
}
