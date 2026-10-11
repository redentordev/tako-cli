package takod

// Local self-healing (add-local-self-healing). takod restarts or starts a
// converged container of an opted-in service on its own node when the
// container is unhealthy or crashed. It never creates containers, never acts
// while a deploy, restore, fence, upgrade or pause is in effect, and backs
// off, gives up and trips a node circuit breaker instead of looping.
//
// The decision logic lives in pkg/selfheal (pure, clock-injected); this file
// wires it to desired state, the shared container sampler, the probes, the
// alert source API and the action executor (heal_action.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redentordev/tako-cli/pkg/selfheal"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

// CapabilityNodeSelfHealingV1 means the node maintains converged records,
// runs the local self-healing loop for services whose desired state carries
// a selfHealing policy, and serves GET/PUT /v1/heal.
const CapabilityNodeSelfHealingV1 = "node.self-healing-v1"

const (
	healEvaluateInterval = 15 * time.Second
	healProbeTick        = 5 * time.Second
	healMaxProbes        = 4
	healLeaseTTL         = 5 * time.Minute
	healLeaseOperation   = "self-heal"
	healDockerTimeout    = 3 * time.Minute
	healJournalStatus    = 20

	healReasonGaveUp       = "gave_up"
	healReasonCircuitOpen  = "circuit_open"
	healReasonActionFailed = "action_failed"
	healEventGaveUp        = "heal_gave_up"
	healEventResumed       = "heal_resumed"
	healEventCircuitOpen   = "heal_circuit_open"
	healEventCircuitClosed = "heal_circuit_closed"
	healEventActionFailed  = "heal_action_failed"
	healBreakerSubject     = "@node"

	// healReasonReplicasUnhealthy: every local replica of a service needs
	// an action at once; the service is held, not restarted.
	healReasonReplicasUnhealthy = "replicas_unhealthy"
	healEventReplicasUnhealthy  = "heal_replicas_unhealthy"
	healEventReplicasRecovered  = "heal_replicas_recovered"
)

// healAlertSourceSpec registers kind selfheal (monitoring design D13).
// Successful actions are never delivered; they are journaled only.
func healAlertSourceSpec() AlertSourceSpec {
	return AlertSourceSpec{
		Kind: AlertKindSelfHeal,
		Reasons: map[string]Severity{
			healReasonGaveUp:            SeverityCritical,
			healReasonCircuitOpen:       SeverityCritical,
			healReasonActionFailed:      SeverityWarning,
			healReasonReplicasUnhealthy: SeverityWarning,
		},
		DetailKeys: []string{"container", "action", "reason", "code", "actions", "breaker"},
		Events: map[string]string{
			healReasonGaveUp:            healEventGaveUp,
			healReasonCircuitOpen:       healEventCircuitOpen,
			healReasonActionFailed:      healEventActionFailed,
			healReasonReplicasUnhealthy: healEventReplicasUnhealthy,
		},
		Recoveries: map[string]string{
			healReasonGaveUp:            healEventResumed,
			healReasonCircuitOpen:       healEventCircuitClosed,
			healReasonReplicasUnhealthy: healEventReplicasRecovered,
		},
	}
}

// healDesiredRevision is the subset of desired state self-healing reads.
type healDesiredRevision struct {
	Services map[string]healDesiredService `json:"services"`
}

type healDesiredService struct {
	Image        string `json:"image"`
	Restart      string `json:"restart"`
	WorkloadKind string `json:"workloadKind"`
	Replicas     int    `json:"replicas"`
	Port         int    `json:"port"`
	Persistent   bool   `json:"persistent"`
	Assignments  []struct {
		Slot   int    `json:"slot"`
		NodeID string `json:"nodeId"`
		Node   string `json:"node"`
	} `json:"assignments"`
	Placement *struct {
		Strategy string `json:"strategy"`
	} `json:"placement"`
	HealthCheck struct {
		Command     string `json:"command"`
		Path        string `json:"path"`
		TCPPort     int    `json:"tcpPort"`
		Interval    string `json:"interval"`
		Timeout     string `json:"timeout"`
		StartPeriod string `json:"startPeriod"`
	} `json:"healthCheck"`
	RemovalPending bool             `json:"removalPending"`
	SelfHealing    *selfheal.Policy `json:"selfHealing"`
}

// healScope is one environment with desired state on this node.
type healScope struct {
	Project     string
	Environment string
	Desired     *healDesiredRevision
	Err         error
}

// healService is one service of a scope with its derived spec.
type healService struct {
	Name    string
	Desired healDesiredService
	Spec    selfheal.ServiceSpec
	Health  *HealthSpec
}

// healManager runs the self-healing loop of one takod.
type healManager struct {
	dataDir string
	node    func() string
	nodeID  func() string
	alerts  *SourceAlertManager
	engine  *selfheal.Engine
	now     func() time.Time

	// samples returns the shared sampler's latest observations.
	samples func() (map[string]ContainerSample, time.Time)
	// nodeGate returns a node-wide skip reason or "".
	nodeGate func(now time.Time) string
	// probe runs one per-container health probe.
	probe func(ctx context.Context, network, container string, spec *HealthSpec) selfheal.ProbeResult
	// executor runs one action under the locks (heal_action.go).
	executor *healExecutor
	// jitter is the per-service action delay (selfheal.ServiceJitter);
	// tests replace it.
	jitter func(project, environment string, spec selfheal.ServiceSpec) time.Duration

	// active is true while any desired service on the node has a policy;
	// the container sampler samples only while it (or an alert) is wanted.
	active atomic.Bool

	// controlMu serializes read-modify-write of control and node state.
	// Lock order: healScopeLock before controlMu.
	controlMu sync.Mutex

	statusMu  sync.Mutex
	lastEvals map[string]healCachedScope

	probeMu   sync.Mutex
	targets   map[string]healProbeTarget
	probes    map[string]selfheal.ProbeResult
	probeNext map[string]time.Time
	probing   map[string]bool
	slots     chan struct{}
	probeWG   sync.WaitGroup
}

func newHealManager(dataDir string, alerts *SourceAlertManager) *healManager {
	manager := &healManager{
		dataDir:   dataDir,
		node:      func() string { return "" },
		nodeID:    func() string { return "" },
		alerts:    alerts,
		engine:    selfheal.NewEngine(),
		now:       func() time.Time { return time.Now().UTC() },
		samples:   func() (map[string]ContainerSample, time.Time) { return nil, time.Time{} },
		nodeGate:  func(time.Time) string { return "" },
		probe:     probeContainerHealth,
		targets:   map[string]healProbeTarget{},
		probes:    map[string]selfheal.ProbeResult{},
		probeNext: map[string]time.Time{},
		probing:   map[string]bool{},
		slots:     make(chan struct{}, healMaxProbes),
	}
	manager.jitter = func(project, environment string, spec selfheal.ServiceSpec) time.Duration {
		key := manager.nodeID()
		if key == "" {
			key = manager.node()
		}
		return selfheal.ServiceJitter(key, project, environment, spec)
	}
	manager.executor = newHealExecutor(manager)
	return manager
}

// initSelfHealing creates the heal manager, registers kind selfheal and
// hooks the container sampler.
func (s *Server) initSelfHealing() {
	manager := newHealManager(s.dataDir, s.sourceAlerts)
	manager.node = func() string { return s.nodeName }
	manager.nodeID = func() string {
		if s.installation == nil {
			return ""
		}
		return s.installation.NodeID
	}
	manager.nodeGate = s.healNodeGate
	manager.executor.enrolled = func() bool { return s.installation != nil }
	if s.sourceAlerts != nil {
		if err := s.sourceAlerts.RegisterSource(healAlertSourceSpec()); err != nil {
			panic(err)
		}
	}
	if s.containerSampler != nil {
		sampler := s.containerSampler
		manager.samples = sampler.ContainerObservations
		previous := sampler.wanted
		sampler.wanted = func() bool {
			return manager.active.Load() || (previous != nil && previous())
		}
	}
	s.heal = manager
}

// Run is the self-healing loop: probes every 5s, evaluation every 15s. It
// idles (no docker calls, no probes) while no desired service on the node
// has a policy.
func (h *healManager) Run(ctx context.Context) {
	defer h.probeWG.Wait()
	probeTicker := time.NewTicker(healProbeTick)
	defer probeTicker.Stop()
	evaluateTicker := time.NewTicker(healEvaluateInterval)
	defer evaluateTicker.Stop()
	h.refreshActive()
	for {
		select {
		case <-ctx.Done():
			return
		case <-probeTicker.C:
			if h.active.Load() {
				h.scheduleProbes(ctx)
			}
		case <-evaluateTicker.C:
			h.Tick(ctx)
		}
	}
}

// refreshActive sets active from desired state and returns the scopes.
func (h *healManager) refreshActive() []healScope {
	scopes := h.loadScopes()
	active := false
	for _, scope := range scopes {
		if scope.Desired == nil {
			continue
		}
		for _, service := range scope.Desired.Services {
			if service.SelfHealing != nil && service.SelfHealing.Enabled {
				active = true
			}
		}
	}
	h.active.Store(active)
	return scopes
}

// loadScopes reads every desired revision on the node.
func (h *healManager) loadScopes() []healScope {
	matches, _ := filepath.Glob(filepath.Join(h.dataDir, "desired", "*", "*", "revision.json"))
	scopes := make([]healScope, 0, len(matches))
	for _, match := range matches {
		environment := filepath.Base(filepath.Dir(match))
		project := filepath.Base(filepath.Dir(filepath.Dir(match)))
		if !isSafeProjectName(project) || !isSafeRuntimeName(environment) {
			continue
		}
		scope := healScope{Project: project, Environment: environment}
		data, err := os.ReadFile(match)
		if err == nil {
			var desired healDesiredRevision
			if err = json.Unmarshal(data, &desired); err == nil {
				scope.Desired = &desired
			}
		}
		scope.Err = err
		scopes = append(scopes, scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		return alertScopeKey(scopes[i].Project, scopes[i].Environment) < alertScopeKey(scopes[j].Project, scopes[j].Environment)
	})
	return scopes
}

// healServices returns the scope's services that have a policy.
func (h *healManager) healServices(scope healScope) []healService {
	if scope.Desired == nil {
		return nil
	}
	singleNode := true
	for _, service := range scope.Desired.Services {
		if len(service.Assignments) > 0 {
			singleNode = false
		}
	}
	names := make([]string, 0, len(scope.Desired.Services))
	for name, service := range scope.Desired.Services {
		if service.SelfHealing != nil && service.SelfHealing.Enabled && isSafeServiceName(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	services := make([]healService, 0, len(names))
	for _, name := range names {
		desired := scope.Desired.Services[name]
		spec := healServiceSpec(name, desired, h.node(), h.nodeID(), singleNode)
		if h.jitter != nil {
			spec.Jitter = h.jitter(scope.Project, scope.Environment, spec)
		}
		services = append(services, healService{
			Name: name, Desired: desired,
			Spec:   spec,
			Health: healHealthSpec(desired),
		})
	}
	return services
}

func healServiceSpec(name string, desired healDesiredService, node, nodeID string, singleNode bool) selfheal.ServiceSpec {
	spec := selfheal.ServiceSpec{Name: name, Policy: desired.SelfHealing, Image: desired.Image}
	spec.LocalReplicas = healLocalReplicas(desired, node, nodeID, singleNode)
	switch {
	case desired.HealthCheck.Path != "" && healHTTPPort(desired) > 0:
		spec.Health = selfheal.HealthHTTP
	case desired.HealthCheck.TCPPort > 0:
		spec.Health = selfheal.HealthTCP
	case desired.HealthCheck.Command != "":
		spec.Health = selfheal.HealthDocker
	}
	spec.StartPeriod = parseHealDuration(desired.HealthCheck.StartPeriod, 0)
	interval := parseHealDuration(desired.HealthCheck.Interval, selfheal.DefaultProbeInterval)
	if interval < selfheal.MinProbeInterval {
		interval = selfheal.MinProbeInterval
	}
	spec.ProbeInterval = interval
	return spec
}

func healHTTPPort(desired healDesiredService) int {
	if desired.HealthCheck.TCPPort > 0 {
		return desired.HealthCheck.TCPPort
	}
	return desired.Port
}

// healHealthSpec builds the per-container probe spec the way the CLI builds
// the reconcile request's health spec (no smoke check).
func healHealthSpec(desired healDesiredService) *HealthSpec {
	timeout := desired.HealthCheck.Timeout
	if timeout == "" {
		timeout = "5s"
	}
	switch {
	case desired.HealthCheck.Path != "" && healHTTPPort(desired) > 0:
		return &HealthSpec{Path: desired.HealthCheck.Path, Port: healHTTPPort(desired), Scheme: "http", Timeout: timeout}
	case desired.HealthCheck.TCPPort > 0:
		return &HealthSpec{Port: desired.HealthCheck.TCPPort, Timeout: timeout}
	}
	return nil
}

func parseHealDuration(value string, fallback time.Duration) time.Duration {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

// healLocalReplicas is the number of replicas desired on this node: 1 for
// global placement, its assignments, or the replica count when the revision
// has no assignments at all (single-node deploys).
func healLocalReplicas(desired healDesiredService, node, nodeID string, singleNode bool) int {
	if desired.Replicas <= 0 {
		return 0
	}
	if desired.Placement != nil && desired.Placement.Strategy == "global" {
		return 1
	}
	if len(desired.Assignments) == 0 {
		if singleNode {
			return desired.Replicas
		}
		return 0
	}
	count := 0
	for _, assignment := range desired.Assignments {
		if (node != "" && assignment.Node == node) || (nodeID != "" && assignment.NodeID == nodeID) {
			count++
		}
	}
	return count
}

// scopeSkip returns an environment-wide exclusion (D4) or "".
func (h *healManager) scopeSkip(project, environment string, now time.Time) string {
	path, err := leasePath(h.dataDir, project, environment)
	if err != nil {
		return selfheal.SkipStateUnreadable
	}
	lease, err := readLeaseFile(path)
	if err != nil {
		return selfheal.SkipStateUnreadable
	}
	if lease != nil && now.Before(lease.ExpiresAt) {
		return selfheal.SkipLeaseHeld
	}
	return ""
}

// serviceSkip returns a per-service exclusion (D4) or "".
func (h *healManager) serviceSkip(project, environment string, service healService, samples map[string]ContainerSample) string {
	switch service.Desired.WorkloadKind {
	case "job", "run":
		return selfheal.SkipWorkloadKind
	}
	if service.Desired.RemovalPending {
		return selfheal.SkipRemovalPending
	}
	if service.Spec.LocalReplicas == 0 {
		return selfheal.SkipNoLocalReplicas
	}
	if _, err := os.Stat(retainedStatePath(h.dataDir, project, environment, service.Name)); err == nil {
		return selfheal.SkipRetention
	} else if !os.IsNotExist(err) {
		return selfheal.SkipStateUnreadable
	}
	for _, sample := range samples {
		if sample.Project != project || sample.Environment != environment {
			continue
		}
		if sample.Service == service.Name+"-maintenance" && sample.Status == "running" {
			return selfheal.SkipMaintenance
		}
		if sample.Status == "running" && (sample.Role == "scheduled-backup" || sample.Role == "backup-verification") {
			return selfheal.SkipBackupRunning
		}
	}
	return ""
}

func healObservation(sample ContainerSample) selfheal.Observation {
	return selfheal.Observation{
		ID: sample.ID, Name: sample.Name, Image: sample.Image, Status: sample.Status,
		Restarting: sample.Restarting, OOMKilled: sample.OOMKilled, ExitCode: sample.ExitCode,
		StartedAt: sample.StartedAt, Health: sample.Health, RestartPolicy: sample.RestartPolicy,
		Revision: sample.Revision, Active: sample.Active,
	}
}

// scopeInput builds the engine input for one scope; control must be read
// under controlMu.
func (h *healManager) scopeInput(scope healScope, services []healService, control *selfheal.Control, samples map[string]ContainerSample, sampledAt time.Time, nodeSkip string, now time.Time) selfheal.ScopeInput {
	input := selfheal.ScopeInput{Project: scope.Project, Environment: scope.Environment, Control: control}
	input.Skip = nodeSkip
	if input.Skip == "" {
		input.Skip = h.scopeSkip(scope.Project, scope.Environment, now)
	}
	byService := map[string]map[string]selfheal.Observation{}
	for _, sample := range samples {
		if sample.Project != scope.Project || sample.Environment != scope.Environment {
			continue
		}
		if byService[sample.Service] == nil {
			byService[sample.Service] = map[string]selfheal.Observation{}
		}
		byService[sample.Service][sample.ID] = healObservation(sample)
	}
	h.probeMu.Lock()
	probes := make(map[string]selfheal.ProbeResult, len(h.probes))
	for id, result := range h.probes {
		probes[id] = result
	}
	h.probeMu.Unlock()
	for _, service := range services {
		in := selfheal.ServiceInput{
			Spec: service.Spec, Observations: byService[service.Name], ObservedAt: sampledAt,
			Skip: h.serviceSkip(scope.Project, scope.Environment, service, samples),
		}
		record, err := selfheal.ReadRecord(h.dataDir, scope.Project, scope.Environment, service.Name)
		if err != nil {
			in.RecordErr = true
		}
		in.Record = record
		in.Probes = map[string]selfheal.ProbeResult{}
		if record != nil {
			for _, container := range record.Containers {
				if result, ok := probes[container.ID]; ok {
					in.Probes[container.ID] = result
				}
			}
		}
		input.Services = append(input.Services, in)
	}
	return input
}

// Tick runs one evaluation of every scope and at most one action.
func (h *healManager) Tick(ctx context.Context) {
	scopes := h.refreshActive()
	now := h.now()
	if !h.active.Load() {
		h.engine.Forget(now)
		h.clearProbes()
		return
	}
	samples, sampledAt := h.samples()
	nodeSkip := h.nodeGate(now)

	h.controlMu.Lock()
	node, nodeErr := selfheal.ReadNodeState(h.dataDir)
	var allEvals []selfheal.ServiceEval
	var policyScopes []healScope
	targets := map[string]healProbeTarget{}
	for _, scope := range scopes {
		services := h.healServices(scope)
		if len(services) == 0 {
			continue
		}
		policyScopes = append(policyScopes, scope)
		control, controlErr := selfheal.ReadControl(h.dataDir, scope.Project, scope.Environment)
		skip := nodeSkip
		if controlErr != nil || nodeErr != nil {
			skip = selfheal.SkipStateUnreadable
		}
		input := h.scopeInput(scope, services, control, samples, sampledAt, skip, now)
		if input.Skip == "" {
			probeTargets(scope.Project, scope.Environment, services, input.Services, targets)
		}
		for _, service := range input.Services {
			h.engine.Observe(scope.Project, scope.Environment, service, now)
		}
		evals := h.engine.EvaluateScope(input, now)
		h.remember(scope.Project, scope.Environment, evals, now)
		if controlErr == nil {
			h.applyEvals(scope, control, evals, now)
		}
		h.reportServiceLevels(scope, control, services, evals, now)
		allEvals = append(allEvals, evals...)
	}
	decision := selfheal.DecideNode(allEvals, node, now)
	if nodeErr == nil {
		h.applyNodeDecision(policyScopes, node, decision, now)
	}
	h.reportBreaker(policyScopes, node, now)
	h.controlMu.Unlock()
	h.setProbeTargets(targets)
	h.engine.Forget(now.Add(-10 * time.Minute))

	if decision.Pick != nil && nodeErr == nil && decision.OpenBreaker == "" {
		h.executor.Execute(ctx, *decision.Pick)
	}
}

// applyEvals persists give-up, clear and reset requests and journals the
// evaluation's decisions. Callers hold controlMu.
func (h *healManager) applyEvals(scope healScope, control *selfheal.Control, evals []selfheal.ServiceEval, now time.Time) {
	changed := false
	before := len(control.Pauses)
	control.PruneExpiredPauses(now)
	if len(control.Pauses) != before {
		changed = true
	}
	var journal []selfheal.JournalEntry
	for _, eval := range evals {
		gaveUp, cleared := selfheal.ApplyEval(control, eval, now)
		if gaveUp || cleared || len(eval.ResetContainers) > 0 {
			changed = true
		}
		for _, entry := range eval.Journal {
			entry.Time, entry.Node = now, h.node()
			journal = append(journal, entry)
		}
		if gaveUp {
			journal = append(journal, selfheal.JournalEntry{Time: now, Node: h.node(), Service: eval.Service, Revision: eval.Revision, Decision: selfheal.DecisionGaveUp, Reason: eval.GiveUpReason})
			h.stateEvent(scope.Project, scope.Environment, events.TypeHealGaveUp, "self-healing gave up", map[string]string{"service": eval.Service, "reason": selfheal.SafeToken(eval.GiveUpReason)})
		}
		if cleared {
			journal = append(journal, selfheal.JournalEntry{Time: now, Node: h.node(), Service: eval.Service, Revision: eval.Revision, Decision: selfheal.DecisionReset, Reason: "new_revision"})
			h.stateEvent(scope.Project, scope.Environment, events.TypeHealReset, "self-healing resumed for a new revision", map[string]string{"service": eval.Service, "reason": "new_revision"})
		}
	}
	if changed {
		logHealRecordError("control write", selfheal.WriteControl(h.dataDir, scope.Project, scope.Environment, control))
	}
	if len(journal) > 0 {
		logHealRecordError("journal", selfheal.AppendJournal(h.dataDir, scope.Project, scope.Environment, journal...))
	}
}

// applyNodeDecision opens or closes the node circuit breaker. Callers hold
// controlMu.
func (h *healManager) applyNodeDecision(scopes []healScope, node *selfheal.NodeState, decision selfheal.NodeDecision, now time.Time) {
	changed := false
	if decision.CloseBreaker {
		node.CloseBreaker()
		changed = true
		for _, scope := range scopes {
			h.journal(scope.Project, scope.Environment, selfheal.JournalEntry{Time: now, Decision: selfheal.DecisionCircuitClosed})
			h.stateEvent(scope.Project, scope.Environment, events.TypeHealCircuitClosed, "self-healing circuit breaker closed", nil)
		}
	}
	if decision.OpenBreaker != "" {
		node.OpenBreaker(decision.OpenBreaker, now)
		changed = true
		for _, scope := range scopes {
			h.journal(scope.Project, scope.Environment, selfheal.JournalEntry{Time: now, Decision: selfheal.DecisionCircuitOpen, Reason: decision.OpenBreaker})
			h.stateEvent(scope.Project, scope.Environment, events.TypeHealCircuitOpen, "self-healing circuit breaker opened", map[string]string{"reason": decision.OpenBreaker})
		}
	}
	if changed {
		logHealRecordError("node state write", selfheal.WriteNodeState(h.dataDir, node))
	}
}

func (h *healManager) journal(project, environment string, entry selfheal.JournalEntry) {
	if entry.Time.IsZero() {
		entry.Time = h.now()
	}
	entry.Node = h.node()
	logHealRecordError("journal", selfheal.AppendJournal(h.dataDir, project, environment, entry))
}

// reportServiceLevels reports one level per service with a policy: gave_up
// (so a reset or a new revision recovers the alert) and replicas_unhealthy
// (every local replica needs an action; recovers when one is healthy).
func (h *healManager) reportServiceLevels(scope healScope, control *selfheal.Control, services []healService, evals []selfheal.ServiceEval, now time.Time) {
	if h.alerts == nil {
		return
	}
	held := map[string]bool{}
	for _, eval := range evals {
		if eval.Skip == selfheal.SkipAllReplicasUnhealthy {
			held[eval.Service] = true
		}
	}
	for _, service := range services {
		observation := LevelObservation{
			Project: scope.Project, Environment: scope.Environment, Kind: AlertKindSelfHeal, Subject: service.Name,
			Interval: healEvaluateInterval, ObservedAt: now,
		}
		if entry := control.Lookup(service.Name); entry != nil && entry.GaveUp {
			observation.Failing = []string{healReasonGaveUp}
			observation.Details = map[string]string{"actions": fmt.Sprint(len(entry.Actions))}
			if reason := selfheal.SafeToken(entry.GaveUpReason); reason != "" && reason != "unknown" {
				observation.Details["reason"] = reason
			}
		} else if held[service.Name] {
			observation.Failing = []string{healReasonReplicasUnhealthy}
		}
		_ = h.alerts.ReportLevel(observation)
	}
}

// reportBreaker reports the node breaker as subject @node of every scope
// with a policy (kind selfheal is service-bound, not node-scoped).
func (h *healManager) reportBreaker(scopes []healScope, node *selfheal.NodeState, now time.Time) {
	if h.alerts == nil {
		return
	}
	for _, scope := range scopes {
		observation := LevelObservation{
			Project: scope.Project, Environment: scope.Environment, Kind: AlertKindSelfHeal, Subject: healBreakerSubject,
			Interval: healEvaluateInterval, ObservedAt: now,
		}
		if node.BreakerOpen(now) {
			observation.Failing = []string{healReasonCircuitOpen}
			if node.BreakerReason != "" {
				observation.Details = map[string]string{"breaker": selfheal.SafeToken(node.BreakerReason)}
			}
		}
		_ = h.alerts.ReportLevel(observation)
	}
}

// healStateEventDocument is a heal.* state event.
type healStateEventDocument struct {
	APIVersion    string            `json:"apiVersion"`
	Kind          string            `json:"kind"`
	SchemaVersion int               `json:"schemaVersion"`
	Type          string            `json:"type"`
	Project       string            `json:"project"`
	Environment   string            `json:"environment"`
	Time          time.Time         `json:"time"`
	Message       string            `json:"message"`
	Details       map[string]string `json:"details,omitempty"`
}

// stateEvent appends a heal.* state event. Details carry names and reason
// codes only.
func (h *healManager) stateEvent(project, environment, eventType, message string, details map[string]string) {
	if details == nil {
		details = map[string]string{}
	}
	if node := h.node(); node != "" {
		details["node"] = node
	}
	document := healStateEventDocument{
		APIVersion: events.APIVersionCurrent, Kind: "StateEvent", SchemaVersion: 1,
		Type: eventType, Project: project, Environment: environment,
		Time: h.now(), Message: message, Details: details,
	}
	data, err := json.Marshal(document)
	if err != nil {
		return
	}
	if _, err := AppendStateEvent(context.Background(), h.dataDir, StateDocumentRequest{
		Project: project, Environment: environment, Content: string(data),
	}); err != nil {
		logHealRecordError("state event", err)
	}
}
