package selfheal

import (
	"sort"
	"time"
)

// Pause limits, see D7.
const (
	DefaultPause = time.Hour
	MaxPause     = 7 * 24 * time.Hour
)

// Control is the persisted per-environment heal state: pauses, give-up,
// action counters and cooldowns. It survives takod restarts so a restart
// never resets the action budget.
type Control struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Pauses        []Pause                    `json:"pauses,omitempty"`
	Services      map[string]*ServiceControl `json:"services,omitempty"`
}

// Pause stops healing for the environment (Service empty) or one service
// until Until; a nil Until pauses until resumed.
type Pause struct {
	Service   string     `json:"service,omitempty"`
	Until     *time.Time `json:"until,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// ServiceControl is the heal state of one service on this node.
type ServiceControl struct {
	GaveUp         bool      `json:"gaveUp,omitempty"`
	GaveUpAt       time.Time `json:"gaveUpAt,omitempty"`
	GaveUpRevision string    `json:"gaveUpRevision,omitempty"`
	GaveUpReason   string    `json:"gaveUpReason,omitempty"`
	// Actions are the times of actions within the give-up window.
	Actions       []time.Time                  `json:"actions,omitempty"`
	Containers    map[string]*ContainerControl `json:"containers,omitempty"`
	LastAction    string                       `json:"lastAction,omitempty"`
	LastActionAt  time.Time                    `json:"lastActionAt,omitempty"`
	LastContainer string                       `json:"lastContainer,omitempty"`
	LastReason    string                       `json:"lastReason,omitempty"`
}

// ContainerControl is the per-container backoff state, keyed by container ID.
type ContainerControl struct {
	Actions       int       `json:"actions"`
	LastActionAt  time.Time `json:"lastActionAt"`
	CooldownUntil time.Time `json:"cooldownUntil"`
}

// ReadControl returns the environment's control state (empty when absent).
func ReadControl(dataDir, project, environment string) (*Control, error) {
	path, err := controlPath(dataDir, project, environment)
	if err != nil {
		return nil, err
	}
	control := &Control{}
	if _, err := readJSON(path, control); err != nil {
		return &Control{SchemaVersion: SchemaVersion}, err
	}
	control.SchemaVersion = SchemaVersion
	return control, nil
}

// WriteControl persists the environment's control state.
func WriteControl(dataDir, project, environment string, control *Control) error {
	path, err := controlPath(dataDir, project, environment)
	if err != nil {
		return err
	}
	control.SchemaVersion = SchemaVersion
	return writeJSONAtomic(path, control)
}

// Service returns the service's control entry, creating it.
func (c *Control) Service(name string) *ServiceControl {
	if c.Services == nil {
		c.Services = map[string]*ServiceControl{}
	}
	service := c.Services[name]
	if service == nil {
		service = &ServiceControl{}
		c.Services[name] = service
	}
	return service
}

// Lookup returns the service's control entry or nil.
func (c *Control) Lookup(name string) *ServiceControl {
	if c == nil || c.Services == nil {
		return nil
	}
	return c.Services[name]
}

// Container returns the container's backoff state, creating it.
func (s *ServiceControl) Container(id string) *ContainerControl {
	if s.Containers == nil {
		s.Containers = map[string]*ContainerControl{}
	}
	container := s.Containers[id]
	if container == nil {
		container = &ContainerControl{}
		s.Containers[id] = container
	}
	return container
}

// ActionsWithin returns the number of actions in (now-window, now].
func (s *ServiceControl) ActionsWithin(window time.Duration, now time.Time) int {
	if s == nil {
		return 0
	}
	count := 0
	for _, at := range s.Actions {
		if now.Sub(at) < window {
			count++
		}
	}
	return count
}

// prune drops action stamps older than window.
func (s *ServiceControl) prune(window time.Duration, now time.Time) {
	kept := s.Actions[:0]
	for _, at := range s.Actions {
		if now.Sub(at) < window {
			kept = append(kept, at)
		}
	}
	s.Actions = kept
}

// PauseActive returns the active pause covering service ("" asks for the
// environment-wide pause only) and whether one exists.
func (c *Control) PauseActive(service string, now time.Time) (Pause, bool) {
	if c == nil {
		return Pause{}, false
	}
	for _, pause := range c.Pauses {
		if pause.Until != nil && !now.Before(*pause.Until) {
			continue
		}
		if pause.Service == "" || (service != "" && pause.Service == service) {
			return pause, true
		}
	}
	return Pause{}, false
}

// AddPause records a pause, replacing an earlier pause of the same target.
func (c *Control) AddPause(service string, until *time.Time, now time.Time) {
	c.PruneExpiredPauses(now)
	kept := c.Pauses[:0]
	for _, pause := range c.Pauses {
		if pause.Service != service {
			kept = append(kept, pause)
		}
	}
	var copied *time.Time
	if until != nil {
		value := until.UTC()
		copied = &value
	}
	c.Pauses = append(kept, Pause{Service: service, Until: copied, CreatedAt: now.UTC()})
	sort.Slice(c.Pauses, func(i, j int) bool { return c.Pauses[i].Service < c.Pauses[j].Service })
}

// Resume removes the pause of the target; an empty service removes every
// pause of the environment. It reports whether anything changed.
func (c *Control) Resume(service string, now time.Time) bool {
	c.PruneExpiredPauses(now)
	kept := c.Pauses[:0]
	changed := false
	for _, pause := range c.Pauses {
		if service == "" || pause.Service == service {
			changed = true
			continue
		}
		kept = append(kept, pause)
	}
	c.Pauses = kept
	return changed
}

// PruneExpiredPauses drops expired pauses.
func (c *Control) PruneExpiredPauses(now time.Time) {
	kept := c.Pauses[:0]
	for _, pause := range c.Pauses {
		if pause.Until == nil || now.Before(*pause.Until) {
			kept = append(kept, pause)
		}
	}
	c.Pauses = kept
}

// Reset clears give-up, counters and cooldowns of a service. It reports
// whether the service had given up.
func (c *Control) Reset(service string) bool {
	entry := c.Lookup(service)
	if entry == nil {
		return false
	}
	gaveUp := entry.GaveUp
	delete(c.Services, service)
	return gaveUp
}

// NodeState is the persisted node-wide breaker state and recent actions.
type NodeState struct {
	SchemaVersion    int         `json:"schemaVersion"`
	Actions          []time.Time `json:"actions,omitempty"`
	BreakerOpenedAt  time.Time   `json:"breakerOpenedAt,omitempty"`
	BreakerOpenUntil time.Time   `json:"breakerOpenUntil,omitempty"`
	BreakerReason    string      `json:"breakerReason,omitempty"`
}

// ReadNodeState returns the node state (empty when absent).
func ReadNodeState(dataDir string) (*NodeState, error) {
	path, err := nodeStatePath(dataDir)
	if err != nil {
		return nil, err
	}
	state := &NodeState{}
	if _, err := readJSON(path, state); err != nil {
		return &NodeState{SchemaVersion: SchemaVersion}, err
	}
	state.SchemaVersion = SchemaVersion
	return state, nil
}

// WriteNodeState persists the node state.
func WriteNodeState(dataDir string, state *NodeState) error {
	path, err := nodeStatePath(dataDir)
	if err != nil {
		return err
	}
	state.SchemaVersion = SchemaVersion
	return writeJSONAtomic(path, state)
}

// BreakerOpen reports whether the breaker is open at now.
func (n *NodeState) BreakerOpen(now time.Time) bool {
	return n != nil && !n.BreakerOpenUntil.IsZero() && now.Before(n.BreakerOpenUntil)
}

// ActionsWithin counts node actions in (now-window, now].
func (n *NodeState) ActionsWithin(window time.Duration, now time.Time) int {
	if n == nil {
		return 0
	}
	count := 0
	for _, at := range n.Actions {
		if now.Sub(at) < window {
			count++
		}
	}
	return count
}
