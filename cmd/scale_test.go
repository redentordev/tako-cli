package cmd

import (
	"slices"
	"testing"

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
