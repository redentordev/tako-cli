package selfheal

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Policy is a service's resolved self-healing policy as takod reads it from
// desired state. Its JSON shape is takoapi.SelfHealingPolicyDocument; it is
// a separate type because takod must not import takoapi (takoapi's tests
// import takod).
type Policy struct {
	Enabled            bool `json:"enabled"`
	RestartUnhealthy   bool `json:"restartUnhealthy"`
	StartExited        bool `json:"startExited"`
	UnhealthyThreshold int  `json:"unhealthyThreshold"`
	MaxRestarts        int  `json:"maxRestarts"`
	WindowSeconds      int  `json:"windowSeconds"`
}

// Service states (D9).
const (
	StateOK           = "ok"
	StateWatching     = "watching"
	StateCooldown     = "cooldown"
	StateGaveUp       = "gave_up"
	StatePaused       = "paused"
	StateNotConverged = "not-converged"
	StateDisabled     = "disabled"
	StateBlocked      = "blocked"
)

// Container states (D9).
const (
	ContainerHealthy          = "healthy"
	ContainerRunning          = "running"
	ContainerUnhealthy        = "unhealthy"
	ContainerExited           = "exited"
	ContainerStopped          = "stopped"
	ContainerCompleted        = "completed"
	ContainerMissing          = "missing"
	ContainerDockerRestarting = "docker-restarting"
	ContainerInactive         = "inactive"
	ContainerReplaced         = "replaced"
	ContainerUnknown          = "unknown"
)

// Actions. v1 never creates containers.
const (
	ActionRestart = "restart"
	ActionStart   = "start"
)

// Reason codes (D3). Error strings and logs are never recorded.
const (
	ReasonDockerUnhealthy = "docker_unhealthy"
	ReasonHTTPStatus      = "http_status"
	ReasonHTTPTimeout     = "http_timeout"
	ReasonConnectFailed   = "connect_failed"
	ReasonExitedNonzero   = "exited_nonzero"
	ReasonOOMKilled       = "oom_killed"
)

// Skip reasons (D4).
const (
	SkipLeaseHeld        = "lease_held"
	SkipWorkerFence      = "worker_fence"
	SkipNodeUpgrade      = "node_upgrade"
	SkipRestorePending   = "restore_pending"
	SkipNotSchedulable   = "node_not_schedulable"
	SkipDenyLatch        = "deny_latch"
	SkipMembership       = "membership_unavailable"
	SkipWorkloadKind     = "workload_kind"
	SkipRemovalPending   = "removal_pending"
	SkipNoLocalReplicas  = "no_local_replicas"
	SkipRetention        = "retention"
	SkipBackupRunning    = "backup_running"
	SkipMaintenance      = "maintenance"
	SkipStaleObservation = "observations_stale"
	SkipCircuitOpen      = "circuit_open"
	SkipStateUnreadable  = "state_unreadable"
)

// Breaker reasons.
const (
	BreakerMassFailure = "mass_failure"
	BreakerActionRate  = "action_rate"
)

// Timing and limits (D3, D5).
const (
	MinGrace             = 2 * time.Minute
	RecordGrace          = 5 * time.Minute
	MinUnhealthySpan     = 60 * time.Second
	CooldownBase         = 2 * time.Minute
	CooldownMax          = 30 * time.Minute
	BreakerWindow        = 10 * time.Minute
	BreakerMaxActions    = 6
	BreakerOpenFor       = 30 * time.Minute
	SamplerInterval      = 30 * time.Second
	StaleIntervals       = 3
	DefaultProbeInterval = 30 * time.Second
	MinProbeInterval     = 10 * time.Second
	DefaultWindow        = 30 * time.Minute
)

// HealthSource selects where unhealthy observations come from.
type HealthSource string

const (
	HealthNone   HealthSource = ""
	HealthDocker HealthSource = "docker"
	HealthHTTP   HealthSource = "http"
	HealthTCP    HealthSource = "tcp"
)

// Cooldown is the per-container cooldown after the n-th action: 2m doubling
// per action, capped at 30m.
func Cooldown(actions int) time.Duration {
	if actions < 1 {
		actions = 1
	}
	cooldown := CooldownBase
	for i := 1; i < actions; i++ {
		cooldown *= 2
		if cooldown >= CooldownMax {
			return CooldownMax
		}
	}
	if cooldown > CooldownMax {
		return CooldownMax
	}
	return cooldown
}

// Window returns the policy's give-up window.
func Window(policy *Policy) time.Duration {
	if policy == nil || policy.WindowSeconds <= 0 {
		return DefaultWindow
	}
	return time.Duration(policy.WindowSeconds) * time.Second
}

func threshold(policy *Policy) int {
	if policy == nil || policy.UnhealthyThreshold <= 0 {
		return 3
	}
	return policy.UnhealthyThreshold
}

func maxRestarts(policy *Policy) int {
	if policy == nil || policy.MaxRestarts <= 0 {
		return 3
	}
	return policy.MaxRestarts
}

// ServiceSpec is the desired-state view of one healed service on this node.
type ServiceSpec struct {
	Name          string
	Policy        *Policy
	Image         string
	LocalReplicas int
	Health        HealthSource
	StartPeriod   time.Duration
	ProbeInterval time.Duration
}

// Observation is one container as sampled from Docker.
type Observation struct {
	ID            string
	Name          string
	Image         string
	Status        string
	Restarting    bool
	OOMKilled     bool
	ExitCode      int
	StartedAt     time.Time
	Health        string
	RestartPolicy string
	Revision      string
	Active        string
}

// ProbeResult is one per-container health probe. Reason is a reason code
// when unhealthy; Unknown marks a probe that could not judge the container.
type ProbeResult struct {
	At      time.Time
	Healthy bool
	Unknown bool
	Reason  string
	Code    int
}

// ServiceInput is everything needed to evaluate one service.
type ServiceInput struct {
	Spec ServiceSpec
	// Record is the converged record; RecordErr marks an unreadable one.
	Record    *Record
	RecordErr bool
	// Skip is a precomputed per-service exclusion (D4), "" when eligible.
	Skip string
	// Observations are the sampled containers of this service by ID.
	Observations map[string]Observation
	ObservedAt   time.Time
	Probes       map[string]ProbeResult
}

// ScopeInput is one environment on this node.
type ScopeInput struct {
	Project     string
	Environment string
	// Skip is an environment-wide exclusion (D4), "" when eligible.
	Skip     string
	Control  *Control
	Services []ServiceInput
}

// ContainerStatus is one recorded container's heal view.
type ContainerStatus struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	State         string     `json:"state"`
	Failures      int        `json:"failures,omitempty"`
	Threshold     int        `json:"threshold,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	Code          int        `json:"code,omitempty"`
	Grace         bool       `json:"grace,omitempty"`
	Actions       int        `json:"actions,omitempty"`
	CooldownUntil *time.Time `json:"cooldownUntil,omitempty"`
}

// Candidate is an action the node may take.
type Candidate struct {
	Project     string
	Environment string
	Service     string
	Container   RecordContainer
	Revision    string
	Action      string
	Reason      string
	Code        int
	Health      HealthSource
	Window      time.Duration
}

// ServiceEval is the evaluation of one service.
type ServiceEval struct {
	Project     string            `json:"-"`
	Environment string            `json:"-"`
	Service     string            `json:"service"`
	State       string            `json:"state"`
	Skip        string            `json:"skip,omitempty"`
	Revision    string            `json:"revision,omitempty"`
	Containers  []ContainerStatus `json:"containers,omitempty"`
	// Candidate is the first action ready to run.
	Candidate *Candidate `json:"-"`
	// NeedsAction is true when some container's condition holds past its
	// grace period, whether or not cooldown or serial rules let it run.
	NeedsAction bool `json:"-"`
	// Eligible counts the service toward the node circuit breaker.
	Eligible bool `json:"-"`
	// GiveUp asks the caller to mark the service gave_up.
	GiveUp       bool   `json:"-"`
	GiveUpReason string `json:"-"`
	// ClearGaveUp asks the caller to clear gave_up (a new revision).
	ClearGaveUp bool `json:"-"`
	// ResetContainers have been healthy for a full window.
	ResetContainers []string `json:"-"`
	// Journal lists decision changes that should be journaled once.
	Journal []JournalEntry `json:"-"`
	Window  time.Duration  `json:"-"`
}

type tracker struct {
	startedAt  time.Time
	failures   int
	since      time.Time
	lastAt     time.Time
	reason     string
	code       int
	lastGoodAt time.Time
	okSince    time.Time
	seen       time.Time
	journaled  string
}

// Engine keeps in-memory health trackers between ticks. Persistent state
// (counters, cooldowns, give-up, breaker) lives in Control and NodeState.
type Engine struct {
	mu       sync.Mutex
	trackers map[string]*tracker
}

// NewEngine returns an empty engine.
func NewEngine() *Engine {
	return &Engine{trackers: map[string]*tracker{}}
}

func trackerKey(project, environment, service, id string) string {
	return project + "/" + environment + "/" + service + "/" + id
}

func (e *Engine) trackerLocked(key string) *tracker {
	t := e.trackers[key]
	if t == nil {
		t = &tracker{}
		e.trackers[key] = t
	}
	return t
}

func fresh(at, now time.Time, interval time.Duration) bool {
	if at.IsZero() {
		return false
	}
	return now.Sub(at) <= time.Duration(StaleIntervals)*interval
}

// Observe feeds new health observations into the trackers. Stale
// observations count as unknown: they reset nothing and never count as
// failures.
func (e *Engine) Observe(project, environment string, in ServiceInput, now time.Time) {
	if in.Record == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	interval := in.Spec.ProbeInterval
	if interval <= 0 {
		interval = DefaultProbeInterval
	}
	for _, container := range in.Record.Containers {
		observation, ok := in.Observations[container.ID]
		if !ok {
			continue
		}
		t := e.trackerLocked(trackerKey(project, environment, in.Spec.Name, container.ID))
		t.seen = now
		if !t.startedAt.Equal(observation.StartedAt) {
			// A new process: earlier failures belonged to the old one.
			t.startedAt = observation.StartedAt
			t.failures, t.since, t.reason, t.code = 0, time.Time{}, "", 0
			t.okSince = time.Time{}
		}
		running := observation.Status == "running" && !observation.Restarting
		if !running {
			t.okSince = time.Time{}
			continue
		}
		var result *ProbeResult
		switch in.Spec.Health {
		case HealthDocker:
			if fresh(in.ObservedAt, now, SamplerInterval) {
				switch observation.Health {
				case "healthy":
					result = &ProbeResult{At: in.ObservedAt, Healthy: true}
				case "unhealthy":
					result = &ProbeResult{At: in.ObservedAt, Reason: ReasonDockerUnhealthy}
				}
			}
		case HealthHTTP, HealthTCP:
			if probe, ok := in.Probes[container.ID]; ok && !probe.Unknown && fresh(probe.At, now, interval) {
				copied := probe
				result = &copied
			}
		default:
			if fresh(in.ObservedAt, now, SamplerInterval) {
				result = &ProbeResult{At: in.ObservedAt, Healthy: true}
			}
		}
		if result == nil || !result.At.After(t.lastAt) {
			continue
		}
		t.lastAt = result.At
		if result.Healthy {
			t.failures, t.since, t.reason, t.code = 0, time.Time{}, "", 0
			t.lastGoodAt = result.At
			if t.okSince.IsZero() {
				t.okSince = result.At
			}
			continue
		}
		if t.failures == 0 {
			t.since = result.At
		}
		t.failures++
		t.reason, t.code = SafeToken(result.Reason), result.Code
		t.okSince = time.Time{}
	}
}

// Forget drops trackers not observed since before cutoff.
func (e *Engine) Forget(cutoff time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, t := range e.trackers {
		if t.seen.Before(cutoff) {
			delete(e.trackers, key)
		}
	}
}

// ForgetScope drops every tracker of an environment.
func (e *Engine) ForgetScope(project, environment string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	prefix := project + "/" + environment + "/"
	for key := range e.trackers {
		if strings.HasPrefix(key, prefix) {
			delete(e.trackers, key)
		}
	}
}

// EvaluateScope evaluates every service of one environment. It reads the
// trackers and the control state but changes neither; the caller applies
// GiveUp, ClearGaveUp, ResetContainers and Journal.
func (e *Engine) EvaluateScope(in ScopeInput, now time.Time) []ServiceEval {
	evals := make([]ServiceEval, 0, len(in.Services))
	services := append([]ServiceInput(nil), in.Services...)
	sort.Slice(services, func(i, j int) bool { return services[i].Spec.Name < services[j].Spec.Name })
	for _, service := range services {
		evals = append(evals, e.evaluateService(in, service, now))
	}
	return evals
}

func (e *Engine) evaluateService(scope ScopeInput, in ServiceInput, now time.Time) ServiceEval {
	spec := in.Spec
	eval := ServiceEval{Project: scope.Project, Environment: scope.Environment, Service: spec.Name, Window: Window(spec.Policy)}
	if in.Record != nil {
		eval.Revision = in.Record.Revision
	}
	if spec.Policy == nil || !spec.Policy.Enabled {
		eval.State = StateDisabled
		return eval
	}
	if scope.Skip != "" {
		eval.State, eval.Skip = StateBlocked, scope.Skip
		return eval
	}
	control := scope.Control
	if _, paused := control.PauseActive(spec.Name, now); paused {
		eval.State = StatePaused
		return eval
	}
	if in.Skip != "" {
		eval.State, eval.Skip = StateBlocked, in.Skip
		return eval
	}
	if in.RecordErr {
		eval.State, eval.Skip = StateNotConverged, SkipStateUnreadable
		return eval
	}
	serviceControl := control.Lookup(spec.Name)
	valid := in.Record.Valid(spec.Image)
	if serviceControl != nil && serviceControl.GaveUp {
		if valid && in.Record.Revision != serviceControl.GaveUpRevision {
			eval.ClearGaveUp = true
			serviceControl = nil
		} else {
			eval.State = StateGaveUp
			return eval
		}
	}
	if !valid || len(in.Record.Containers) != spec.LocalReplicas {
		eval.State = StateNotConverged
		return eval
	}
	if !fresh(in.ObservedAt, now, SamplerInterval) {
		eval.State, eval.Skip = StateWatching, SkipStaleObservation
		for _, container := range in.Record.Containers {
			eval.Containers = append(eval.Containers, ContainerStatus{ID: container.ID, Name: container.Name, State: ContainerUnknown})
		}
		return eval
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	record := in.Record
	window := eval.Window
	notConverged := false
	unhealthy := false
	cooling := false
	for _, container := range record.Containers {
		status := ContainerStatus{ID: container.ID, Name: container.Name}
		key := trackerKey(scope.Project, scope.Environment, spec.Name, container.ID)
		t := e.trackers[key]
		if t == nil {
			t = &tracker{}
		}
		observation, ok := in.Observations[container.ID]
		if !ok {
			status.State = ContainerMissing
			for _, other := range in.Observations {
				if other.Name == container.Name {
					status.State = ContainerReplaced
				}
			}
			if status.State == ContainerReplaced {
				notConverged = true
			} else {
				e.journalOnce(&eval, key, container, record.Revision, DecisionMissing, "", 0)
			}
			eval.Containers = append(eval.Containers, status)
			continue
		}
		if observation.Name != container.Name || observation.Revision != record.Revision {
			status.State = ContainerReplaced
			notConverged = true
			eval.Containers = append(eval.Containers, status)
			continue
		}
		if observation.Active == "false" {
			status.State = ContainerInactive
			eval.Containers = append(eval.Containers, status)
			continue
		}
		action, reason, code := classify(&status, observation, spec, t)
		switch status.State {
		case ContainerCompleted:
			e.journalOnce(&eval, key, container, record.Revision, DecisionCompleted, "", observation.ExitCode)
		case ContainerStopped:
			e.journalOnce(&eval, key, container, record.Revision, DecisionSkippedStopped, "", 0)
		default:
			if t.journaled != "" && e.trackers[key] != nil {
				e.trackers[key].journaled = ""
			}
		}
		if status.State == ContainerUnhealthy {
			unhealthy = true
		}
		var containerControl *ContainerControl
		if serviceControl != nil {
			containerControl = serviceControl.Containers[container.ID]
		}
		inCooldown := false
		if containerControl != nil {
			status.Actions = containerControl.Actions
			if now.Before(containerControl.CooldownUntil) {
				until := containerControl.CooldownUntil
				status.CooldownUntil = &until
				inCooldown = true
				cooling = true
			}
			if containerControl.Actions > 0 && !t.okSince.IsZero() && now.Sub(t.okSince) >= window && action == "" {
				eval.ResetContainers = append(eval.ResetContainers, container.ID)
			}
		}
		if action != "" {
			if !pastGrace(observation, spec, record, now) {
				status.Grace = true
			} else {
				eval.NeedsAction = true
				if !inCooldown && !e.serialBlockedLocked(scope, spec.Name, serviceControl, container.ID, now) && eval.Candidate == nil {
					eval.Candidate = &Candidate{
						Project: scope.Project, Environment: scope.Environment, Service: spec.Name,
						Container: container, Revision: record.Revision, Action: action,
						Reason: reason, Code: code, Health: spec.Health, Window: window,
					}
				}
			}
		}
		eval.Containers = append(eval.Containers, status)
	}
	if notConverged {
		eval.State = StateNotConverged
		eval.Candidate, eval.NeedsAction, eval.ResetContainers = nil, false, nil
		return eval
	}
	eval.Eligible = true
	if eval.NeedsAction && serviceControl.ActionsWithin(window, now) >= maxRestarts(spec.Policy) {
		eval.GiveUp = true
		eval.Candidate = nil
		eval.GiveUpReason = firstReason(eval.Containers)
		eval.State = StateGaveUp
		return eval
	}
	switch {
	case eval.Candidate != nil || unhealthy || eval.NeedsAction:
		eval.State = StateWatching
	case cooling:
		eval.State = StateCooldown
	default:
		eval.State = StateOK
	}
	return eval
}

func firstReason(containers []ContainerStatus) string {
	for _, container := range containers {
		if container.Reason != "" {
			return container.Reason
		}
	}
	return ""
}

func (e *Engine) journalOnce(eval *ServiceEval, key string, container RecordContainer, revision, decision, reason string, code int) {
	t := e.trackerLocked(key)
	if t.journaled == decision {
		return
	}
	t.journaled = decision
	eval.Journal = append(eval.Journal, JournalEntry{
		Service: eval.Service, Container: container.Name, Revision: revision,
		Decision: decision, Reason: reason, Code: code,
	})
}

// serialBlockedLocked keeps the next replica of a service waiting until the
// last acted-on replica is healthy again or its cooldown ended.
func (e *Engine) serialBlockedLocked(scope ScopeInput, service string, control *ServiceControl, id string, now time.Time) bool {
	if control == nil || control.LastContainer == "" || control.LastContainer == id || control.LastActionAt.IsZero() {
		return false
	}
	last := control.Containers[control.LastContainer]
	if last == nil || !now.Before(last.CooldownUntil) {
		return false
	}
	t := e.trackers[trackerKey(scope.Project, scope.Environment, service, control.LastContainer)]
	if t != nil && t.lastGoodAt.After(control.LastActionAt) {
		return false
	}
	return true
}

func normalizeRestartPolicy(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "no"
	}
	if strings.HasPrefix(value, "on-failure") {
		return "on-failure"
	}
	return value
}

// classify fills status from one observation and returns the action its
// condition calls for (D3 table).
func classify(status *ContainerStatus, observation Observation, spec ServiceSpec, t *tracker) (string, string, int) {
	policy := spec.Policy
	if observation.Restarting {
		status.State = ContainerDockerRestarting
		return "", "", 0
	}
	switch observation.Status {
	case "running":
		if t.failures > 0 {
			status.State = ContainerUnhealthy
			status.Failures, status.Threshold = t.failures, threshold(policy)
			status.Reason, status.Code = t.reason, t.code
			if spec.Health != HealthNone && policy.RestartUnhealthy && t.failures >= threshold(policy) && t.lastAt.Sub(t.since) >= MinUnhealthySpan {
				return ActionRestart, t.reason, t.code
			}
			return "", "", 0
		}
		if spec.Health != HealthNone && !t.lastGoodAt.IsZero() {
			status.State = ContainerHealthy
		} else {
			status.State = ContainerRunning
		}
		return "", "", 0
	case "exited":
		switch normalizeRestartPolicy(observation.RestartPolicy) {
		case "always", "unless-stopped":
			status.State = ContainerStopped
			return "", "", 0
		}
		if observation.ExitCode == 0 && !observation.OOMKilled {
			status.State = ContainerCompleted
			return "", "", 0
		}
		status.State = ContainerExited
		status.Reason, status.Code = ReasonExitedNonzero, observation.ExitCode
		if observation.OOMKilled {
			status.Reason = ReasonOOMKilled
		}
		if policy.StartExited {
			return ActionStart, status.Reason, status.Code
		}
		return "", "", 0
	default:
		status.State = ContainerStopped
		return "", "", 0
	}
}

func pastGrace(observation Observation, spec ServiceSpec, record *Record, now time.Time) bool {
	grace := spec.StartPeriod
	if grace < MinGrace {
		grace = MinGrace
	}
	if !observation.StartedAt.IsZero() && now.Before(observation.StartedAt.Add(grace)) {
		return false
	}
	return !now.Before(record.RecordedAt.Add(RecordGrace))
}

// NodeDecision is the node-wide outcome of one tick.
type NodeDecision struct {
	// Pick is the single action to run this tick, if any.
	Pick *Candidate
	// OpenBreaker names why the breaker opens now ("" keeps it as is).
	OpenBreaker string
	// CloseBreaker is set when an open breaker's time is up.
	CloseBreaker bool
}

// DecideNode applies the circuit breaker and serial execution to the
// evaluations of every environment on the node.
func DecideNode(evals []ServiceEval, node *NodeState, now time.Time) NodeDecision {
	var decision NodeDecision
	if node != nil && !node.BreakerOpenUntil.IsZero() && !now.Before(node.BreakerOpenUntil) {
		decision.CloseBreaker = true
	}
	if node.BreakerOpen(now) {
		return decision
	}
	eligible, needing := 0, 0
	var candidates []*Candidate
	for i := range evals {
		if !evals[i].Eligible {
			continue
		}
		eligible++
		if evals[i].NeedsAction {
			needing++
		}
		if evals[i].Candidate != nil {
			candidates = append(candidates, evals[i].Candidate)
		}
	}
	if eligible >= 2 && needing*2 > eligible {
		decision.OpenBreaker = BreakerMassFailure
		return decision
	}
	if len(candidates) == 0 {
		return decision
	}
	if node.ActionsWithin(BreakerWindow, now) >= BreakerMaxActions {
		decision.OpenBreaker = BreakerActionRate
		return decision
	}
	decision.Pick = candidates[0]
	return decision
}

// OpenBreaker opens the node breaker for BreakerOpenFor.
func (n *NodeState) OpenBreaker(reason string, now time.Time) {
	n.BreakerOpenedAt = now.UTC()
	n.BreakerOpenUntil = now.Add(BreakerOpenFor).UTC()
	n.BreakerReason = reason
}

// CloseBreaker closes the node breaker.
func (n *NodeState) CloseBreaker() {
	n.BreakerOpenedAt, n.BreakerOpenUntil, n.BreakerReason = time.Time{}, time.Time{}, ""
}

// RecordAction applies one executed action (successful or failed) to the
// service's counters and cooldowns and to the node's action rate.
func RecordAction(control *Control, node *NodeState, candidate Candidate, decision string, now time.Time) {
	service := control.Service(candidate.Service)
	window := candidate.Window
	if window <= 0 {
		window = DefaultWindow
	}
	service.prune(window, now)
	service.Actions = append(service.Actions, now.UTC())
	container := service.Container(candidate.Container.ID)
	container.Actions++
	container.LastActionAt = now.UTC()
	container.CooldownUntil = now.Add(Cooldown(container.Actions)).UTC()
	service.LastAction = decision
	service.LastActionAt = now.UTC()
	service.LastContainer = candidate.Container.ID
	service.LastReason = SafeToken(candidate.Reason)
	if node != nil {
		kept := node.Actions[:0]
		for _, at := range node.Actions {
			if now.Sub(at) < BreakerWindow {
				kept = append(kept, at)
			}
		}
		node.Actions = append(kept, now.UTC())
	}
}

// ApplyEval applies an evaluation's give-up, clear and reset requests to the
// control state. It reports whether the service gave up or was cleared now.
func ApplyEval(control *Control, eval ServiceEval, now time.Time) (gaveUp, cleared bool) {
	if eval.ClearGaveUp {
		if entry := control.Lookup(eval.Service); entry != nil && entry.GaveUp {
			delete(control.Services, eval.Service)
			cleared = true
		}
	}
	if eval.GiveUp {
		entry := control.Service(eval.Service)
		if !entry.GaveUp {
			entry.GaveUp = true
			entry.GaveUpAt = now.UTC()
			entry.GaveUpRevision = eval.Revision
			entry.GaveUpReason = SafeToken(eval.GiveUpReason)
			gaveUp = true
		}
	}
	if len(eval.ResetContainers) > 0 {
		if entry := control.Lookup(eval.Service); entry != nil {
			for _, id := range eval.ResetContainers {
				if container := entry.Containers[id]; container != nil {
					container.Actions = 0
					container.CooldownUntil = time.Time{}
				}
			}
		}
	}
	return gaveUp, cleared
}
