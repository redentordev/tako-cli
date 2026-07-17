package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
)

func TestStateReplicatorTargetsAllEnvironmentMeshNodes(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
			"node-b": {Host: "10.0.0.2"},
			"ready":  {Host: "10.0.0.3", Lifecycle: "ready"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b", "ready"},
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
	if _, ok := got["ready"]; ok {
		t.Fatal("ready connectivity-only node received state replication mutation")
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

func TestStateReplicatorConvertsInternalDeploymentToPublicDocuments(t *testing.T) {
	lastCheck := time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC)
	deployment := &DeploymentState{
		ID:          "dep-1",
		ProjectName: "demo",
		Environment: "production",
		Status:      StatusSuccess,
		Services: map[string]ServiceState{
			"web": {
				Name:     "web",
				Image:    "example/web:latest",
				Replicas: 2,
				HealthCheck: HealthCheckState{
					Enabled:   true,
					Path:      "/healthz",
					Healthy:   true,
					LastCheck: lastCheck,
				},
			},
		},
	}
	history := &DeploymentHistory{
		ProjectName: "demo",
		Environment: "production",
		Deployments: []*DeploymentState{deployment},
		LastUpdated: lastCheck,
	}

	deploymentDocument, err := deploymentStateDocument(deployment)
	if err != nil {
		t.Fatalf("deploymentStateDocument returned error: %v", err)
	}
	historyDocument, err := deploymentHistoryDocument(history)
	if err != nil {
		t.Fatalf("deploymentHistoryDocument returned error: %v", err)
	}

	if deploymentDocument.ID != "dep-1" || deploymentDocument.Status != takoapi.StatusSuccess || deploymentDocument.Services["web"].HealthCheck.LastCheck != lastCheck {
		t.Fatalf("deployment document = %#v", deploymentDocument)
	}
	if historyDocument.ProjectName != "demo" || len(historyDocument.Deployments) != 1 || historyDocument.Deployments[0].ID != "dep-1" {
		t.Fatalf("history document = %#v", historyDocument)
	}

	deploymentDocument.ID = "changed"
	historyDocument.Deployments[0].ID = "changed"
	if deployment.ID != "dep-1" || history.Deployments[0].ID != "dep-1" {
		t.Fatalf("conversion should clone without mutating internal input")
	}
}

func TestStateReplicatorPreparesPublicDocumentsLikeLegacyManager(t *testing.T) {
	replicator := NewStateReplicator(nil, &config.Config{}, "production", "demo", false)
	deployment := &takoapi.DeploymentStateDocument{ID: "dep-1", ProjectName: "other", Environment: "staging", Host: "old"}
	history := &takoapi.DeploymentHistoryDocument{ProjectName: "other", Environment: "staging", Server: "old"}

	replicator.prepareReplicaDocuments("10.0.0.2", deployment, history)

	if deployment.ID != "dep-1" || deployment.ProjectName != "demo" || deployment.Environment != "production" || deployment.Host != "10.0.0.2" {
		t.Fatalf("deployment document = %#v", deployment)
	}
	if history.ProjectName != "demo" || history.Environment != "production" || history.Server != "10.0.0.2" || history.LastUpdated.IsZero() {
		t.Fatalf("history document = %#v", history)
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
