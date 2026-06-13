package state

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestStateReplicatorTargetsAllEnvironmentMeshNodes(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
			"node-b": {Host: "10.0.0.2"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b"},
			},
		},
	}

	replicator := NewStateReplicator(nil, cfg, "production", "demo", false)
	got := replicator.getReplicaServers()

	if len(got) != 2 {
		t.Fatalf("replica servers = %d, want 2", len(got))
	}
	if _, ok := got["node-a"]; !ok {
		t.Fatal("node-a should be included")
	}
	if _, ok := got["node-b"]; !ok {
		t.Fatal("node-b should be included")
	}
}

func TestStateReplicatorSkipsSingleNodeEnvironment(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a"},
			},
		},
	}

	replicator := NewStateReplicator(nil, cfg, "production", "demo", false)
	if got := replicator.getReplicaServers(); len(got) != 0 {
		t.Fatalf("replica servers = %v, want none for single-node environment", got)
	}
}
