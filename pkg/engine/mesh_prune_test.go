package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func meshPruneConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Name: "app", Version: "1.0.0"},
		Servers: map[string]config.ServerConfig{
			"a":     {Host: "203.0.113.1", User: "deploy"},
			"s":     {Host: "203.0.113.2", User: "deploy"},
			"old-1": {Host: "203.0.113.3", User: "deploy"},
			"old-2": {Host: "203.0.113.4", User: "deploy"},
			"down":  {Host: "203.0.113.5", User: "deploy"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"preview": {Servers: []string{"a", "s"}},
		},
	}
}

func TestMeshPruneCandidatesAreServersOutsideTheEnvironment(t *testing.T) {
	cfg := meshPruneConfig()
	got, err := MeshPruneCandidates(cfg, "preview", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"down", "old-1", "old-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	if _, err := MeshPruneCandidates(cfg, "preview", []string{"s"}); Classify(err) != ClassInvalid {
		t.Fatalf("pruning a member err = %v, want invalid request", err)
	}
	if got, err := MeshPruneCandidates(cfg, "preview", []string{"old-1"}); err != nil || !reflect.DeepEqual(got, []string{"old-1"}) {
		t.Fatalf("explicit candidates = %v, %v", got, err)
	}
}

// Review finding 4: a server removed from preview keeps preview's peer set
// until mesh prune deletes it. Prune leases each server before deleting,
// skips unreachable servers with a warning, and never touches servers
// without the set.
func TestPruneMeshPeerSetsLeasesBeforeDeletingAndSkipsUnreachable(t *testing.T) {
	withSet := takod.Status{Runtime: "takod", Capabilities: []string{takod.CapabilityMeshPeerSetsV1}, MeshPeerSets: []takod.MeshPeerSetSummary{{Project: "app", Environment: "preview"}, {Project: "app", Environment: "production"}}}
	withoutSet := takod.Status{Runtime: "takod", Capabilities: []string{takod.CapabilityMeshPeerSetsV1}, MeshPeerSets: []takod.MeshPeerSetSummary{{Project: "app", Environment: "production"}}}
	statuses := map[string]takod.Status{"old-1": withSet, "old-2": withoutSet}
	var calls []string
	leased := map[string]bool{}
	ops := MeshPruneOps{
		Status: func(_ context.Context, server string) (takod.Status, error) {
			status, ok := statuses[server]
			if !ok {
				return takod.Status{}, errors.New("dial tcp: connection refused")
			}
			return status, nil
		},
		Lease: func(_ context.Context, server string) (func(), error) {
			calls = append(calls, "lease "+server)
			leased[server] = true
			return func() { calls = append(calls, "release "+server); leased[server] = false }, nil
		},
		Delete: func(_ context.Context, server string) error {
			if !leased[server] {
				t.Fatalf("deleted the peer set on unleased %s", server)
			}
			calls = append(calls, "delete "+server)
			return nil
		},
	}
	var warnings []string
	outcomes, err := PruneMeshPeerSets(context.Background(), "app", "preview", []string{"down", "old-1", "old-2"}, ops, func(message string) { warnings = append(warnings, message) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lease old-1", "delete old-1", "release old-1"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	got := map[string]string{}
	for _, outcome := range outcomes {
		got[outcome.Server] = outcome.Outcome
	}
	if want := map[string]string{"down": MeshPruneOutcomeUnreachable, "old-1": MeshPruneOutcomePruned, "old-2": MeshPruneOutcomeAbsent}; !reflect.DeepEqual(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "server down is unreachable") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestPruneMeshPeerSetsRefusedLeaseIsLocked(t *testing.T) {
	status := takod.Status{Runtime: "takod", Capabilities: []string{takod.CapabilityMeshPeerSetsV1}, MeshPeerSets: []takod.MeshPeerSetSummary{{Project: "app", Environment: "preview"}}}
	ops := MeshPruneOps{
		Status: func(context.Context, string) (takod.Status, error) { return status, nil },
		Lease: func(context.Context, string) (func(), error) {
			return nil, &LockedError{Operation: "mesh-prune", Err: errors.New("lease held by deploy")}
		},
		Delete: func(context.Context, string) error {
			t.Fatal("deleted without a lease")
			return nil
		},
	}
	outcomes, err := PruneMeshPeerSets(context.Background(), "app", "preview", []string{"old-1"}, ops, nil)
	if Classify(err) != ClassLocked || len(outcomes) != 1 || outcomes[0].Outcome != MeshPruneOutcomeLocked {
		t.Fatalf("outcomes = %+v, err = %v", outcomes, err)
	}
}
