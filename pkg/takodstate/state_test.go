package takodstate

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestBuildDesiredRevisionSanitizesServiceState(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
	}
	services := map[string]config.ServiceConfig{
		"web": {
			Build:    ".",
			Port:     3000,
			Replicas: 0,
			Env: map[string]string{
				"DATABASE_URL": "postgres://user:password@example/db",
				"TOKEN":        "top-secret-token",
			},
			EnvFile:   ".env.production",
			Secrets:   []string{"TOKEN:prod/token", "DATABASE_URL"},
			Volumes:   []string{"data:/data"},
			Proxy:     &config.ProxyConfig{Domain: "example.com"},
			Deploy:    config.DeployConfig{Strategy: "recreate"},
			DependsOn: []string{"db"},
		},
	}

	revision, err := BuildDesiredRevision(
		cfg,
		"production",
		"deploy",
		services,
		map[string]string{"web": "registry.example.com/demo/web:abc123"},
		[]string{"node-b", "node-a"},
		GitInfo{CommitShort: "abc1234"},
	)
	if err != nil {
		t.Fatalf("BuildDesiredRevision returned error: %v", err)
	}

	service := revision.Services["web"]
	if service.Replicas != 0 {
		t.Fatalf("expected scale-to-zero to be persisted as 0 replicas, got %d", service.Replicas)
	}
	if !slices.Equal(service.EnvKeys, []string{"DATABASE_URL", "TOKEN"}) {
		t.Fatalf("unexpected env keys: %#v", service.EnvKeys)
	}
	if !slices.Equal(service.SecretRefs, []string{"DATABASE_URL", "TOKEN:prod/token"}) {
		t.Fatalf("unexpected secret refs: %#v", service.SecretRefs)
	}
	if !service.EnvFile {
		t.Fatal("expected env file presence to be recorded")
	}
	if !slices.Equal(revision.TargetNodes, []string{"node-a", "node-b"}) {
		t.Fatalf("target nodes were not sorted: %#v", revision.TargetNodes)
	}

	data, err := json.Marshal(revision)
	if err != nil {
		t.Fatalf("failed to marshal revision: %v", err)
	}
	serialized := string(data)
	for _, secretValue := range []string{"postgres://user:password@example/db", "top-secret-token"} {
		if strings.Contains(serialized, secretValue) {
			t.Fatalf("desired revision leaked raw env value %q: %s", secretValue, serialized)
		}
	}
}

func TestBuildDesiredRevisionNormalizesNegativeReplicaCount(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
	}
	services := map[string]config.ServiceConfig{
		"worker": {
			Image:    "busybox:latest",
			Replicas: -2,
		},
	}

	revision, err := BuildDesiredRevision(cfg, "production", "deploy", services, nil, nil, GitInfo{})
	if err != nil {
		t.Fatalf("BuildDesiredRevision returned error: %v", err)
	}

	if got := revision.Services["worker"].Replicas; got != 1 {
		t.Fatalf("expected negative replicas to normalize to 1, got %d", got)
	}
}
