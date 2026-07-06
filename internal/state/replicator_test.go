package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	if err := replicator.ReplicateDeployment(nil, nil); err != nil {
		t.Fatalf("ReplicateDeployment single-node error = %v, want nil", err)
	}
}

func TestStateReplicatorReturnsReplicaFailures(t *testing.T) {
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
	replicator.replicateNode = func(ctx context.Context, serverName string, server config.ServerConfig, deployment *DeploymentState, history *DeploymentHistory) error {
		if deployment.ID != "dep-1" {
			return fmt.Errorf("deployment ID = %q, want dep-1", deployment.ID)
		}
		if history.Environment != "production" {
			return fmt.Errorf("history environment = %q, want production", history.Environment)
		}
		if serverName == "node-b" {
			return errors.New("disk full")
		}
		return nil
	}

	err := replicator.ReplicateDeployment(
		&DeploymentState{ID: "dep-1"},
		&DeploymentHistory{Environment: "production"},
	)
	if err == nil {
		t.Fatal("ReplicateDeployment returned nil, want error")
	}
	for _, want := range []string{"state replication failed", "node-b", "disk full"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestStateReplicatorContextCancellation(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())
	replicator := NewStateReplicator(nil, cfg, "production", "demo", false)
	replicator.replicateNode = func(ctx context.Context, serverName string, server config.ServerConfig, deployment *DeploymentState, history *DeploymentHistory) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}

	err := replicator.ReplicateDeploymentContext(ctx, &DeploymentState{ID: "dep-1"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
