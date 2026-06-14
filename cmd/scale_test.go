package cmd

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestScaleTargetServersUsesSortedEnvironmentNodes(t *testing.T) {
	cfg := scaleTargetConfig()

	servers, err := scaleTargetServers(cfg, "production", "")
	if err != nil {
		t.Fatalf("scaleTargetServers returned error: %v", err)
	}
	if !slices.Equal(servers, []string{"node-a", "node-b"}) {
		t.Fatalf("servers = %#v, want node-a/node-b", servers)
	}
}

func TestScaleTargetServersHonorsServerOverride(t *testing.T) {
	cfg := scaleTargetConfig()

	servers, err := scaleTargetServers(cfg, "production", "node-b")
	if err != nil {
		t.Fatalf("scaleTargetServers returned error: %v", err)
	}
	if !slices.Equal(servers, []string{"node-b"}) {
		t.Fatalf("servers = %#v, want node-b", servers)
	}
}

func TestScaleTargetServersRejectsOutsideEnvironmentOverride(t *testing.T) {
	cfg := scaleTargetConfig()

	if _, err := scaleTargetServers(cfg, "production", "node-c"); err == nil {
		t.Fatal("scaleTargetServers should reject server outside environment")
	}
}

func TestScaleTargetSummaryIsDeterministic(t *testing.T) {
	summary := scaleTargetSummary(map[string]int{
		"worker": 3,
		"web":    2,
	})

	if summary != "web=2, worker=3" {
		t.Fatalf("summary = %q, want sorted summary", summary)
	}
}

func TestBuildScaleDeploymentStateRecordsScaledServices(t *testing.T) {
	start := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	cfg := scaleTargetConfig()
	cfg.Project = config.ProjectConfig{Name: "demo", Version: "1.2.3"}
	cfg.Runtime = &config.RuntimeConfig{Mode: config.RuntimeModeTakod}
	services := map[string]config.ServiceConfig{
		"web": {
			Port:     3000,
			Replicas: 4,
			Env:      map[string]string{"TOKEN": "secret"},
		},
	}

	deployment := buildScaleDeploymentState(
		cfg,
		"production",
		"10.0.0.1",
		start,
		2*time.Second,
		map[string]int{"web": 4},
		services,
		map[string]string{"web": "demo/web:abc123"},
	)

	if deployment.ProjectName != "demo" || deployment.Version != "1.2.3" {
		t.Fatalf("deployment project/version = %s/%s, want demo/1.2.3", deployment.ProjectName, deployment.Version)
	}
	if deployment.Status != "success" {
		t.Fatalf("status = %q, want success", deployment.Status)
	}
	if !strings.Contains(deployment.Message, "web=4") {
		t.Fatalf("message = %q, want scale target", deployment.Message)
	}
	web := deployment.Services["web"]
	if web.Image != "demo/web:abc123" || web.Replicas != 4 || web.Port != 3000 {
		t.Fatalf("web state = %#v, want image/replicas/port", web)
	}
	if web.Env["TOKEN"] != "<redacted>" {
		t.Fatalf("web env = %#v, want redacted TOKEN", web.Env)
	}
}

func scaleTargetConfig() *config.Config {
	return &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
			"node-b": {Host: "10.0.0.2"},
			"node-c": {Host: "10.0.0.3"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {Servers: []string{"node-b", "node-a"}},
		},
	}
}
