package takod

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/selfheal"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

// Heal control actions of PUT /v1/heal.
const (
	HealActionPause  = "pause"
	HealActionResume = "resume"
	HealActionReset  = "reset"
)

// HealStatusResponse is GET /v1/heal: one environment's self-healing view
// on this node. It carries names, states, reason codes, numbers and times
// only, never logs or error text.
type HealStatusResponse struct {
	Project     string                  `json:"project"`
	Environment string                  `json:"environment"`
	Node        string                  `json:"node,omitempty"`
	Enabled     bool                    `json:"enabled"`
	EvaluatedAt *time.Time              `json:"evaluatedAt,omitempty"`
	Pauses      []selfheal.Pause        `json:"pauses,omitempty"`
	Breaker     HealBreakerStatus       `json:"breaker"`
	Services    []HealServiceStatus     `json:"services"`
	Journal     []selfheal.JournalEntry `json:"journal,omitempty"`
}

// HealBreakerStatus is the node circuit breaker.
type HealBreakerStatus struct {
	Open          bool       `json:"open"`
	Reason        string     `json:"reason,omitempty"`
	OpenedAt      *time.Time `json:"openedAt,omitempty"`
	Until         *time.Time `json:"until,omitempty"`
	RecentActions int        `json:"recentActions"`
}

// HealServiceStatus is one service with a policy on this node.
type HealServiceStatus struct {
	Service       string                     `json:"service"`
	State         string                     `json:"state"`
	Skip          string                     `json:"skip,omitempty"`
	Revision      string                     `json:"revision,omitempty"`
	Policy        *selfheal.Policy           `json:"policy,omitempty"`
	PausedUntil   *time.Time                 `json:"pausedUntil,omitempty"`
	GaveUpAt      *time.Time                 `json:"gaveUpAt,omitempty"`
	GaveUpReason  string                     `json:"gaveUpReason,omitempty"`
	RecentActions int                        `json:"recentActions"`
	LastAction    string                     `json:"lastAction,omitempty"`
	LastActionAt  *time.Time                 `json:"lastActionAt,omitempty"`
	LastReason    string                     `json:"lastReason,omitempty"`
	Containers    []selfheal.ContainerStatus `json:"containers,omitempty"`
}

// HealChangeRequest is PUT /v1/heal.
type HealChangeRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Action      string `json:"action"`
	// Service limits the change to one service; empty means the whole
	// environment.
	Service string `json:"service,omitempty"`
	// Until ends a pause; nil pauses for selfheal.DefaultPause unless
	// Indefinite is set (until resumed).
	Until      *time.Time `json:"until,omitempty"`
	Indefinite bool       `json:"indefinite,omitempty"`
}

// HealChangeResponse is the PUT /v1/heal result on one node.
type HealChangeResponse struct {
	Project     string     `json:"project"`
	Environment string     `json:"environment"`
	Node        string     `json:"node,omitempty"`
	Action      string     `json:"action"`
	Service     string     `json:"service,omitempty"`
	Changed     bool       `json:"changed"`
	PausedUntil *time.Time `json:"pausedUntil,omitempty"`
	Indefinite  bool       `json:"indefinite,omitempty"`
}

func (s *Server) handleHeal(w http.ResponseWriter, r *http.Request) {
	if s.heal == nil {
		http.Error(w, "self-healing is not running", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		project, environment := r.URL.Query().Get("project"), r.URL.Query().Get("environment")
		if !isSafeProjectName(project) || !isSafeRuntimeName(environment) {
			http.Error(w, "invalid project or environment", http.StatusBadRequest)
			return
		}
		limit := healJournalStatus
		if value := r.URL.Query().Get("journal"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 || parsed > selfheal.MaxJournalEntries {
				http.Error(w, "invalid journal limit", http.StatusBadRequest)
				return
			}
			limit = parsed
		}
		writeHygieneJSON(w, s.heal.Status(project, environment, limit))
	case http.MethodPut:
		defer r.Body.Close()
		var request HealChangeRequest
		if err := decodeJSONRequest(w, r, &request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		response, err := s.heal.Change(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeHygieneJSON(w, response)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// remember caches the latest evaluation of a scope for status reads.
// Status never evaluates itself, so reading it cannot consume journal
// decisions of the loop.
func (h *healManager) remember(project, environment string, evals []selfheal.ServiceEval, now time.Time) {
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	if h.lastEvals == nil {
		h.lastEvals = map[string]healCachedScope{}
	}
	h.lastEvals[alertScopeKey(project, environment)] = healCachedScope{Evals: append([]selfheal.ServiceEval(nil), evals...), At: now}
}

type healCachedScope struct {
	Evals []selfheal.ServiceEval
	At    time.Time
}

// Status returns one environment's self-healing view.
func (h *healManager) Status(project, environment string, journalLimit int) HealStatusResponse {
	now := h.now()
	response := HealStatusResponse{Project: project, Environment: environment, Node: h.node(), Services: []HealServiceStatus{}}
	var services []healService
	for _, scope := range h.loadScopes() {
		if scope.Project == project && scope.Environment == environment {
			services = h.healServices(scope)
		}
	}
	response.Enabled = len(services) > 0

	h.controlMu.Lock()
	control, _ := selfheal.ReadControl(h.dataDir, project, environment)
	node, _ := selfheal.ReadNodeState(h.dataDir)
	h.controlMu.Unlock()
	control.PruneExpiredPauses(now)
	response.Pauses = control.Pauses
	response.Breaker = HealBreakerStatus{Open: node.BreakerOpen(now), RecentActions: node.ActionsWithin(selfheal.BreakerWindow, now)}
	if response.Breaker.Open {
		response.Breaker.Reason = node.BreakerReason
		response.Breaker.OpenedAt = timePointer(node.BreakerOpenedAt)
		response.Breaker.Until = timePointer(node.BreakerOpenUntil)
	}

	h.statusMu.Lock()
	cached, haveCached := h.lastEvals[alertScopeKey(project, environment)]
	h.statusMu.Unlock()
	if haveCached {
		response.EvaluatedAt = timePointer(cached.At)
	}
	evals := map[string]selfheal.ServiceEval{}
	for _, eval := range cached.Evals {
		evals[eval.Service] = eval
	}
	for _, service := range services {
		status := HealServiceStatus{Service: service.Name, State: selfheal.StateWatching, Policy: service.Spec.Policy}
		if eval, ok := evals[service.Name]; ok {
			status.State, status.Skip, status.Revision, status.Containers = eval.State, eval.Skip, eval.Revision, eval.Containers
		}
		if pause, paused := control.PauseActive(service.Name, now); paused {
			status.State, status.Skip = selfheal.StatePaused, ""
			if pause.Until != nil {
				until := *pause.Until
				status.PausedUntil = &until
			}
		}
		entry := control.Lookup(service.Name)
		if entry != nil {
			status.RecentActions = entry.ActionsWithin(selfheal.Window(service.Spec.Policy), now)
			status.LastAction, status.LastReason = entry.LastAction, entry.LastReason
			if !entry.LastActionAt.IsZero() {
				status.LastActionAt = timePointer(entry.LastActionAt)
			}
			if entry.GaveUp {
				status.State = selfheal.StateGaveUp
				status.GaveUpAt = timePointer(entry.GaveUpAt)
				status.GaveUpReason = entry.GaveUpReason
			}
		}
		if status.State == selfheal.StateGaveUp && (entry == nil || !entry.GaveUp) {
			// Reset since the last evaluation.
			status.State = selfheal.StateWatching
		}
		response.Services = append(response.Services, status)
	}
	if journalLimit > 0 {
		journal, err := selfheal.ReadJournal(h.dataDir, project, environment, journalLimit)
		if err == nil {
			response.Journal = journal
		}
	}
	return response
}

func validateHealChangeRequest(request HealChangeRequest, now time.Time) error {
	if !isSafeProjectName(request.Project) || !isSafeRuntimeName(request.Environment) {
		return fmt.Errorf("invalid project or environment")
	}
	if request.Service != "" && !isSafeServiceName(request.Service) {
		return fmt.Errorf("invalid service name")
	}
	switch request.Action {
	case HealActionPause:
		if request.Indefinite && request.Until != nil {
			return fmt.Errorf("a pause is either indefinite or has an end time")
		}
		if request.Until != nil {
			if !request.Until.After(now) {
				return fmt.Errorf("pause end time must be in the future")
			}
			if request.Until.Sub(now) > selfheal.MaxPause {
				return fmt.Errorf("pause may last at most %s", selfheal.MaxPause)
			}
		}
	case HealActionResume, HealActionReset:
		if request.Until != nil || request.Indefinite {
			return fmt.Errorf("%s takes no pause duration", request.Action)
		}
	default:
		return fmt.Errorf("action must be pause, resume or reset")
	}
	return nil
}

// Change applies pause, resume or reset. It takes the scope lock first, so
// it waits for an in-flight action of the environment: when a pause
// returns, no action is running and none starts until it ends.
func (h *healManager) Change(request HealChangeRequest) (*HealChangeResponse, error) {
	now := h.now()
	request.Action = strings.TrimSpace(request.Action)
	if err := validateHealChangeRequest(request, now); err != nil {
		return nil, err
	}
	lock := healScopeLock(request.Project, request.Environment)
	lock.Lock()
	defer lock.Unlock()
	h.controlMu.Lock()
	defer h.controlMu.Unlock()
	control, err := selfheal.ReadControl(h.dataDir, request.Project, request.Environment)
	if err != nil {
		return nil, fmt.Errorf("self-healing state is unreadable")
	}
	response := &HealChangeResponse{Project: request.Project, Environment: request.Environment, Node: h.node(), Action: request.Action, Service: request.Service}
	entry := selfheal.JournalEntry{Time: now, Node: h.node(), Service: request.Service}
	var eventType, message string
	details := map[string]string{}
	if request.Service != "" {
		details["service"] = request.Service
	}
	switch request.Action {
	case HealActionPause:
		until := request.Until
		if until == nil && !request.Indefinite {
			value := now.Add(selfheal.DefaultPause)
			until = &value
		}
		control.AddPause(request.Service, until, now)
		response.Changed, response.Indefinite = true, until == nil
		if until != nil {
			value := until.UTC()
			response.PausedUntil = &value
			details["until"] = value.Format(time.RFC3339)
		}
		entry.Decision, eventType, message = selfheal.DecisionPaused, events.TypeHealPaused, "self-healing paused"
	case HealActionResume:
		response.Changed = control.Resume(request.Service, now)
		entry.Decision, eventType, message = selfheal.DecisionResumed, events.TypeHealResumed, "self-healing resumed"
	case HealActionReset:
		var names []string
		if request.Service != "" {
			names = []string{request.Service}
		} else {
			for name := range control.Services {
				names = append(names, name)
			}
			sort.Strings(names)
		}
		for _, name := range names {
			if control.Lookup(name) != nil {
				control.Reset(name)
				response.Changed = true
			}
		}
		if request.Service == "" {
			node, nodeErr := selfheal.ReadNodeState(h.dataDir)
			if nodeErr == nil && !node.BreakerOpenUntil.IsZero() {
				node.CloseBreaker()
				node.Actions = nil
				logHealRecordError("node state write", selfheal.WriteNodeState(h.dataDir, node))
				response.Changed = true
			}
		}
		entry.Decision, eventType, message = selfheal.DecisionReset, events.TypeHealReset, "self-healing reset"
	}
	if response.Changed {
		if err := selfheal.WriteControl(h.dataDir, request.Project, request.Environment, control); err != nil {
			return nil, fmt.Errorf("self-healing state could not be written")
		}
		logHealRecordError("journal", selfheal.AppendJournal(h.dataDir, request.Project, request.Environment, entry))
		h.stateEvent(request.Project, request.Environment, eventType, message, details)
	}
	return response, nil
}
