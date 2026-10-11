package takod

// Alert source API (design D1/D13 of add-takod-health-monitoring).
//
// Sources are in-process takod components (service checks, host thresholds,
// container OOM/crash-loop detection, certificate expiry, and the sibling
// self-healing, disk-hygiene and node-status components). They never talk to
// alert targets themselves: they register a closed vocabulary once with
// RegisterSource and then report observations with ReportLevel and
// ReportEvent. SourceAlertManager owns episodes, damping, flap control,
// cross-scope deduplication, delivery, retries and targets.
//
// SourceAlertManager is separate from AlertManager (job and backup alerts)
// and persists under <dataDir>/alerts/{config,credentials,state} plus
// <dataDir>/alerts/ledger.json, so a takod without this feature never reads
// the new kinds and job and backup alert storage stays unchanged.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/alerttarget"
	"github.com/redentordev/tako-cli/pkg/recovery"
)

// Severity is the severity of an alert subject. Only warning and critical
// exist: purely informational facts go to journals and state events, never
// to alert targets.
type Severity string

const (
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

func (s Severity) valid() bool { return s == SeverityWarning || s == SeverityCritical }

func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 2
	case SeverityWarning:
		return 1
	default:
		return 0
	}
}

// Alert kinds (design D13). Each kind is registered by exactly one source.
// The selfheal, hygiene and node kinds are reserved for the sibling changes
// add-local-self-healing, add-disk-hygiene-defaults and
// add-server-status-history, which register their reasons and detail keys
// through RegisterSource.
const (
	AlertKindService     = "service"
	AlertKindHost        = "host"
	AlertKindContainer   = "container"
	AlertKindCertificate = "certificate"
	AlertKindSelfHeal    = "selfheal"
	AlertKindHygiene     = "hygiene"
	AlertKindNode        = "node"
)

// Subject hold states reported through LevelObservation.Hold. A held subject
// keeps its damping counters and produces no transition.
const (
	SubjectHoldPaused      = "paused"
	SubjectHoldMaintenance = "maintenance"
	SubjectHoldUnknown     = "unknown"
)

// Subject states reported by Status and `tako monitor`.
const (
	SubjectStateOK          = "ok"
	SubjectStateAlerting    = "alerting"
	SubjectStatePaused      = "paused"
	SubjectStateMaintenance = "maintenance"
	SubjectStateFlapping    = "flapping"
	SubjectStateStale       = "stale"
	SubjectStateUnknown     = "unknown"
)

// Manager-owned detail keys. Sources cannot report them; the manager adds
// them to deliveries.
const (
	alertDetailRepeats             = "repeats"
	alertDetailAddedReasons        = "addedReasons"
	alertDetailRetired             = "retired"
	alertDetailConsecutiveFailures = "consecutiveFailures"
)

const (
	sourceAlertsDirName         = "alerts"
	sourceAlertSchemaVersion    = 1
	maxSourceAlertDeliveries    = 1024
	maxSourceAlertSubjects      = 4096
	sourceFlapTransitionLimit   = 6
	sourceFlapWindow            = time.Hour
	sourceFlapStableFor         = 30 * time.Minute
	defaultSourceEventCooldown  = 15 * time.Minute
	defaultSourceReportInterval = 60 * time.Second
	sourceStaleIntervals        = 3
	serviceWebhookProvider      = "service-webhook"
)

var (
	alertTokenPattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	alertDetailValuePattern = regexp.MustCompile(`^[A-Za-z0-9._:@/-]{1,128}$`)
	alertSubjectPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)
	alertDetailKeyPattern   = regexp.MustCompile(`^[a-z][A-Za-z0-9]{0,63}$`)
)

// AlertSourceSpec is registered once at takod startup by the component that
// owns Kind.
type AlertSourceSpec struct {
	// Kind is the subject kind from the D13 table, e.g. "service", "host".
	Kind string
	// Reasons is the closed reason vocabulary with each reason's default
	// severity.
	Reasons map[string]Severity
	// DetailKeys allowlists detail keys. Values must be enum tokens or
	// numbers matching ^[A-Za-z0-9._:@/-]{1,128}$.
	DetailKeys []string
	// Events maps a reason to the notification event name used for
	// opening, escalation, suppressed and event deliveries.
	Events map[string]string
	// Recoveries maps a reason to the event name used for recovery; when a
	// reason has none, the opening event name is used.
	Recoveries map[string]string
	// NodeScoped marks node-global kinds (host, hygiene, node). Their
	// observations may leave Project/Environment empty to fan out to every
	// subscribing scope, and their deliveries are deduplicated across scopes
	// per target endpoint.
	NodeScoped bool
}

// LevelObservation is a level-triggered sample. A subject is failing when
// len(Failing) > 0. Sources report every evaluation; a subject not reported
// for longer than 3x Interval is shown as stale (no transition).
type LevelObservation struct {
	// Project and Environment name the scope. They may be empty only for
	// NodeScoped kinds, which then fan out to every subscribing scope.
	Project, Environment string
	// Kind is a registered kind. Subject is the service name for
	// service-bound kinds, otherwise a domain, filesystem or condition
	// name; subjects that are not services start with "@" (e.g. "@node").
	Kind, Subject string
	// Failing lists registered reason codes; empty means passing.
	Failing []string
	// Severities optionally overrides the registered default severity of a
	// failing reason (warning or critical only).
	Severities map[string]Severity
	// Details carries allowlisted keys only.
	Details map[string]string
	// Damping is per subject; zero values mean OpenAfter=1, RecoverAfter=1.
	Damping Damping
	// Interval is the source's reporting interval (default 60s); it sets
	// the staleness horizon.
	Interval time.Duration
	// Hold reports the subject paused, in maintenance or unknown instead
	// of passing or failing; HoldReason is a reason token shown in status
	// (e.g. "operation", "grace", "no_local_route").
	Hold, HoldReason string
	ObservedAt       time.Time
}

// EventObservation is a one-shot occurrence (e.g. one OOM kill). It is
// delivered once per (subject, reason) per Cooldown (default 15m); further
// occurrences inside the cooldown increment a counter carried as
// details.repeats in the next delivery.
type EventObservation struct {
	Project, Environment, Kind, Subject, Reason string
	// Severity optionally overrides the registered default severity.
	Severity   Severity
	Details    map[string]string
	Cooldown   time.Duration
	ObservedAt time.Time
}

// Damping controls when a level subject opens and recovers.
type Damping struct {
	// OpenAfter consecutive failing observations open the subject
	// (default 1).
	OpenAfter int
	// OpenFor additionally requires failing continuously this long before
	// opening (default 0: no duration requirement).
	OpenFor time.Duration
	// RecoverAfter consecutive passing observations recover it (default 1).
	RecoverAfter int
}

// SourceAlertStatus is the additive "sources" object of GET /v1/alerts and
// of each /v1/metrics alerts[] entry. It never contains targets.
type SourceAlertStatus struct {
	EvaluatedAt         time.Time              `json:"evaluatedAt,omitempty"`
	Subjects            []SourceSubjectStatus  `json:"subjects"`
	Deliveries          []SourceDeliveryStatus `json:"deliveries,omitempty"`
	PendingDeliveries   int                    `json:"pendingDeliveries"`
	ExhaustedDeliveries int                    `json:"exhaustedDeliveries"`
}

// SourceSubjectStatus is the node's view of one alert subject.
type SourceSubjectStatus struct {
	Kind           string            `json:"kind"`
	Subject        string            `json:"subject"`
	Node           string            `json:"node,omitempty"`
	State          string            `json:"state"`
	Severity       string            `json:"severity,omitempty"`
	Reasons        []string          `json:"reasons,omitempty"`
	Since          time.Time         `json:"since,omitempty"`
	LastObservedAt time.Time         `json:"lastObservedAt,omitempty"`
	Details        map[string]string `json:"details,omitempty"`
	Paused         bool              `json:"paused,omitempty"`
	Flapping       bool              `json:"flapping,omitempty"`
}

// SourceDeliveryStatus is one queued delivery without its target.
type SourceDeliveryStatus struct {
	ID            string    `json:"id"`
	EpisodeID     string    `json:"episodeId,omitempty"`
	Kind          string    `json:"kind"`
	Subject       string    `json:"subject"`
	Transition    string    `json:"transition"`
	Event         string    `json:"event,omitempty"`
	Provider      string    `json:"provider"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"nextAttemptAt,omitempty"`
	Exhausted     bool      `json:"exhausted,omitempty"`
	LastError     string    `json:"lastError,omitempty"`
}

// SourceAlertPolicy is the node-side health monitoring part of an alert
// policy (AlertConfigRequest's additive fields).
type SourceAlertPolicy struct {
	Monitors     []MonitorSpec           `json:"monitors,omitempty"`
	Host         *HostAlertPolicy        `json:"host,omitempty"`
	Containers   *ContainerAlertPolicy   `json:"containers,omitempty"`
	Certificates *CertificateAlertPolicy `json:"certificates,omitempty"`
}

// SourceScopePolicy is a read-only copy of one scope's source policy for
// in-process sources.
type SourceScopePolicy struct {
	Project     string
	Environment string
	Policy      SourceAlertPolicy
}

type alertKindRule struct {
	nodeScoped   bool
	serviceBound bool
	subscribed   func(scope *sourceAlertScope) bool
}

// alertKindTable is the D13 routing contract: which kinds exist, whether
// they are node-scoped, whether their non-@ subjects are services (and so
// reach that service's monitoring webhook), and when a scope subscribes.
var alertKindTable = map[string]alertKindRule{
	AlertKindService: {serviceBound: true, subscribed: func(s *sourceAlertScope) bool {
		return len(s.policy.Monitors) > 0 && s.hasAnyTarget()
	}},
	AlertKindHost: {nodeScoped: true, subscribed: func(s *sourceAlertScope) bool {
		return s.policy.Host != nil && s.hasSharedTarget()
	}},
	AlertKindContainer: {serviceBound: true, subscribed: func(s *sourceAlertScope) bool {
		return s.policy.Containers != nil && s.hasAnyTarget()
	}},
	AlertKindCertificate: {subscribed: func(s *sourceAlertScope) bool {
		return s.policy.Certificates != nil && s.hasSharedTarget()
	}},
	AlertKindSelfHeal: {serviceBound: true, subscribed: func(s *sourceAlertScope) bool {
		return s.hasAnyTarget()
	}},
	AlertKindHygiene: {nodeScoped: true, subscribed: func(s *sourceAlertScope) bool {
		return s.policy.Host != nil && s.hasSharedTarget()
	}},
	AlertKindNode: {nodeScoped: true, subscribed: func(s *sourceAlertScope) bool {
		return s.policy.Host != nil && s.policy.Host.Conditions && s.hasSharedTarget()
	}},
}

type registeredAlertSource struct {
	spec    AlertSourceSpec
	rule    alertKindRule
	details map[string]bool
}

type sourceAlertTarget struct {
	Provider string `json:"provider"`
	URL      string `json:"url"`
}

type sourceAlertConfigDocument struct {
	SchemaVersion  int               `json:"schemaVersion"`
	Project        string            `json:"project"`
	Environment    string            `json:"environment"`
	Policy         SourceAlertPolicy `json:"policy"`
	Targets        []string          `json:"targets,omitempty"`
	ServiceTargets map[string]string `json:"serviceTargets,omitempty"`
	Retiring       bool              `json:"retiring,omitempty"`
}

type sourceAlertCredentialsDocument struct {
	SchemaVersion int                          `json:"schemaVersion"`
	Project       string                       `json:"project"`
	Environment   string                       `json:"environment"`
	Targets       map[string]sourceAlertTarget `json:"targets"`
}

type sourceEventState struct {
	LastDeliveredAt time.Time `json:"lastDeliveredAt"`
	Repeats         int       `json:"repeats,omitempty"`
}

type sourceSubjectState struct {
	Kind               string                       `json:"kind"`
	Subject            string                       `json:"subject"`
	CreatedAt          time.Time                    `json:"createdAt"`
	Reasons            []string                     `json:"reasons,omitempty"`
	Details            map[string]string            `json:"details,omitempty"`
	Severity           Severity                     `json:"severity,omitempty"`
	Hold               string                       `json:"hold,omitempty"`
	HoldReason         string                       `json:"holdReason,omitempty"`
	ConsecutiveFailing int                          `json:"consecutiveFailing,omitempty"`
	ConsecutivePassing int                          `json:"consecutivePassing,omitempty"`
	FailingSince       time.Time                    `json:"failingSince,omitempty"`
	LastObservedAt     time.Time                    `json:"lastObservedAt,omitempty"`
	IntervalSeconds    int64                        `json:"intervalSeconds,omitempty"`
	Active             bool                         `json:"active,omitempty"`
	ChangedAt          time.Time                    `json:"changedAt,omitempty"`
	Notified           bool                         `json:"notified,omitempty"`
	NotifiedSeverity   Severity                     `json:"notifiedSeverity,omitempty"`
	EpisodeID          string                       `json:"episodeId,omitempty"`
	OpenedAt           time.Time                    `json:"openedAt,omitempty"`
	OpenedReasons      []string                     `json:"openedReasons,omitempty"`
	AddedReasons       []string                     `json:"addedReasons,omitempty"`
	NotifiedTargets    map[string]bool              `json:"notifiedTargets,omitempty"`
	SuppressedTargets  map[string]bool              `json:"suppressedTargets,omitempty"`
	Transitions        []time.Time                  `json:"transitions,omitempty"`
	Flapping           bool                         `json:"flapping,omitempty"`
	LastChangeAt       time.Time                    `json:"lastChangeAt,omitempty"`
	Events             map[string]*sourceEventState `json:"events,omitempty"`
}

type sourceDelivery struct {
	ID              string            `json:"id"`
	EpisodeID       string            `json:"episodeId,omitempty"`
	Kind            string            `json:"kind"`
	Subject         string            `json:"subject"`
	Transition      string            `json:"transition"`
	Event           string            `json:"event"`
	Target          string            `json:"target"`
	Severity        Severity          `json:"severity"`
	Reasons         []string          `json:"reasons,omitempty"`
	Details         map[string]string `json:"details,omitempty"`
	OpenedAt        time.Time         `json:"openedAt,omitempty"`
	CreatedAt       time.Time         `json:"createdAt"`
	Attempts        int               `json:"attempts"`
	NextAttemptAt   time.Time         `json:"nextAttemptAt,omitempty"`
	Exhausted       bool              `json:"exhausted,omitempty"`
	LastError       string            `json:"lastError,omitempty"`
	DispatchStarted bool              `json:"dispatchStarted,omitempty"`
	inFlight        bool
}

type sourceAlertStateDocument struct {
	SchemaVersion int                            `json:"schemaVersion"`
	Project       string                         `json:"project"`
	Environment   string                         `json:"environment"`
	EvaluatedAt   time.Time                      `json:"evaluatedAt,omitempty"`
	Subjects      map[string]*sourceSubjectState `json:"subjects"`
	Deliveries    []sourceDelivery               `json:"deliveries,omitempty"`
}

type sourceAlertScope struct {
	project, environment string
	policy               SourceAlertPolicy
	sharedTargets        []string
	serviceTargets       map[string]string
	targets              map[string]sourceAlertTarget
	retiring             bool
	state                *sourceAlertStateDocument
	dirty                bool
}

func (s *sourceAlertScope) hasSharedTarget() bool { return len(s.sharedTargets) > 0 }
func (s *sourceAlertScope) hasAnyTarget() bool {
	return len(s.sharedTargets) > 0 || len(s.serviceTargets) > 0
}

type sourceLedgerEntry struct {
	Scope     string    `json:"scope"`
	EpisodeID string    `json:"episodeId,omitempty"`
	At        time.Time `json:"at"`
}

type sourceLedgerDocument struct {
	SchemaVersion int                           `json:"schemaVersion"`
	Entries       map[string]*sourceLedgerEntry `json:"entries"`
}

// SourceAlertManager runs episodes and deliveries for registered alert
// sources. It is safe for concurrent use.
type SourceAlertManager struct {
	dataDir string
	now     func() time.Time
	deliver func(context.Context, string, string, map[string]any) alertDeliveryResult
	client  *http.Client

	mu          sync.Mutex
	node        string
	sources     map[string]*registeredAlertSource
	scopes      map[string]*sourceAlertScope
	ledger      map[string]*sourceLedgerEntry
	ledgerDirty bool
	lastPersist error
}

// NewSourceAlertManager returns a manager persisting under
// <dataDir>/alerts. No kinds are registered.
func NewSourceAlertManager(dataDir string) *SourceAlertManager {
	manager := &SourceAlertManager{
		dataDir: dataDir,
		now:     func() time.Time { return time.Now().UTC() },
		client:  newAlertHTTPClient(),
		sources: make(map[string]*registeredAlertSource),
		scopes:  make(map[string]*sourceAlertScope),
		ledger:  make(map[string]*sourceLedgerEntry),
	}
	manager.deliver = func(ctx context.Context, provider, endpoint string, payload map[string]any) alertDeliveryResult {
		return deliverAlertHTTP(ctx, manager.client, provider, endpoint, payload)
	}
	return manager
}

// SetNodeName sets the node name carried in payloads and status.
func (m *SourceAlertManager) SetNodeName(node string) {
	m.mu.Lock()
	m.node = node
	m.mu.Unlock()
}

// RegisterSource registers the vocabulary of one kind. The kind must appear
// in the D13 table, NodeScoped must match it, every reason needs a severity
// and an opening event name, and a kind can be registered only once.
func (m *SourceAlertManager) RegisterSource(spec AlertSourceSpec) error {
	rule, known := alertKindTable[spec.Kind]
	if !known {
		return fmt.Errorf("alert source kind %q is not in the alert kind table", spec.Kind)
	}
	if spec.NodeScoped != rule.nodeScoped {
		return fmt.Errorf("alert source kind %q must set NodeScoped=%t", spec.Kind, rule.nodeScoped)
	}
	if len(spec.Reasons) == 0 {
		return fmt.Errorf("alert source kind %q has no reasons", spec.Kind)
	}
	for reason, severity := range spec.Reasons {
		if !alertTokenPattern.MatchString(reason) {
			return fmt.Errorf("alert source kind %q reason %q is not a snake_case token", spec.Kind, reason)
		}
		if !severity.valid() {
			return fmt.Errorf("alert source kind %q reason %q has severity %q; only warning and critical exist", spec.Kind, reason, severity)
		}
		if !alertTokenPattern.MatchString(spec.Events[reason]) {
			return fmt.Errorf("alert source kind %q reason %q needs a snake_case event name", spec.Kind, reason)
		}
	}
	for reason, event := range spec.Events {
		if _, ok := spec.Reasons[reason]; !ok {
			return fmt.Errorf("alert source kind %q event for unknown reason %q", spec.Kind, reason)
		}
		if !alertTokenPattern.MatchString(event) {
			return fmt.Errorf("alert source kind %q event %q is not a snake_case token", spec.Kind, event)
		}
	}
	for reason, event := range spec.Recoveries {
		if _, ok := spec.Reasons[reason]; !ok || !alertTokenPattern.MatchString(event) {
			return fmt.Errorf("alert source kind %q has an invalid recovery event for %q", spec.Kind, reason)
		}
	}
	details := make(map[string]bool, len(spec.DetailKeys))
	for _, key := range spec.DetailKeys {
		if !alertDetailKeyPattern.MatchString(key) {
			return fmt.Errorf("alert source kind %q detail key %q is invalid", spec.Kind, key)
		}
		switch key {
		case alertDetailRepeats, alertDetailAddedReasons, alertDetailRetired, alertDetailConsecutiveFailures:
			return fmt.Errorf("alert source kind %q detail key %q is reserved for the manager", spec.Kind, key)
		}
		details[key] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sources[spec.Kind]; exists {
		return fmt.Errorf("alert source kind %q is already registered", spec.Kind)
	}
	copied := spec
	copied.Reasons = make(map[string]Severity, len(spec.Reasons))
	for reason, severity := range spec.Reasons {
		copied.Reasons[reason] = severity
	}
	copied.Events = cloneStringMap(spec.Events)
	copied.Recoveries = cloneStringMap(spec.Recoveries)
	copied.DetailKeys = append([]string(nil), spec.DetailKeys...)
	m.sources[spec.Kind] = &registeredAlertSource{spec: copied, rule: rule, details: details}
	return nil
}

// Subscribed reports whether the deployed policy of a scope enables kind.
// Reports for unsubscribed scopes are dropped, so sources may call this to
// skip expensive work.
func (m *SourceAlertManager) Subscribed(project, environment, kind string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := m.scopes[alertScopeKey(project, environment)]
	return m.subscribedLocked(scope, kind)
}

func (m *SourceAlertManager) subscribedLocked(scope *sourceAlertScope, kind string) bool {
	source := m.sources[kind]
	if scope == nil || scope.retiring || source == nil {
		return false
	}
	return source.rule.subscribed(scope)
}

// AnySubscribed reports whether any scope subscribes to kind.
func (m *SourceAlertManager) AnySubscribed(kind string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range m.scopes {
		if m.subscribedLocked(scope, kind) {
			return true
		}
	}
	return false
}

// ScopePolicies returns copies of every active scope's policy, sorted by
// scope.
func (m *SourceAlertManager) ScopePolicies() []SourceScopePolicy {
	m.mu.Lock()
	defer m.mu.Unlock()
	policies := make([]SourceScopePolicy, 0, len(m.scopes))
	for _, scope := range m.scopes {
		if scope.retiring {
			continue
		}
		policies = append(policies, SourceScopePolicy{Project: scope.project, Environment: scope.environment, Policy: cloneSourceAlertPolicy(scope.policy)})
	}
	sort.Slice(policies, func(i, j int) bool {
		return alertScopeKey(policies[i].Project, policies[i].Environment) < alertScopeKey(policies[j].Project, policies[j].Environment)
	})
	return policies
}

func cloneSourceAlertPolicy(policy SourceAlertPolicy) SourceAlertPolicy {
	clone := SourceAlertPolicy{Monitors: append([]MonitorSpec(nil), policy.Monitors...)}
	if policy.Host != nil {
		host := *policy.Host
		clone.Host = &host
	}
	if policy.Containers != nil {
		containers := *policy.Containers
		clone.Containers = &containers
	}
	if policy.Certificates != nil {
		certificates := *policy.Certificates
		clone.Certificates = &certificates
	}
	return clone
}

func (m *SourceAlertManager) validateObservationLocked(project, environment, kind, subject string, reasons []string, details map[string]string) (*registeredAlertSource, error) {
	source := m.sources[kind]
	if source == nil {
		return nil, fmt.Errorf("alert kind %q is not registered", kind)
	}
	if project != "" || environment != "" || !source.spec.NodeScoped {
		if !isSafeProjectName(project) || !isSafeRuntimeName(environment) {
			return nil, fmt.Errorf("invalid alert scope")
		}
	}
	if !validSourceSubject(source.rule, subject) {
		return nil, fmt.Errorf("invalid %s alert subject", kind)
	}
	for _, reason := range reasons {
		if _, ok := source.spec.Reasons[reason]; !ok {
			return nil, fmt.Errorf("reason %q is not registered for alert kind %q", reason, kind)
		}
	}
	for key, value := range details {
		if !source.details[key] {
			return nil, fmt.Errorf("detail key %q is not registered for alert kind %q", key, kind)
		}
		if !alertDetailValuePattern.MatchString(value) {
			return nil, fmt.Errorf("detail %q of alert kind %q is not an enum token or number", key, kind)
		}
	}
	return source, nil
}

// validSourceSubject accepts "@"-prefixed non-service subjects for every
// kind, service names for service-bound kinds, and domain/filesystem/
// condition tokens for the others. Service names only contain
// [A-Za-z0-9_-], so they never collide with "@" subjects.
func validSourceSubject(rule alertKindRule, subject string) bool {
	if strings.HasPrefix(subject, "@") {
		return alertSubjectPattern.MatchString(subject[1:]) && len(subject) <= 64
	}
	if rule.serviceBound {
		return isSafeServiceName(subject)
	}
	return alertSubjectPattern.MatchString(subject)
}

func isNonServiceSubject(subject string) bool { return strings.HasPrefix(subject, "@") }

// ReportLevel records one level observation. A report for an unsubscribed
// scope or kind is dropped silently; an unregistered kind, reason or detail
// key, a malformed detail value or a severity other than warning/critical
// is rejected.
func (m *SourceAlertManager) ReportLevel(observation LevelObservation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.validateObservationLocked(observation.Project, observation.Environment, observation.Kind, observation.Subject, observation.Failing, observation.Details); err != nil {
		return err
	}
	for reason, severity := range observation.Severities {
		if !severity.valid() {
			return fmt.Errorf("severity %q of reason %q is invalid; only warning and critical exist", severity, reason)
		}
	}
	switch observation.Hold {
	case "", SubjectHoldPaused, SubjectHoldMaintenance, SubjectHoldUnknown:
	default:
		return fmt.Errorf("invalid subject hold %q", observation.Hold)
	}
	if observation.HoldReason != "" && !alertTokenPattern.MatchString(observation.HoldReason) {
		return fmt.Errorf("invalid subject hold reason")
	}
	if observation.Damping.OpenAfter < 0 || observation.Damping.RecoverAfter < 0 || observation.Damping.OpenFor < 0 || observation.Interval < 0 {
		return fmt.Errorf("invalid alert damping")
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = m.now()
	}
	for _, scope := range m.targetScopesLocked(observation.Project, observation.Environment, observation.Kind) {
		m.applyLevelLocked(scope, observation)
	}
	return nil
}

// ReportEvent records one event observation; see ReportLevel for
// validation and subscription rules.
func (m *SourceAlertManager) ReportEvent(observation EventObservation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if observation.Reason == "" {
		return fmt.Errorf("event observation requires a reason")
	}
	if _, err := m.validateObservationLocked(observation.Project, observation.Environment, observation.Kind, observation.Subject, []string{observation.Reason}, observation.Details); err != nil {
		return err
	}
	if observation.Severity != "" && !observation.Severity.valid() {
		return fmt.Errorf("severity %q is invalid; only warning and critical exist", observation.Severity)
	}
	if observation.Cooldown < 0 {
		return fmt.Errorf("invalid event cooldown")
	}
	if observation.Cooldown == 0 {
		observation.Cooldown = defaultSourceEventCooldown
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = m.now()
	}
	for _, scope := range m.targetScopesLocked(observation.Project, observation.Environment, observation.Kind) {
		m.applyEventLocked(scope, observation)
	}
	return nil
}

// Retire closes a subject that the source no longer owns (e.g. the http
// checker moved to another node): an open episode gets one recovery with
// details.retired=true to the targets that received the opening, and the
// subject is forgotten. Retiring an unknown subject is a no-op.
func (m *SourceAlertManager) Retire(project, environment, kind, subject string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := m.scopes[alertScopeKey(project, environment)]
	if scope == nil {
		return nil
	}
	m.retireSubjectLocked(scope, sourceSubjectKey(kind, subject))
	return nil
}

// Subjects lists the subject names of kind currently tracked for a scope.
func (m *SourceAlertManager) Subjects(project, environment, kind string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := m.scopes[alertScopeKey(project, environment)]
	if scope == nil {
		return nil
	}
	var subjects []string
	for _, subject := range scope.state.Subjects {
		if subject.Kind == kind {
			subjects = append(subjects, subject.Subject)
		}
	}
	sort.Strings(subjects)
	return subjects
}

func (m *SourceAlertManager) targetScopesLocked(project, environment, kind string) []*sourceAlertScope {
	if project != "" {
		scope := m.scopes[alertScopeKey(project, environment)]
		if !m.subscribedLocked(scope, kind) {
			return nil
		}
		return []*sourceAlertScope{scope}
	}
	keys := make([]string, 0, len(m.scopes))
	for key, scope := range m.scopes {
		if m.subscribedLocked(scope, kind) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	scopes := make([]*sourceAlertScope, 0, len(keys))
	for _, key := range keys {
		scopes = append(scopes, m.scopes[key])
	}
	return scopes
}

func sourceSubjectKey(kind, subject string) string { return kind + "/" + subject }

func (m *SourceAlertManager) subjectLocked(scope *sourceAlertScope, kind, subject string, now time.Time) *sourceSubjectState {
	key := sourceSubjectKey(kind, subject)
	state := scope.state.Subjects[key]
	if state == nil {
		if len(scope.state.Subjects) >= maxSourceAlertSubjects {
			return nil
		}
		state = &sourceSubjectState{Kind: kind, Subject: subject, CreatedAt: now, ChangedAt: now}
		scope.state.Subjects[key] = state
	}
	return state
}

func (m *SourceAlertManager) applyLevelLocked(scope *sourceAlertScope, observation LevelObservation) {
	now := observation.ObservedAt.UTC()
	subject := m.subjectLocked(scope, observation.Kind, observation.Subject, now)
	if subject == nil {
		return
	}
	scope.dirty = true
	interval := observation.Interval
	if interval <= 0 {
		interval = defaultSourceReportInterval
	}
	subject.IntervalSeconds = int64(interval / time.Second)
	subject.LastObservedAt = now
	if observation.Hold != "" {
		subject.Hold = observation.Hold
		subject.HoldReason = observation.HoldReason
		m.syncNotificationLocked(scope, subject, now, false)
		return
	}
	subject.Hold, subject.HoldReason = "", ""
	source := m.sources[observation.Kind]
	failing := sortedUniqueStrings(observation.Failing)
	subject.Details = cloneStringMap(observation.Details)
	if len(failing) > 0 {
		subject.ConsecutiveFailing++
		subject.ConsecutivePassing = 0
		if subject.FailingSince.IsZero() {
			subject.FailingSince = now
		}
		subject.Reasons = failing
		subject.Severity = observationSeverity(source.spec, failing, observation.Severities)
	} else {
		subject.ConsecutivePassing++
		subject.ConsecutiveFailing = 0
		subject.FailingSince = time.Time{}
		subject.Reasons = nil
	}
	openAfter, recoverAfter := observation.Damping.OpenAfter, observation.Damping.RecoverAfter
	if openAfter <= 0 {
		openAfter = 1
	}
	if recoverAfter <= 0 {
		recoverAfter = 1
	}
	changed := false
	switch {
	case !subject.Active && len(failing) > 0 && subject.ConsecutiveFailing >= openAfter && (observation.Damping.OpenFor <= 0 || now.Sub(subject.FailingSince) >= observation.Damping.OpenFor):
		subject.Active = true
		subject.ChangedAt = now
		subject.AddedReasons = nil
		changed = true
	case subject.Active && len(failing) == 0 && subject.ConsecutivePassing >= recoverAfter:
		subject.Active = false
		subject.ChangedAt = now
		changed = true
	case subject.Active && len(failing) > 0:
		if subject.Notified {
			for _, reason := range failing {
				if !containsString(subject.OpenedReasons, reason) && !containsString(subject.AddedReasons, reason) {
					subject.AddedReasons = append(subject.AddedReasons, reason)
				}
			}
			sort.Strings(subject.AddedReasons)
		}
		if severityRank(subject.Severity) > severityRank(subject.NotifiedSeverity) && subject.Notified {
			changed = true
		}
	}
	m.syncNotificationLocked(scope, subject, now, changed)
}

func observationSeverity(spec AlertSourceSpec, reasons []string, overrides map[string]Severity) Severity {
	best := Severity("")
	for _, reason := range reasons {
		severity := spec.Reasons[reason]
		if override, ok := overrides[reason]; ok {
			severity = override
		}
		if severityRank(severity) > severityRank(best) {
			best = severity
		}
	}
	return best
}

// primaryReason is the highest-severity reason, ties broken by name.
func primaryReason(spec AlertSourceSpec, reasons []string, overrides map[string]Severity) string {
	best, bestRank := "", -1
	for _, reason := range sortedUniqueStrings(reasons) {
		severity := spec.Reasons[reason]
		if override, ok := overrides[reason]; ok {
			severity = override
		}
		if rank := severityRank(severity); rank > bestRank {
			best, bestRank = reason, rank
		}
	}
	return best
}

// syncNotificationLocked delivers the difference between the subject's
// damped state and what targets were last told, subject to flap control: at
// most 6 transitions per rolling hour, then one suppressed delivery and
// silence until the subject has been stable for 30 minutes.
func (m *SourceAlertManager) syncNotificationLocked(scope *sourceAlertScope, subject *sourceSubjectState, now time.Time, changed bool) {
	if subject.Flapping {
		if changed {
			subject.LastChangeAt = now
		}
		if now.Sub(subject.LastChangeAt) < sourceFlapStableFor {
			return
		}
		subject.Flapping = false
		subject.Transitions = nil
	}
	var transition string
	switch {
	case subject.Active && !subject.Notified:
		transition = AlertTransitionOpening
	case !subject.Active && subject.Notified:
		transition = AlertTransitionRecovery
	case subject.Active && subject.Notified && severityRank(subject.Severity) > severityRank(subject.NotifiedSeverity):
		transition = AlertTransitionEscalation
	default:
		return
	}
	cutoff := now.Add(-sourceFlapWindow)
	kept := subject.Transitions[:0]
	for _, at := range subject.Transitions {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	subject.Transitions = kept
	if len(subject.Transitions) >= sourceFlapTransitionLimit {
		subject.Flapping = true
		subject.LastChangeAt = now
		m.queueLocked(scope, subject, AlertTransitionSuppressed, m.routingTargetsLocked(scope, subject), now, nil)
		return
	}
	subject.Transitions = append(subject.Transitions, now)
	switch transition {
	case AlertTransitionOpening:
		m.openLocked(scope, subject, now)
	case AlertTransitionRecovery:
		m.recoverLocked(scope, subject, now, nil)
	case AlertTransitionEscalation:
		subject.NotifiedSeverity = subject.Severity
		for index := range scope.state.Deliveries {
			delivery := &scope.state.Deliveries[index]
			if delivery.EpisodeID == subject.EpisodeID && delivery.Transition == AlertTransitionOpening && !delivery.DispatchStarted {
				delivery.Severity = subject.Severity
				delivery.Reasons = append([]string(nil), subject.Reasons...)
			}
		}
		m.queueLocked(scope, subject, AlertTransitionEscalation, sortedTargetSet(subject.NotifiedTargets), now, nil)
	}
}

func (m *SourceAlertManager) openLocked(scope *sourceAlertScope, subject *sourceSubjectState, now time.Time) {
	subject.Notified = true
	subject.NotifiedSeverity = subject.Severity
	subject.EpisodeID = newAlertID()
	subject.OpenedAt = now
	subject.OpenedReasons = append([]string(nil), subject.Reasons...)
	subject.AddedReasons = nil
	subject.NotifiedTargets = map[string]bool{}
	subject.SuppressedTargets = map[string]bool{}
	targets := m.claimLedgerLocked(scope, subject, m.routingTargetsLocked(scope, subject), now)
	extra := map[string]string{}
	if subject.ConsecutiveFailing > 0 {
		extra[alertDetailConsecutiveFailures] = strconv.Itoa(subject.ConsecutiveFailing)
	}
	m.queueLocked(scope, subject, AlertTransitionOpening, targets, now, extra)
}

func (m *SourceAlertManager) recoverLocked(scope *sourceAlertScope, subject *sourceSubjectState, now time.Time, extra map[string]string) {
	filtered := scope.state.Deliveries[:0]
	for _, delivery := range scope.state.Deliveries {
		if delivery.EpisodeID == subject.EpisodeID && delivery.Transition == AlertTransitionOpening && !delivery.inFlight {
			continue
		}
		filtered = append(filtered, delivery)
	}
	scope.state.Deliveries = filtered
	if extra == nil {
		extra = map[string]string{}
	}
	if len(subject.AddedReasons) > 0 {
		extra[alertDetailAddedReasons] = strings.Join(subject.AddedReasons, ",")
	}
	m.queueLocked(scope, subject, AlertTransitionRecovery, sortedTargetSet(subject.NotifiedTargets), now, extra)
	m.releaseLedgerLocked(scope, subject)
	subject.Notified = false
	subject.NotifiedSeverity = ""
	subject.NotifiedTargets = nil
	subject.SuppressedTargets = nil
	subject.AddedReasons = nil
}

// routingTargetsLocked returns the targets a subject's deliveries go to:
// every shared notification target, plus the service's monitoring webhook
// for service-bound subjects of that service.
func (m *SourceAlertManager) routingTargetsLocked(scope *sourceAlertScope, subject *sourceSubjectState) []string {
	targets := append([]string(nil), scope.sharedTargets...)
	if source := m.sources[subject.Kind]; source != nil && source.rule.serviceBound && !isNonServiceSubject(subject.Subject) {
		if target := scope.serviceTargets[subject.Subject]; target != "" {
			targets = append(targets, target)
		}
	}
	return sortedUniqueStrings(targets)
}

func sortedTargetSet(set map[string]bool) []string {
	targets := make([]string, 0, len(set))
	for target, ok := range set {
		if ok {
			targets = append(targets, target)
		}
	}
	sort.Strings(targets)
	return targets
}

func (m *SourceAlertManager) ledgerApplies(subject *sourceSubjectState) bool {
	source := m.sources[subject.Kind]
	return isNonServiceSubject(subject.Subject) || (source != nil && source.spec.NodeScoped)
}

// targetFingerprint is the SHA-256 of provider and normalized URL.
func targetFingerprint(target sourceAlertTarget) string {
	sum := sha256.Sum256([]byte(target.Provider + "\x00" + target.URL))
	return hex.EncodeToString(sum[:])
}

func sourceTargetID(target sourceAlertTarget) string {
	return target.Provider + ":" + targetFingerprint(target)[:16]
}

func ledgerKey(kind, subject, fingerprint string) string {
	return kind + "|" + subject + "|" + fingerprint
}

// claimLedgerLocked filters an opening's targets through the node-global
// ledger: a target whose endpoint already has an open episode for the same
// node-scoped subject from another scope is suppressed for this whole
// episode (opening, escalation and recovery).
func (m *SourceAlertManager) claimLedgerLocked(scope *sourceAlertScope, subject *sourceSubjectState, targets []string, now time.Time) []string {
	if !m.ledgerApplies(subject) {
		return targets
	}
	owner := alertScopeKey(scope.project, scope.environment)
	var allowed []string
	for _, targetID := range targets {
		target, ok := scope.targets[targetID]
		if !ok {
			continue
		}
		key := ledgerKey(subject.Kind, subject.Subject, targetFingerprint(target))
		if entry := m.ledger[key]; entry != nil && entry.Scope != owner {
			if subject.SuppressedTargets == nil {
				subject.SuppressedTargets = map[string]bool{}
			}
			subject.SuppressedTargets[targetID] = true
			continue
		}
		m.ledger[key] = &sourceLedgerEntry{Scope: owner, EpisodeID: subject.EpisodeID, At: now}
		m.ledgerDirty = true
		allowed = append(allowed, targetID)
	}
	return allowed
}

func (m *SourceAlertManager) releaseLedgerLocked(scope *sourceAlertScope, subject *sourceSubjectState) {
	if !m.ledgerApplies(subject) {
		return
	}
	owner := alertScopeKey(scope.project, scope.environment)
	prefix := subject.Kind + "|" + subject.Subject + "|"
	for key, entry := range m.ledger {
		if strings.HasPrefix(key, prefix) && entry.Scope == owner && entry.EpisodeID == subject.EpisodeID {
			delete(m.ledger, key)
			m.ledgerDirty = true
		}
	}
}

func (m *SourceAlertManager) eventEventName(source *registeredAlertSource, reason string) string {
	return source.spec.Events[reason]
}

func (m *SourceAlertManager) applyEventLocked(scope *sourceAlertScope, observation EventObservation) {
	now := observation.ObservedAt.UTC()
	subject := m.subjectLocked(scope, observation.Kind, observation.Subject, now)
	if subject == nil {
		return
	}
	scope.dirty = true
	if subject.Events == nil {
		subject.Events = map[string]*sourceEventState{}
	}
	if subject.LastObservedAt.Before(now) {
		subject.LastObservedAt = now
	}
	if subject.IntervalSeconds == 0 {
		subject.IntervalSeconds = int64(observation.Cooldown / time.Second)
	}
	eventState := subject.Events[observation.Reason]
	if eventState != nil && now.Sub(eventState.LastDeliveredAt) < observation.Cooldown {
		eventState.Repeats++
		return
	}
	repeats := 0
	if eventState != nil {
		repeats = eventState.Repeats
	}
	subject.Events[observation.Reason] = &sourceEventState{LastDeliveredAt: now}
	source := m.sources[observation.Kind]
	severity := source.spec.Reasons[observation.Reason]
	if observation.Severity != "" {
		severity = observation.Severity
	}
	details := cloneStringMap(observation.Details)
	if details == nil {
		details = map[string]string{}
	}
	if repeats > 0 {
		details[alertDetailRepeats] = strconv.Itoa(repeats)
	}
	targets := m.routingTargetsLocked(scope, subject)
	if m.ledgerApplies(subject) {
		owner := alertScopeKey(scope.project, scope.environment)
		var allowed []string
		for _, targetID := range targets {
			target, ok := scope.targets[targetID]
			if !ok {
				continue
			}
			key := ledgerKey(subject.Kind, subject.Subject+"#"+observation.Reason, targetFingerprint(target))
			if entry := m.ledger[key]; entry != nil && entry.Scope != owner && now.Sub(entry.At) < observation.Cooldown {
				continue
			}
			m.ledger[key] = &sourceLedgerEntry{Scope: owner, At: now}
			m.ledgerDirty = true
			allowed = append(allowed, targetID)
		}
		targets = allowed
	}
	for _, target := range targets {
		m.appendDeliveryLocked(scope, sourceDelivery{
			ID: newAlertID(), Kind: subject.Kind, Subject: subject.Subject, Transition: AlertTransitionEvent,
			Event: m.eventEventName(source, observation.Reason), Target: target, Severity: severity,
			Reasons: []string{observation.Reason}, Details: cloneStringMap(details), CreatedAt: now, NextAttemptAt: now,
		})
	}
}

func (m *SourceAlertManager) queueLocked(scope *sourceAlertScope, subject *sourceSubjectState, transition string, targets []string, now time.Time, extra map[string]string) {
	source := m.sources[subject.Kind]
	if source == nil {
		return
	}
	reasons := subject.Reasons
	if transition == AlertTransitionRecovery {
		reasons = subject.OpenedReasons
	}
	event := ""
	primary := primaryReason(source.spec, reasons, nil)
	if primary == "" {
		primary = primaryReason(source.spec, subject.OpenedReasons, nil)
	}
	if transition == AlertTransitionRecovery {
		event = source.spec.Recoveries[primary]
	}
	if event == "" {
		event = source.spec.Events[primary]
	}
	severity := subject.Severity
	if transition == AlertTransitionRecovery {
		severity = subject.NotifiedSeverity
	}
	if severity == "" {
		severity = source.spec.Reasons[primary]
	}
	details := cloneStringMap(subject.Details)
	if details == nil {
		details = map[string]string{}
	}
	for key, value := range extra {
		details[key] = value
	}
	for _, target := range targets {
		duplicate := false
		for _, existing := range scope.state.Deliveries {
			if existing.EpisodeID != "" && existing.EpisodeID == subject.EpisodeID && existing.Transition == transition && existing.Target == target && transition != AlertTransitionEscalation && !existing.Exhausted {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		m.appendDeliveryLocked(scope, sourceDelivery{
			ID: newAlertID(), EpisodeID: subject.EpisodeID, Kind: subject.Kind, Subject: subject.Subject,
			Transition: transition, Event: event, Target: target, Severity: severity,
			Reasons: append([]string(nil), reasons...), Details: cloneStringMap(details), OpenedAt: subject.OpenedAt,
			CreatedAt: now, NextAttemptAt: now,
		})
	}
}

func (m *SourceAlertManager) appendDeliveryLocked(scope *sourceAlertScope, delivery sourceDelivery) bool {
	if len(scope.state.Deliveries) >= maxSourceAlertDeliveries {
		for index, existing := range scope.state.Deliveries {
			if existing.Exhausted && !existing.inFlight {
				scope.state.Deliveries = append(scope.state.Deliveries[:index], scope.state.Deliveries[index+1:]...)
				break
			}
		}
	}
	if len(scope.state.Deliveries) >= maxSourceAlertDeliveries {
		return false
	}
	scope.state.Deliveries = append(scope.state.Deliveries, delivery)
	scope.dirty = true
	return true
}

func (m *SourceAlertManager) retireSubjectLocked(scope *sourceAlertScope, key string) {
	subject := scope.state.Subjects[key]
	if subject == nil {
		return
	}
	if subject.Notified {
		subject.Active = false
		m.recoverLocked(scope, subject, m.now(), map[string]string{alertDetailRetired: "true"})
	}
	filtered := scope.state.Deliveries[:0]
	for _, delivery := range scope.state.Deliveries {
		if sourceSubjectKey(delivery.Kind, delivery.Subject) == key && delivery.Transition != AlertTransitionRecovery && !delivery.inFlight && !delivery.DispatchStarted {
			continue
		}
		filtered = append(filtered, delivery)
	}
	scope.state.Deliveries = filtered
	delete(scope.state.Subjects, key)
	scope.dirty = true
}

// Status returns the node's subject and delivery status for one scope.
func (m *SourceAlertManager) Status(project, environment string) (SourceAlertStatus, error) {
	if !isSafeProjectName(project) || !isSafeRuntimeName(environment) {
		return SourceAlertStatus{}, fmt.Errorf("invalid alert scope")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	status := SourceAlertStatus{Subjects: []SourceSubjectStatus{}}
	scope := m.scopes[alertScopeKey(project, environment)]
	if scope == nil {
		return status, nil
	}
	now := m.now()
	status.EvaluatedAt = scope.state.EvaluatedAt
	for _, subject := range scope.state.Subjects {
		status.Subjects = append(status.Subjects, m.subjectStatusLocked(subject, now))
	}
	sort.Slice(status.Subjects, func(i, j int) bool {
		if status.Subjects[i].Kind != status.Subjects[j].Kind {
			return status.Subjects[i].Kind < status.Subjects[j].Kind
		}
		return status.Subjects[i].Subject < status.Subjects[j].Subject
	})
	for _, delivery := range scope.state.Deliveries {
		provider, _, _ := strings.Cut(delivery.Target, ":")
		status.Deliveries = append(status.Deliveries, SourceDeliveryStatus{
			ID: delivery.ID, EpisodeID: delivery.EpisodeID, Kind: delivery.Kind, Subject: delivery.Subject,
			Transition: delivery.Transition, Event: delivery.Event, Provider: provider, Attempts: delivery.Attempts,
			NextAttemptAt: delivery.NextAttemptAt, Exhausted: delivery.Exhausted, LastError: delivery.LastError,
		})
		if delivery.Exhausted {
			status.ExhaustedDeliveries++
		} else {
			status.PendingDeliveries++
		}
	}
	return status, nil
}

// Configured reports whether a scope has a source policy on this node.
func (m *SourceAlertManager) Configured(project, environment string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := m.scopes[alertScopeKey(project, environment)]
	return scope != nil && !scope.retiring
}

func (m *SourceAlertManager) subjectStatusLocked(subject *sourceSubjectState, now time.Time) SourceSubjectStatus {
	status := SourceSubjectStatus{
		Kind: subject.Kind, Subject: subject.Subject, Node: m.node, Since: subject.ChangedAt,
		LastObservedAt: subject.LastObservedAt, Details: cloneStringMap(subject.Details), Flapping: subject.Flapping,
	}
	interval := time.Duration(subject.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = defaultSourceReportInterval
	}
	stale := !subject.LastObservedAt.IsZero() && now.Sub(subject.LastObservedAt) > sourceStaleIntervals*interval
	switch {
	case subject.Flapping:
		status.State = SubjectStateFlapping
		status.Reasons = append([]string(nil), subject.Reasons...)
	case subject.Hold == SubjectHoldPaused:
		status.State, status.Paused = SubjectStatePaused, true
	case subject.Hold == SubjectHoldMaintenance:
		status.State, status.Paused = SubjectStateMaintenance, true
	case subject.Hold == SubjectHoldUnknown:
		status.State = SubjectStateUnknown
	case stale:
		status.State = SubjectStateStale
	case subject.Active:
		status.State = SubjectStateAlerting
		status.Reasons = append([]string(nil), subject.Reasons...)
		status.Severity = string(subject.Severity)
		status.Since = subject.OpenedAt
		if status.Since.IsZero() {
			status.Since = subject.ChangedAt
		}
	default:
		status.State = SubjectStateOK
	}
	if subject.Hold != "" && subject.HoldReason != "" {
		status.Reasons = []string{subject.HoldReason}
	}
	if status.State == SubjectStateFlapping || status.State == SubjectStateAlerting {
		if status.Severity == "" {
			status.Severity = string(subject.Severity)
		}
	}
	return status
}

// sourcesEnabled reports whether a request carries node-side monitoring.
func sourcesEnabled(request AlertConfigRequest) bool {
	return len(request.Monitors) > 0 || request.Host != nil || request.Containers != nil || request.Certificates != nil
}

// alertConfigEnabled reports whether a request enables any alerting: job
// and backup policy or node-side monitoring sections.
func alertConfigEnabled(request AlertConfigRequest) bool {
	return alertPolicyEnabled(request.Policy) || sourcesEnabled(request)
}

// ApplyConfig installs the node-side monitoring part of request for its
// scope. Episodes on targets that are no longer configured are closed with
// a recovery; open episodes are opened on newly added targets; subjects of
// kinds the policy no longer subscribes are retired.
func (m *SourceAlertManager) ApplyConfig(request AlertConfigRequest) error {
	request = normalizeAlertConfigRequest(request)
	if err := validateAlertConfigRequest(request); err != nil {
		return err
	}
	if !sourcesEnabled(request) {
		return m.RemoveScope(request.Project, request.Environment)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := alertScopeKey(request.Project, request.Environment)
	scope := m.scopes[key]
	if scope == nil {
		scope = &sourceAlertScope{
			project: request.Project, environment: request.Environment,
			targets: map[string]sourceAlertTarget{},
			state:   newSourceAlertState(request.Project, request.Environment),
		}
		m.scopes[key] = scope
	}
	scope.retiring = false
	scope.policy = SourceAlertPolicy{Monitors: append([]MonitorSpec(nil), request.Monitors...), Host: request.Host, Containers: request.Containers, Certificates: request.Certificates}
	scope.sharedTargets = nil
	scope.serviceTargets = map[string]string{}
	for _, provider := range configuredAlertProviders(request.Targets) {
		target := sourceAlertTarget{Provider: provider, URL: alertTargetForProvider(request.Targets, provider)}
		id := sourceTargetID(target)
		scope.targets[id] = target
		scope.sharedTargets = append(scope.sharedTargets, id)
	}
	sort.Strings(scope.sharedTargets)
	for service, url := range request.ServiceTargets {
		target := sourceAlertTarget{Provider: serviceWebhookProvider, URL: url}
		id := sourceTargetID(target)
		scope.targets[id] = target
		scope.serviceTargets[service] = id
	}
	now := m.now()
	for subjectKey, subject := range scope.state.Subjects {
		if !m.subscribedLocked(scope, subject.Kind) || (subject.Kind == AlertKindService && !scopeMonitorsService(scope.policy, subject.Subject)) {
			m.retireSubjectLocked(scope, subjectKey)
			continue
		}
		if !subject.Notified {
			continue
		}
		routing := m.routingTargetsLocked(scope, subject)
		current := map[string]bool{}
		for _, target := range routing {
			current[target] = true
		}
		var removed []string
		for target := range subject.NotifiedTargets {
			if !current[target] {
				removed = append(removed, target)
				delete(subject.NotifiedTargets, target)
			}
		}
		sort.Strings(removed)
		if len(removed) > 0 {
			m.queueLocked(scope, subject, AlertTransitionRecovery, removed, now, nil)
		}
		var missing []string
		for _, target := range routing {
			if subject.NotifiedTargets[target] || subject.SuppressedTargets[target] || m.pendingOpeningLocked(scope, subject, target) {
				continue
			}
			missing = append(missing, target)
		}
		if len(missing) > 0 {
			m.queueLocked(scope, subject, AlertTransitionOpening, m.claimLedgerLocked(scope, subject, missing, now), now, nil)
		}
	}
	m.pruneTargetsLocked(scope)
	scope.dirty = true
	return m.persistScopeLocked(context.Background(), scope, true)
}

func scopeMonitorsService(policy SourceAlertPolicy, service string) bool {
	for _, monitor := range policy.Monitors {
		if monitor.Service == service {
			return true
		}
	}
	return false
}

func (m *SourceAlertManager) pendingOpeningLocked(scope *sourceAlertScope, subject *sourceSubjectState, target string) bool {
	for _, delivery := range scope.state.Deliveries {
		if delivery.EpisodeID == subject.EpisodeID && delivery.Transition == AlertTransitionOpening && delivery.Target == target && !delivery.Exhausted {
			return true
		}
	}
	return false
}

// pruneTargetsLocked drops target URLs that are neither configured nor
// referenced by an open episode or a pending delivery.
func (m *SourceAlertManager) pruneTargetsLocked(scope *sourceAlertScope) {
	needed := map[string]bool{}
	for _, target := range scope.sharedTargets {
		needed[target] = true
	}
	for _, target := range scope.serviceTargets {
		needed[target] = true
	}
	for _, subject := range scope.state.Subjects {
		for target := range subject.NotifiedTargets {
			needed[target] = true
		}
	}
	for _, delivery := range scope.state.Deliveries {
		if !delivery.Exhausted {
			needed[delivery.Target] = true
		}
	}
	for id := range scope.targets {
		if !needed[id] {
			delete(scope.targets, id)
		}
	}
}

// RemoveScope disables node-side monitoring for a scope: open episodes get
// a retired recovery and the scope's alerts tree is removed once those
// deliveries are finished.
func (m *SourceAlertManager) RemoveScope(project, environment string) error {
	if !isSafeProjectName(project) || !isSafeRuntimeName(environment) {
		return fmt.Errorf("invalid alert scope")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := alertScopeKey(project, environment)
	scope := m.scopes[key]
	if scope == nil {
		return m.removeScopeFilesLocked(project, environment)
	}
	scope.retiring = true
	scope.policy = SourceAlertPolicy{}
	for subjectKey := range scope.state.Subjects {
		m.retireSubjectLocked(scope, subjectKey)
	}
	return m.finalizeScopeLocked(context.Background(), scope)
}

func (m *SourceAlertManager) finalizeScopeLocked(ctx context.Context, scope *sourceAlertScope) error {
	if !scope.retiring {
		return nil
	}
	pending := false
	for _, delivery := range scope.state.Deliveries {
		if !delivery.Exhausted {
			pending = true
			break
		}
	}
	if !pending {
		owner := alertScopeKey(scope.project, scope.environment)
		for key, entry := range m.ledger {
			if entry.Scope == owner {
				delete(m.ledger, key)
				m.ledgerDirty = true
			}
		}
		delete(m.scopes, owner)
		if err := m.removeScopeFilesLocked(scope.project, scope.environment); err != nil {
			return err
		}
		return m.persistLedgerLocked(ctx)
	}
	m.pruneTargetsLocked(scope)
	scope.dirty = true
	return m.persistScopeLocked(ctx, scope, true)
}

// Run evaluates staleness and flap release every 30s and dispatches due
// deliveries every second until ctx is done.
func (m *SourceAlertManager) Run(ctx context.Context) {
	evaluate := time.NewTicker(30 * time.Second)
	dispatch := time.NewTicker(time.Second)
	defer evaluate.Stop()
	defer dispatch.Stop()
	for {
		select {
		case <-ctx.Done():
			m.flush(context.Background())
			return
		case <-evaluate.C:
			m.Evaluate()
		case <-dispatch.C:
			m.DispatchDue(ctx)
			m.flush(ctx)
		}
	}
}

// Evaluate releases flapping subjects that have been stable for 30 minutes.
func (m *SourceAlertManager) Evaluate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, scope := range m.scopes {
		for _, subject := range scope.state.Subjects {
			if subject.Flapping {
				m.syncNotificationLocked(scope, subject, now, false)
				scope.dirty = true
			}
		}
		scope.state.EvaluatedAt = now
	}
}

func (m *SourceAlertManager) flush(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for _, scope := range m.scopes {
		if scope.dirty {
			if err := m.persistScopeLocked(ctx, scope, false); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	if m.ledgerDirty {
		if err := m.persistLedgerLocked(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil && (m.lastPersist == nil || m.lastPersist.Error() != firstErr.Error()) {
		fmt.Fprintf(os.Stderr, "takod alert source state persistence deferred: %v\n", firstErr)
	}
	m.lastPersist = firstErr
}

// DispatchDue delivers every due delivery, one at a time, outside the
// manager lock.
func (m *SourceAlertManager) DispatchDue(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		now := m.now()
		var scope *sourceAlertScope
		index := -1
		keys := make([]string, 0, len(m.scopes))
		for key := range m.scopes {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			candidate := m.scopes[key]
			for i, delivery := range candidate.state.Deliveries {
				if !delivery.Exhausted && !delivery.inFlight && !delivery.NextAttemptAt.After(now) {
					scope, index = candidate, i
					break
				}
			}
			if index >= 0 {
				break
			}
		}
		if index < 0 {
			m.mu.Unlock()
			return
		}
		delivery := scope.state.Deliveries[index]
		target, ok := scope.targets[delivery.Target]
		if !ok {
			scope.state.Deliveries[index].Exhausted = true
			scope.state.Deliveries[index].LastError = "target_unavailable"
			scope.dirty = true
			m.mu.Unlock()
			continue
		}
		payload := m.payloadLocked(scope, delivery)
		scope.state.Deliveries[index].inFlight = true
		scope.state.Deliveries[index].DispatchStarted = true
		scope.dirty = true
		m.mu.Unlock()

		result := m.deliver(ctx, target.Provider, target.URL, payload)

		m.mu.Lock()
		if m.scopes[alertScopeKey(scope.project, scope.environment)] != scope {
			m.mu.Unlock()
			continue
		}
		index = sourceDeliveryIndex(scope, delivery.ID)
		if index < 0 {
			m.mu.Unlock()
			continue
		}
		current := &scope.state.Deliveries[index]
		current.inFlight = false
		if result.Interrupted || ctx.Err() != nil {
			current.LastError = "interrupted"
			m.mu.Unlock()
			return
		}
		current.Attempts++
		current.LastError = result.Error
		switch {
		case result.Delivered:
			done := *current
			scope.state.Deliveries = append(scope.state.Deliveries[:index], scope.state.Deliveries[index+1:]...)
			if done.Transition == AlertTransitionOpening {
				subject := scope.state.Subjects[sourceSubjectKey(done.Kind, done.Subject)]
				if subject != nil && subject.Notified && subject.EpisodeID == done.EpisodeID {
					if subject.NotifiedTargets == nil {
						subject.NotifiedTargets = map[string]bool{}
					}
					subject.NotifiedTargets[done.Target] = true
				} else {
					// The episode closed while the opening was in flight:
					// the target saw an opening, so it gets the recovery.
					recoveryDelivery := done
					recoveryDelivery.ID = newAlertID()
					recoveryDelivery.Transition = AlertTransitionRecovery
					if source := m.sources[done.Kind]; source != nil {
						if event := source.spec.Recoveries[primaryReason(source.spec, done.Reasons, nil)]; event != "" {
							recoveryDelivery.Event = event
						}
					}
					recoveryDelivery.Attempts, recoveryDelivery.Exhausted, recoveryDelivery.LastError, recoveryDelivery.DispatchStarted = 0, false, "", false
					recoveryDelivery.NextAttemptAt = m.now()
					m.appendDeliveryLocked(scope, recoveryDelivery)
				}
			}
		case !result.Retryable || current.Attempts >= alertDeliveryMaxAttempts:
			current.Exhausted = true
		default:
			delays := []time.Duration{time.Second, 5 * time.Second}
			current.NextAttemptAt = m.now().Add(delays[current.Attempts-1])
		}
		scope.dirty = true
		if scope.retiring {
			if err := m.finalizeScopeLocked(ctx, scope); err != nil {
				fmt.Fprintf(os.Stderr, "takod alert source retirement cleanup failed: %v\n", err)
			}
		}
		m.mu.Unlock()
	}
}

func sourceDeliveryIndex(scope *sourceAlertScope, id string) int {
	for index := range scope.state.Deliveries {
		if scope.state.Deliveries[index].ID == id {
			return index
		}
	}
	return -1
}

func (m *SourceAlertManager) payloadLocked(scope *sourceAlertScope, delivery sourceDelivery) map[string]any {
	reasons := delivery.Reasons
	message := fmt.Sprintf("Tako alert %s for %s/%s %s %s", delivery.Transition, scope.project, scope.environment, delivery.Kind, delivery.Subject)
	if m.node != "" {
		message += " on " + m.node
	}
	if len(reasons) > 0 {
		message += ": " + strings.Join(reasons, ", ")
	}
	return alertPayloadV1{
		EventID: delivery.ID, EpisodeID: delivery.EpisodeID, Transition: delivery.Transition, Event: delivery.Event,
		Project: scope.project, Environment: scope.environment, Node: m.node, Kind: delivery.Kind, Subject: delivery.Subject,
		Severity: string(delivery.Severity), Reasons: reasons, Details: delivery.Details, OpenedAt: delivery.OpenedAt,
		Message: message, Timestamp: m.now(),
	}.Map()
}

// ---- persistence ----

func newSourceAlertState(project, environment string) *sourceAlertStateDocument {
	return &sourceAlertStateDocument{SchemaVersion: sourceAlertSchemaVersion, Project: project, Environment: environment, Subjects: map[string]*sourceSubjectState{}}
}

func (m *SourceAlertManager) root() string { return filepath.Join(m.dataDir, sourceAlertsDirName) }

func (m *SourceAlertManager) configPath(project, environment string) string {
	return filepath.Join(m.root(), "config", project, environment+".json")
}

func (m *SourceAlertManager) credentialsPath(project, environment string) string {
	return filepath.Join(m.root(), "credentials", project, environment+".json")
}

func (m *SourceAlertManager) statePath(project, environment string) string {
	return filepath.Join(m.root(), "state", project, environment+".json")
}

func (m *SourceAlertManager) ledgerPath() string { return filepath.Join(m.root(), "ledger.json") }

func (m *SourceAlertManager) writeDocument(path string, value any) error {
	if err := validatePathParentsNoSymlink(m.dataDir, path, "alert source path"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data, 0600)
}

// persistScopeLocked writes a scope's documents. withConfig also rewrites
// its policy and credentials. A failure (for example low disk) keeps the
// in-memory state authoritative and dirty so a later flush retries.
func (m *SourceAlertManager) persistScopeLocked(ctx context.Context, scope *sourceAlertScope, withConfig bool) error {
	unlock, err := recovery.AcquireMutationLockContext(ctx, m.dataDir)
	if err != nil {
		scope.dirty = true
		return err
	}
	defer unlock()
	if withConfig {
		credentials := sourceAlertCredentialsDocument{SchemaVersion: sourceAlertSchemaVersion, Project: scope.project, Environment: scope.environment, Targets: map[string]sourceAlertTarget{}}
		for id, target := range scope.targets {
			credentials.Targets[id] = target
		}
		if err := m.writeDocument(m.credentialsPath(scope.project, scope.environment), credentials); err != nil {
			scope.dirty = true
			return err
		}
		config := sourceAlertConfigDocument{
			SchemaVersion: sourceAlertSchemaVersion, Project: scope.project, Environment: scope.environment,
			Policy: scope.policy, Targets: append([]string(nil), scope.sharedTargets...), ServiceTargets: cloneStringMap(scope.serviceTargets), Retiring: scope.retiring,
		}
		if err := m.writeDocument(m.configPath(scope.project, scope.environment), config); err != nil {
			scope.dirty = true
			return err
		}
	}
	if err := m.writeDocument(m.statePath(scope.project, scope.environment), scope.state); err != nil {
		scope.dirty = true
		return err
	}
	scope.dirty = false
	if m.ledgerDirty {
		return m.writeLedgerUnlocked()
	}
	return nil
}

func (m *SourceAlertManager) persistLedgerLocked(ctx context.Context) error {
	unlock, err := recovery.AcquireMutationLockContext(ctx, m.dataDir)
	if err != nil {
		return err
	}
	defer unlock()
	return m.writeLedgerUnlocked()
}

func (m *SourceAlertManager) writeLedgerUnlocked() error {
	if err := m.writeDocument(m.ledgerPath(), sourceLedgerDocument{SchemaVersion: sourceAlertSchemaVersion, Entries: m.ledger}); err != nil {
		return err
	}
	m.ledgerDirty = false
	return nil
}

func (m *SourceAlertManager) removeScopeFilesLocked(project, environment string) error {
	for _, path := range []string{m.configPath(project, environment), m.credentialsPath(project, environment), m.statePath(project, environment)} {
		if err := validatePathParentsNoSymlink(m.dataDir, path, "alert source path"); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = os.Remove(filepath.Dir(path))
	}
	return nil
}

// Load reads persisted scopes. Documents are validated strictly; a scope
// whose documents are invalid or from a newer schema is skipped with a
// warning instead of failing takod startup (the next deploy rewrites it),
// and subjects of kinds no registered source owns are dropped.
func (m *SourceAlertManager) Load(ctx context.Context) error {
	unlock, err := recovery.AcquireMutationLockContext(ctx, m.dataDir)
	if err != nil {
		return err
	}
	defer unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateStorageTreeNoSymlink(m.root(), "alert source storage"); err != nil {
		return err
	}
	m.scopes = make(map[string]*sourceAlertScope)
	err = loadAlertDocuments(filepath.Join(m.root(), "config"), func(path string, data []byte) error {
		scope, loadErr := m.loadScopeLocked(path, data)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "takod: skipping alert source scope %s: %v\n", path, loadErr)
			return nil
		}
		m.scopes[alertScopeKey(scope.project, scope.environment)] = scope
		return nil
	})
	if err != nil {
		return err
	}
	m.ledger = make(map[string]*sourceLedgerEntry)
	if data, readErr := os.ReadFile(m.ledgerPath()); readErr == nil {
		var ledger sourceLedgerDocument
		if json.Unmarshal(data, &ledger) == nil && ledger.SchemaVersion == sourceAlertSchemaVersion {
			for key, entry := range ledger.Entries {
				if entry != nil && m.scopes[entry.Scope] != nil {
					m.ledger[key] = entry
				}
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	return nil
}

func (m *SourceAlertManager) loadScopeLocked(path string, data []byte) (*sourceAlertScope, error) {
	var config sourceAlertConfigDocument
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if config.SchemaVersion != sourceAlertSchemaVersion {
		return nil, fmt.Errorf("unsupported schema version %d", config.SchemaVersion)
	}
	if !isSafeProjectName(config.Project) || !isSafeRuntimeName(config.Environment) || filepath.Clean(path) != filepath.Clean(m.configPath(config.Project, config.Environment)) {
		return nil, fmt.Errorf("config identity mismatch")
	}
	if err := validateSourceAlertPolicy(config.Policy); err != nil {
		return nil, err
	}
	credentialsPath := m.credentialsPath(config.Project, config.Environment)
	info, err := os.Lstat(credentialsPath)
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		return nil, fmt.Errorf("credentials must be a regular 0600 file")
	}
	credentialData, err := os.ReadFile(credentialsPath)
	if err != nil {
		return nil, err
	}
	var credentials sourceAlertCredentialsDocument
	if err := json.Unmarshal(credentialData, &credentials); err != nil || credentials.SchemaVersion != sourceAlertSchemaVersion || credentials.Project != config.Project || credentials.Environment != config.Environment {
		return nil, fmt.Errorf("credentials identity mismatch")
	}
	scope := &sourceAlertScope{
		project: config.Project, environment: config.Environment, policy: config.Policy,
		sharedTargets: append([]string(nil), config.Targets...), serviceTargets: cloneStringMap(config.ServiceTargets),
		targets: map[string]sourceAlertTarget{}, retiring: config.Retiring,
	}
	if scope.serviceTargets == nil {
		scope.serviceTargets = map[string]string{}
	}
	for id, target := range credentials.Targets {
		if !validSourceTarget(target) || sourceTargetID(target) != id {
			return nil, fmt.Errorf("invalid credential target")
		}
		scope.targets[id] = target
	}
	for _, id := range scope.sharedTargets {
		if _, ok := scope.targets[id]; !ok || strings.HasPrefix(id, serviceWebhookProvider+":") {
			return nil, fmt.Errorf("unknown shared target")
		}
	}
	for service, id := range scope.serviceTargets {
		if _, ok := scope.targets[id]; !ok || !isSafeServiceName(service) || !strings.HasPrefix(id, serviceWebhookProvider+":") {
			return nil, fmt.Errorf("unknown service target")
		}
	}
	scope.state = newSourceAlertState(config.Project, config.Environment)
	stateData, err := os.ReadFile(m.statePath(config.Project, config.Environment))
	if err == nil {
		var state sourceAlertStateDocument
		if err := json.Unmarshal(stateData, &state); err != nil || state.SchemaVersion != sourceAlertSchemaVersion || state.Project != config.Project || state.Environment != config.Environment || len(state.Deliveries) > maxSourceAlertDeliveries {
			return nil, fmt.Errorf("invalid state document")
		}
		if state.Subjects == nil {
			state.Subjects = map[string]*sourceSubjectState{}
		}
		for key, subject := range state.Subjects {
			source := (*registeredAlertSource)(nil)
			if subject != nil {
				source = m.sources[subject.Kind]
			}
			if subject == nil || source == nil || key != sourceSubjectKey(subject.Kind, subject.Subject) || !validSourceSubject(source.rule, subject.Subject) {
				delete(state.Subjects, key)
				continue
			}
			for _, reason := range append(append(append([]string(nil), subject.Reasons...), subject.OpenedReasons...), subject.AddedReasons...) {
				if _, ok := source.spec.Reasons[reason]; !ok {
					return nil, fmt.Errorf("invalid subject reason")
				}
			}
			if (subject.Severity != "" && !subject.Severity.valid()) || (subject.NotifiedSeverity != "" && !subject.NotifiedSeverity.valid()) || (subject.Notified && subject.EpisodeID == "") {
				return nil, fmt.Errorf("invalid subject state")
			}
		}
		filtered := state.Deliveries[:0]
		seen := map[string]bool{}
		for _, delivery := range state.Deliveries {
			if delivery.ID == "" || seen[delivery.ID] || !validSourceTransition(delivery.Transition) || delivery.Attempts < 0 || delivery.Attempts > alertDeliveryMaxAttempts || !delivery.Severity.valid() {
				return nil, fmt.Errorf("invalid delivery")
			}
			if m.sources[delivery.Kind] == nil {
				continue
			}
			if _, ok := scope.targets[delivery.Target]; !ok && !delivery.Exhausted {
				return nil, fmt.Errorf("delivery references an unknown target")
			}
			seen[delivery.ID] = true
			filtered = append(filtered, delivery)
		}
		state.Deliveries = filtered
		scope.state = &state
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return scope, nil
}

func validSourceTransition(transition string) bool {
	switch transition {
	case AlertTransitionOpening, AlertTransitionRecovery, AlertTransitionEscalation, AlertTransitionEvent, AlertTransitionSuppressed:
		return true
	default:
		return false
	}
}

func validSourceTarget(target sourceAlertTarget) bool {
	provider := target.Provider
	if provider == serviceWebhookProvider {
		provider = "webhook"
	}
	if !validAlertProvider(provider) {
		return false
	}
	normalized, err := alerttarget.NormalizeAndValidate(provider, target.URL)
	return err == nil && normalized == target.URL
}

// ---- policy validation (shared by takod's handler and lifecycle gate) ----

const (
	maxMonitorSpecs          = 200
	maxMonitorIntervalSecond = int64(time.Hour / time.Second)
	maxHealthPathLength      = 1024
)

func validateSourceAlertPolicy(policy SourceAlertPolicy) error {
	if len(policy.Monitors) > maxMonitorSpecs {
		return fmt.Errorf("at most %d monitors per environment", maxMonitorSpecs)
	}
	seen := map[string]bool{}
	for _, monitor := range policy.Monitors {
		if !isSafeServiceName(monitor.Service) || seen[monitor.Service] {
			return fmt.Errorf("invalid monitor service")
		}
		seen[monitor.Service] = true
		if monitor.CheckType != MonitorCheckHTTP && monitor.CheckType != MonitorCheckContainer {
			return fmt.Errorf("invalid monitor check type for %s", monitor.Service)
		}
		if monitor.IntervalSeconds < 10 || monitor.IntervalSeconds > maxMonitorIntervalSecond || monitor.TimeoutSeconds < 1 || monitor.TimeoutSeconds > 60 || monitor.TimeoutSeconds >= monitor.IntervalSeconds {
			return fmt.Errorf("invalid monitor timing for %s", monitor.Service)
		}
		if monitor.AlertAfter < 1 || monitor.AlertAfter > 100 || monitor.RecoverAfter < 1 || monitor.RecoverAfter > 100 {
			return fmt.Errorf("invalid monitor damping for %s", monitor.Service)
		}
		if monitor.CheckerNode != "" && !isSafeRuntimeName(monitor.CheckerNode) {
			return fmt.Errorf("invalid monitor checker node for %s", monitor.Service)
		}
		if monitor.CheckType == MonitorCheckHTTP {
			if domain, err := normalizeProxyCertificateDomain(monitor.Domain); err != nil || domain != monitor.Domain || strings.HasPrefix(domain, "*.") {
				return fmt.Errorf("invalid monitor domain for %s", monitor.Service)
			}
			if !strings.HasPrefix(monitor.HealthPath, "/") || len(monitor.HealthPath) > maxHealthPathLength || hasControlChars(monitor.HealthPath) || strings.ContainsAny(monitor.HealthPath, " #") {
				return fmt.Errorf("invalid monitor health path for %s", monitor.Service)
			}
		}
	}
	if host := policy.Host; host != nil {
		for _, percent := range []int{host.DiskPercent, host.InodesPercent, host.MemoryPercent, host.CPUPercent} {
			if percent < 0 || percent > 99 {
				return fmt.Errorf("invalid host alert percent")
			}
		}
		if host.CPUForSeconds < 0 || host.CPUForSeconds > 86400 || host.LoadPerCore < 0 || host.LoadPerCore > 1000 || math.IsNaN(host.LoadPerCore) {
			return fmt.Errorf("invalid host alert policy")
		}
	}
	if containers := policy.Containers; containers != nil {
		if containers.CrashLoopRestarts < 0 || containers.CrashLoopRestarts > 1000 || containers.CrashLoopWindowSeconds < 0 || containers.CrashLoopWindowSeconds > 86400 || (containers.CrashLoopRestarts > 0 && containers.CrashLoopWindowSeconds < 60) {
			return fmt.Errorf("invalid container alert policy")
		}
	}
	if certificates := policy.Certificates; certificates != nil {
		if certificates.CriticalBeforeSeconds < 3600 || certificates.WarnBeforeSeconds < certificates.CriticalBeforeSeconds || certificates.WarnBeforeSeconds > int64((365*24*time.Hour)/time.Second) {
			return fmt.Errorf("invalid certificate alert policy")
		}
	}
	return nil
}
