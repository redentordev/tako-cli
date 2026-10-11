package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/selfheal"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// Machine documents of `tako heal`.
const (
	KindHealStatus       = "HealStatus"
	KindHealChangeResult = "HealChangeResult"
)

// HealStatus statuses.
const (
	HealStatusOK          = "ok"
	HealStatusAttention   = "attention"
	HealStatusDisabled    = "disabled"
	HealStatusUnreachable = "unreachable"
	HealStatusUnsupported = "unsupported"
)

const healNodeTimeout = 15 * time.Second

// HealNode is one node's self-healing view of the environment. Every field
// is a closed vocabulary, a public name, a timestamp or a number.
type HealNode struct {
	Node        string                    `json:"node"`
	Reachable   bool                      `json:"reachable"`
	Supported   bool                      `json:"supported"`
	Error       string                    `json:"error,omitempty"`
	Enabled     bool                      `json:"enabled"`
	EvaluatedAt *time.Time                `json:"evaluatedAt,omitempty"`
	Pauses      []selfheal.Pause          `json:"pauses,omitempty"`
	Breaker     *takod.HealBreakerStatus  `json:"breaker,omitempty"`
	Services    []takod.HealServiceStatus `json:"services"`
	Journal     []selfheal.JournalEntry   `json:"journal,omitempty"`
}

// HealStatusResult is the `tako heal status` document (kind HealStatus).
type HealStatusResult struct {
	APIVersion  string     `json:"apiVersion"`
	Kind        string     `json:"kind"`
	Project     string     `json:"project"`
	Environment string     `json:"environment"`
	Service     string     `json:"service,omitempty"`
	Status      string     `json:"status"`
	Nodes       []HealNode `json:"nodes"`
	Error       string     `json:"error,omitempty"`
}

// HealChangeNode is one node's answer to a pause, resume or reset.
type HealChangeNode struct {
	Node        string     `json:"node"`
	Changed     bool       `json:"changed"`
	PausedUntil *time.Time `json:"pausedUntil,omitempty"`
	Indefinite  bool       `json:"indefinite,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// HealChangeResult is the `tako heal pause|resume|reset` document.
type HealChangeResult struct {
	APIVersion  string           `json:"apiVersion"`
	Kind        string           `json:"kind"`
	Project     string           `json:"project"`
	Environment string           `json:"environment"`
	Action      string           `json:"action"`
	Service     string           `json:"service,omitempty"`
	PausedUntil *time.Time       `json:"pausedUntil,omitempty"`
	Indefinite  bool             `json:"indefinite,omitempty"`
	Nodes       []HealChangeNode `json:"nodes"`
	Error       string           `json:"error,omitempty"`
}

// HealNodeRead is one node's raw status read.
type HealNodeRead struct {
	Node HealNode
	Err  error
}

// HealStatusReadFunc reads one node's status document.
type HealStatusReadFunc func(ctx context.Context, server string) (*takod.HealStatusResponse, error)

// ReadHealStatus reads GET /v1/heal from every server in parallel.
func ReadHealStatus(ctx context.Context, cfg *config.Config, envName string, servers []string, service string, journal int) (HealStatusResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	pool := ssh.NewPool()
	defer pool.CloseAll()
	factory, err := nodeclient.NewFactory(cfg, pool, TakodSocketFromConfig(cfg))
	if err != nil {
		return HealStatusResult{APIVersion: takoapi.APIVersionCurrent, Kind: KindHealStatus, Project: cfg.Project.Name, Environment: envName, Service: service, Nodes: []HealNode{}}, err
	}
	defer factory.CloseIdleConnections()
	socket := TakodSocketFromConfig(cfg)
	return CollectHealStatus(ctx, cfg.Project.Name, envName, servers, service, func(ctx context.Context, server string) (*takod.HealStatusResponse, error) {
		client, _, err := factory.Client(ctx, server)
		if err != nil {
			return nil, &ConnectivityError{Server: server, Err: fmt.Errorf("failed to connect to %s: %w", server, err)}
		}
		output, err := takodclient.HealStatus(ctx, client, socket, server, cfg.Project.Name, envName, journal)
		if err != nil {
			return nil, err
		}
		var response takod.HealStatusResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return nil, fmt.Errorf("invalid self-healing status from %s: %w", server, err)
		}
		return &response, nil
	})
}

// CollectHealStatus fans out read and classifies the outcome: 0 ok or
// disabled; 6 a service gave up, the breaker is open, or a node could not
// be read; 4 every node unreachable; 2 no node supports self-healing.
func CollectHealStatus(ctx context.Context, project, envName string, servers []string, service string, read HealStatusReadFunc) (HealStatusResult, error) {
	result := HealStatusResult{APIVersion: takoapi.APIVersionCurrent, Kind: KindHealStatus, Project: project, Environment: envName, Service: service, Nodes: []HealNode{}}
	servers = append([]string(nil), servers...)
	sort.Strings(servers)
	reads := make([]HealNodeRead, len(servers))
	var wg sync.WaitGroup
	for index, server := range servers {
		wg.Add(1)
		go func(index int, server string) {
			defer wg.Done()
			nodeCtx, cancel := context.WithTimeout(ctx, healNodeTimeout)
			defer cancel()
			reads[index] = healNodeRead(nodeCtx, server, service, read)
		}(index, server)
	}
	wg.Wait()
	return ClassifyHealStatus(result, reads)
}

func healNodeRead(ctx context.Context, server, service string, read HealStatusReadFunc) HealNodeRead {
	out := HealNodeRead{Node: HealNode{Node: server, Services: []takod.HealServiceStatus{}}}
	response, err := read(ctx, server)
	if err != nil {
		out.Err = err
		var connectivity *ConnectivityError
		var capability *takodclient.CapabilityRequiredError
		switch {
		case errors.As(err, &connectivity):
			out.Node.Error = "unreachable"
		case errors.As(err, &capability):
			out.Node.Reachable, out.Node.Error = true, "unsupported"
		default:
			out.Node.Reachable, out.Node.Error = true, "status_unavailable"
		}
		return out
	}
	out.Node.Reachable, out.Node.Supported = true, true
	out.Node.Enabled = response.Enabled
	out.Node.EvaluatedAt = response.EvaluatedAt
	out.Node.Pauses = response.Pauses
	breaker := response.Breaker
	out.Node.Breaker = &breaker
	out.Node.Journal = response.Journal
	for _, status := range response.Services {
		if service != "" && status.Service != service {
			continue
		}
		out.Node.Services = append(out.Node.Services, status)
	}
	if service != "" {
		var journal []selfheal.JournalEntry
		for _, entry := range out.Node.Journal {
			if entry.Service == "" || entry.Service == service {
				journal = append(journal, entry)
			}
		}
		out.Node.Journal = journal
	}
	return out
}

// ClassifyHealStatus derives the result status and error class.
func ClassifyHealStatus(result HealStatusResult, reads []HealNodeRead) (HealStatusResult, error) {
	reachable, supported, enabled := 0, 0, 0
	var firstConnectivity, firstCapability, firstFailure error
	attention := false
	for _, read := range reads {
		node := read.Node
		if node.Reachable {
			reachable++
		} else if firstConnectivity == nil {
			firstConnectivity = read.Err
		}
		if node.Supported {
			supported++
		}
		if node.Enabled {
			enabled++
		}
		var capability *takodclient.CapabilityRequiredError
		if errors.As(read.Err, &capability) && firstCapability == nil {
			firstCapability = read.Err
		}
		if read.Err != nil && firstFailure == nil {
			firstFailure = read.Err
		}
		if node.Breaker != nil && node.Breaker.Open && node.Enabled {
			attention = true
		}
		for _, service := range node.Services {
			if service.State == selfheal.StateGaveUp {
				attention = true
			}
		}
		result.Nodes = append(result.Nodes, node)
	}
	switch {
	case len(reads) > 0 && reachable == 0:
		result.Status = HealStatusUnreachable
		return result, firstConnectivity
	case len(reads) == 0:
		result.Status = HealStatusUnsupported
		return result, &InvalidRequestError{Err: fmt.Errorf("environment %s has no servers", result.Environment)}
	case supported == 0:
		result.Status = HealStatusUnsupported
		if firstCapability != nil {
			return result, firstCapability
		}
		return result, &AttentionError{Err: fmt.Errorf("self-healing status could not be read: %w", firstFailure)}
	}
	if firstFailure != nil || attention {
		result.Status = HealStatusAttention
		return result, &AttentionError{Err: fmt.Errorf("self-healing needs attention in %s/%s", result.Project, result.Environment)}
	}
	if enabled == 0 {
		result.Status = HealStatusDisabled
		return result, nil
	}
	result.Status = HealStatusOK
	return result, nil
}

// HealChangeRequest is one pause, resume or reset across nodes.
type HealChangeRequest struct {
	Action     string
	Service    string
	Until      *time.Time
	Indefinite bool
	// Operation is the lease operation (e.g. heal-pause).
	Operation string
}

// HealChangeFunc sends one node's change.
type HealChangeFunc func(ctx context.Context, server string, request takod.HealChangeRequest) (*takod.HealChangeResponse, error)

// ChangeHeal applies a pause, resume or reset on every server under the
// environment's operation leases (so it never races a deploy) and the
// operation fence on enrolled nodes.
func ChangeHeal(ctx context.Context, cfg *config.Config, envName string, servers []string, request HealChangeRequest) (HealChangeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result := newHealChangeResult(cfg.Project.Name, envName, request)
	if len(servers) == 0 {
		return result, &InvalidRequestError{Err: fmt.Errorf("environment %s has no servers", envName)}
	}
	pool := ssh.NewPool()
	defer pool.CloseAll()
	socket := TakodSocketFromConfig(cfg)
	// Capability preflight on every node before taking leases or changing
	// anything: an old agent fails the whole command with exit 2.
	preflight, err := nodeclient.NewFactory(cfg, pool, socket)
	if err != nil {
		return result, err
	}
	defer preflight.CloseIdleConnections()
	if checked, err := ApplyHealChange(ctx, cfg.Project.Name, envName, servers, request, func(ctx context.Context, server string) error {
		client, _, err := preflight.Client(ctx, server)
		if err != nil {
			return &ConnectivityError{Server: server, Err: fmt.Errorf("failed to connect to %s: %w", server, err)}
		}
		return takodclient.RequireCapability(ctx, client, socket, server, takodclient.CapabilityNodeSelfHealingV1, takodclient.HealFeature)
	}, nil); err != nil {
		return checked, err
	}
	operation := request.Operation
	if operation == "" {
		operation = "heal-" + request.Action
	}
	leaseSet, err := AcquireRemoteOperationLeasesContext(ctx, pool, cfg, envName, servers, operation)
	if err != nil {
		return result, err
	}
	defer leaseSet.Release()
	leaseCtx, cancel := leaseSet.BindContext(ctx)
	defer cancel()
	factory, err := nodeclient.NewFactory(cfg, pool, TakodSocketFromConfig(cfg))
	if err != nil {
		return result, err
	}
	defer factory.CloseIdleConnections()
	factory.SetOperationFenceSource(leaseSet)
	return ApplyHealChange(leaseCtx, cfg.Project.Name, envName, servers, request, nil, func(ctx context.Context, server string, change takod.HealChangeRequest) (*takod.HealChangeResponse, error) {
		client, _, err := factory.Client(ctx, server)
		if err != nil {
			return nil, &ConnectivityError{Server: server, Err: fmt.Errorf("failed to connect to %s: %w", server, err)}
		}
		output, err := takodclient.HealChange(ctx, client, socket, server, change)
		if err != nil {
			return nil, err
		}
		var response takod.HealChangeResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return nil, fmt.Errorf("invalid self-healing response from %s: %w", server, err)
		}
		return &response, nil
	})
}

func newHealChangeResult(project, envName string, request HealChangeRequest) HealChangeResult {
	result := HealChangeResult{
		APIVersion: takoapi.APIVersionCurrent, Kind: KindHealChangeResult, Project: project, Environment: envName,
		Action: request.Action, Service: request.Service, Nodes: []HealChangeNode{},
	}
	if request.Until != nil {
		until := request.Until.UTC()
		result.PausedUntil = &until
	}
	result.Indefinite = request.Indefinite
	return result
}

// ApplyHealChange sends the change to every node in order. check (the
// capability preflight) runs on every node before any node is changed, so a
// node without self-healing changes nothing (exit 2).
func ApplyHealChange(ctx context.Context, project, envName string, servers []string, request HealChangeRequest, check func(ctx context.Context, server string) error, send HealChangeFunc) (HealChangeResult, error) {
	result := newHealChangeResult(project, envName, request)
	servers = append([]string(nil), servers...)
	sort.Strings(servers)
	change := takod.HealChangeRequest{
		Project: project, Environment: envName, Action: request.Action, Service: request.Service,
		Until: request.Until, Indefinite: request.Indefinite,
	}
	if check != nil {
		for _, server := range servers {
			if err := check(ctx, server); err != nil {
				for _, name := range servers {
					node := HealChangeNode{Node: name}
					if name == server {
						node.Error = err.Error()
					}
					result.Nodes = append(result.Nodes, node)
				}
				return result, err
			}
		}
	}
	if send == nil {
		return result, nil
	}
	failures := 0
	var firstErr, firstInvalid error
	for _, server := range servers {
		node := HealChangeNode{Node: server}
		response, err := send(ctx, server, change)
		if err != nil {
			failures++
			node.Error = err.Error()
			if firstErr == nil {
				firstErr = err
			}
			var httpErr *takodclient.HTTPError
			if errors.As(err, &httpErr) && httpErr.Status == 400 && firstInvalid == nil {
				firstInvalid = err
			}
			result.Nodes = append(result.Nodes, node)
			continue
		}
		node.Changed, node.PausedUntil, node.Indefinite = response.Changed, response.PausedUntil, response.Indefinite
		if result.PausedUntil == nil && response.PausedUntil != nil {
			until := response.PausedUntil.UTC()
			result.PausedUntil = &until
		}
		result.Nodes = append(result.Nodes, node)
	}
	switch {
	case failures == 0:
		return result, nil
	case failures == len(servers):
		if firstInvalid != nil {
			return result, &InvalidRequestError{Err: firstInvalid}
		}
		return result, firstErr
	}
	return result, &AttentionError{Err: fmt.Errorf("self-healing %s failed on %d of %d node(s)", request.Action, failures, len(servers))}
}

// StatusServiceHeal is the additive `heal` object of a `tako ps` service row.
type StatusServiceHeal struct {
	State         string     `json:"state"`
	Skip          string     `json:"skip,omitempty"`
	PausedUntil   *time.Time `json:"pausedUntil,omitempty"`
	RecentActions int        `json:"recentActions"`
	LastAction    string     `json:"lastAction,omitempty"`
	LastActionAt  *time.Time `json:"lastActionAt,omitempty"`
	LastReason    string     `json:"lastReason,omitempty"`
}

// healStateRank orders states so the worst one across nodes wins.
var healStateRank = map[string]int{
	selfheal.StateGaveUp: 7, selfheal.StatePaused: 6, selfheal.StateBlocked: 5, selfheal.StateWatching: 4,
	selfheal.StateCooldown: 3, selfheal.StateNotConverged: 2, selfheal.StateOK: 1,
}

// AttachStatusHeal merges per-node heal service states into the ps rows.
func AttachStatusHeal(services []StatusService, nodes []HealNode) {
	for i := range services {
		var merged *StatusServiceHeal
		for _, node := range nodes {
			for _, status := range node.Services {
				if status.Service != services[i].Name {
					continue
				}
				if merged == nil {
					merged = &StatusServiceHeal{State: status.State, Skip: status.Skip, PausedUntil: status.PausedUntil, LastReason: status.LastReason}
				} else if healStateRank[status.State] > healStateRank[merged.State] {
					merged.State, merged.Skip, merged.PausedUntil = status.State, status.Skip, status.PausedUntil
				}
				merged.RecentActions += status.RecentActions
				if status.LastActionAt != nil && (merged.LastActionAt == nil || status.LastActionAt.After(*merged.LastActionAt)) {
					merged.LastAction, merged.LastActionAt, merged.LastReason = status.LastAction, status.LastActionAt, status.LastReason
				}
			}
		}
		services[i].Heal = merged
	}
}

// HealStatusNote is the ps status note for states that need an operator:
// heal:gave_up or heal:paused; empty otherwise.
func HealStatusNote(heal *StatusServiceHeal) string {
	if heal == nil {
		return ""
	}
	switch heal.State {
	case selfheal.StateGaveUp, selfheal.StatePaused:
		return "heal:" + heal.State
	}
	return ""
}
