package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// Mesh prune outcomes, one per candidate server.
const (
	MeshPruneOutcomePruned      = "pruned"
	MeshPruneOutcomeAbsent      = "absent"
	MeshPruneOutcomeUnsupported = "unsupported"
	MeshPruneOutcomeUnreachable = "unreachable"
	MeshPruneOutcomeLocked      = "locked"
	MeshPruneOutcomeFailed      = "failed"
)

// MeshPruneNodeOutcome reports what mesh prune did on one server.
type MeshPruneNodeOutcome struct {
	Server  string `json:"server"`
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// MeshPruneOps are the node operations of mesh prune. Status reads GET
// /v1/status (an error means the server is unreachable); Lease takes the
// environment's lease on that one server and returns its release; Delete
// removes the environment's peer set.
type MeshPruneOps struct {
	Status func(ctx context.Context, server string) (takod.Status, error)
	Lease  func(ctx context.Context, server string) (release func(), err error)
	Delete func(ctx context.Context, server string) error
}

// MeshPruneCandidates returns the servers mesh prune may touch for an
// environment: declared, non-enrolled servers that are not schedulable
// members of it. A peer set the environment left on such a server (after
// the server was removed from its servers list) is never cleaned by a
// deploy, which only contacts members, and can make another environment's
// peer union on that server conflict later. explicit narrows the
// candidates; naming a member or an enrolled server is an invalid request.
func MeshPruneCandidates(cfg *config.Config, environment string, explicit []string) ([]string, error) {
	if cfg == nil {
		return nil, invalidRequestf("mesh prune requires a loaded config")
	}
	environmentServers, err := cfg.GetEnvironmentServers(environment)
	if err != nil {
		return nil, invalidRequestf("%v", err)
	}
	members, err := config.ResolveSchedulableEnvironmentTargets(cfg.Servers, environmentServers, environment)
	if err != nil {
		return nil, invalidRequestf("%v", err)
	}
	isMember := make(map[string]bool, len(members))
	for _, name := range members {
		isMember[name] = true
	}
	enrolled := func(server config.ServerConfig) bool {
		return strings.TrimSpace(server.ClusterID) != "" && strings.TrimSpace(server.NodeID) != ""
	}
	if len(explicit) > 0 {
		out := make([]string, 0, len(explicit))
		seen := map[string]bool{}
		for _, name := range explicit {
			server, ok := cfg.Servers[name]
			switch {
			case !ok:
				return nil, invalidRequestf("server %s is not declared in servers", name)
			case isMember[name]:
				return nil, invalidRequestf("server %s is a member of environment %s; its peer set is current (use 'tako destroy' to remove the environment)", name, environment)
			case enrolled(server):
				return nil, invalidRequestf("server %s is enrolled; the platform owns its mesh", name)
			}
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		sort.Strings(out)
		return out, nil
	}
	var out []string
	for name, server := range cfg.Servers {
		if isMember[name] || enrolled(server) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// PruneMeshPeerSets deletes the environment's mesh peer set from each
// candidate server that still stores one. A server is leased before its set
// is deleted; an unreachable server is reported and skipped (warn), because
// nothing unleased may be mutated. The error is a LockedError when a lease
// was refused, or a plain error when a delete failed.
func PruneMeshPeerSets(ctx context.Context, project, environment string, servers []string, ops MeshPruneOps, warn func(string)) ([]MeshPruneNodeOutcome, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if warn == nil {
		warn = func(string) {}
	}
	outcomes := make([]MeshPruneNodeOutcome, 0, len(servers))
	var lockedErrs, failedErrs []string
	for _, server := range servers {
		if err := ctx.Err(); err != nil {
			return outcomes, err
		}
		outcome := MeshPruneNodeOutcome{Server: server}
		status, err := ops.Status(ctx, server)
		switch {
		case err != nil:
			outcome.Outcome, outcome.Error = MeshPruneOutcomeUnreachable, err.Error()
			warn(fmt.Sprintf("server %s is unreachable; skipped (rerun mesh prune once it is reachable): %v", server, err))
		case !statusHasCapability(&status, takod.CapabilityMeshPeerSetsV1):
			outcome.Outcome = MeshPruneOutcomeUnsupported
		case !statusHasPeerSet(status, project, environment):
			outcome.Outcome = MeshPruneOutcomeAbsent
		default:
			outcome = pruneMeshPeerSet(ctx, server, ops)
			if outcome.Outcome == MeshPruneOutcomeLocked {
				lockedErrs = append(lockedErrs, outcome.Error)
			} else if outcome.Outcome == MeshPruneOutcomeFailed {
				failedErrs = append(failedErrs, outcome.Error)
			}
		}
		outcomes = append(outcomes, outcome)
	}
	if len(lockedErrs) > 0 {
		return outcomes, &LockedError{Operation: "mesh-prune", Err: fmt.Errorf("mesh prune blocked: %s", strings.Join(lockedErrs, "; "))}
	}
	if len(failedErrs) > 0 {
		return outcomes, fmt.Errorf("mesh prune failed: %s", strings.Join(failedErrs, "; "))
	}
	return outcomes, nil
}

func pruneMeshPeerSet(ctx context.Context, server string, ops MeshPruneOps) MeshPruneNodeOutcome {
	outcome := MeshPruneNodeOutcome{Server: server}
	release, err := ops.Lease(ctx, server)
	if err != nil {
		outcome.Outcome = MeshPruneOutcomeFailed
		var locked *LockedError
		if errors.As(err, &locked) {
			outcome.Outcome = MeshPruneOutcomeLocked
		}
		outcome.Error = fmt.Sprintf("%s: %v", server, err)
		return outcome
	}
	if release != nil {
		defer release()
	}
	if err := ops.Delete(ctx, server); err != nil {
		outcome.Outcome, outcome.Error = MeshPruneOutcomeFailed, fmt.Sprintf("%s: %v", server, err)
		return outcome
	}
	outcome.Outcome = MeshPruneOutcomePruned
	return outcome
}

func statusHasPeerSet(status takod.Status, project, environment string) bool {
	for _, set := range status.MeshPeerSets {
		if set.Project == project && set.Environment == environment {
			return true
		}
	}
	return false
}

// SSHMeshPruneOps returns the production node operations of mesh prune.
func SSHMeshPruneOps(cfg *config.Config, pool *ssh.Pool, environment string) (MeshPruneOps, func(), error) {
	factory, err := nodeclient.NewFactory(cfg, pool, TakodSocketFromConfig(cfg))
	if err != nil {
		return MeshPruneOps{}, nil, err
	}
	socket := TakodSocketFromConfig(cfg)
	ops := MeshPruneOps{
		Status: func(ctx context.Context, server string) (takod.Status, error) {
			client, _, err := factory.Client(ctx, server)
			if err != nil {
				return takod.Status{}, err
			}
			output, err := takodclient.RequestJSONWithContext(ctx, client, socket, "GET", "/v1/status", nil)
			if err != nil {
				return takod.Status{}, err
			}
			var status takod.Status
			if err := json.Unmarshal([]byte(output), &status); err != nil {
				return takod.Status{}, fmt.Errorf("parse takod status: %w", err)
			}
			return status, nil
		},
		Lease: func(ctx context.Context, server string) (func(), error) {
			set, err := AcquireRemoteOperationLeasesContext(ctx, pool, cfg, environment, []string{server}, "mesh-prune")
			if err != nil {
				return nil, err
			}
			return set.Release, nil
		},
		Delete: func(ctx context.Context, server string) error {
			client, _, err := factory.Client(ctx, server)
			if err != nil {
				return err
			}
			return RemoveMeshPeerSetContext(ctx, client, cfg, cfg.Servers[server], environment)
		},
	}
	return ops, factory.CloseIdleConnections, nil
}
