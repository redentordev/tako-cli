package takod

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/selfheal"
)

// healProbeTarget is one recorded running container of an HTTP/TCP-checked
// service with restartUnhealthy.
type healProbeTarget struct {
	ID       string
	Network  string
	Spec     *HealthSpec
	Interval time.Duration
}

// setProbeTargets replaces the probe targets (computed by each Tick) and
// drops results of containers no longer probed.
func (h *healManager) setProbeTargets(targets map[string]healProbeTarget) {
	h.probeMu.Lock()
	defer h.probeMu.Unlock()
	h.targets = targets
	for id := range h.probes {
		if _, ok := targets[id]; !ok {
			delete(h.probes, id)
			delete(h.probeNext, id)
		}
	}
}

func (h *healManager) clearProbes() {
	h.setProbeTargets(map[string]healProbeTarget{})
}

// probeTargets computes the probe targets of one scope from its engine
// input: recorded containers that are running, of services that restart
// unhealthy containers and are checked over HTTP or TCP.
func probeTargets(project, environment string, services []healService, input []selfheal.ServiceInput, targets map[string]healProbeTarget) {
	network := runtimeid.NetworkName(project, environment)
	for index, service := range services {
		if index >= len(input) || service.Health == nil || service.Spec.Policy == nil || !service.Spec.Policy.RestartUnhealthy {
			continue
		}
		if service.Spec.Health != selfheal.HealthHTTP && service.Spec.Health != selfheal.HealthTCP {
			continue
		}
		in := input[index]
		if in.Skip != "" || in.Record == nil || !in.Record.Valid(service.Spec.Image) {
			continue
		}
		for _, container := range in.Record.Containers {
			observation, ok := in.Observations[container.ID]
			if !ok || observation.Status != "running" || observation.Restarting {
				continue
			}
			targets[container.ID] = healProbeTarget{ID: container.ID, Network: network, Spec: service.Health, Interval: service.Spec.ProbeInterval}
		}
	}
}

// scheduleProbes starts every due probe; at most healMaxProbes run at once.
func (h *healManager) scheduleProbes(ctx context.Context) {
	now := h.now()
	h.probeMu.Lock()
	var due []healProbeTarget
	for id, target := range h.targets {
		if h.probing[id] || now.Before(h.probeNext[id]) {
			continue
		}
		h.probing[id] = true
		interval := target.Interval
		if interval <= 0 {
			interval = selfheal.DefaultProbeInterval
		}
		h.probeNext[id] = now.Add(interval)
		due = append(due, target)
	}
	h.probeMu.Unlock()
	for _, target := range due {
		h.probeWG.Add(1)
		go func(target healProbeTarget) {
			defer h.probeWG.Done()
			defer func() {
				h.probeMu.Lock()
				delete(h.probing, target.ID)
				h.probeMu.Unlock()
			}()
			select {
			case h.slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-h.slots }()
			result := h.probe(ctx, target.Network, target.ID, target.Spec)
			h.probeMu.Lock()
			if _, ok := h.targets[target.ID]; ok {
				h.probes[target.ID] = result
			}
			h.probeMu.Unlock()
		}(target)
	}
}

// probeContainerHealth runs one health check against one container with the
// rollout health helpers (no smoke check) and maps the outcome to a reason
// code. Error text never leaves this function.
func probeContainerHealth(ctx context.Context, network, container string, spec *HealthSpec) selfheal.ProbeResult {
	var err error
	if spec.Path != "" {
		err = checkContainerHTTPHealth(ctx, network, container, spec)
	} else {
		err = checkContainerTCPHealth(ctx, network, container, spec)
	}
	result := selfheal.ProbeResult{At: time.Now().UTC()}
	if err == nil {
		result.Healthy = true
		return result
	}
	return classifyProbeError(result, err, ctx)
}

func classifyProbeError(result selfheal.ProbeResult, err error, ctx context.Context) selfheal.ProbeResult {
	var target *healthTargetError
	var status *healthStatusError
	var netErr net.Error
	switch {
	case ctx.Err() != nil, errors.As(err, &target):
		// takod shutting down, or the container's address is unknown: the
		// probe cannot judge the container.
		result.Unknown = true
	case errors.As(err, &status):
		result.Reason, result.Code = selfheal.ReasonHTTPStatus, status.status
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		result.Reason = selfheal.ReasonHTTPTimeout
	default:
		result.Reason = selfheal.ReasonConnectFailed
	}
	return result
}
