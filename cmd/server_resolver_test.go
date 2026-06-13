package cmd

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestResolveServerUsesFirstEnvironmentNodeByDefault(t *testing.T) {
	cfg := resolverConfig()

	name, server, err := resolveServer(cfg, "production", "")
	if err != nil {
		t.Fatalf("resolveServer returned error: %v", err)
	}
	if name != "node-a" {
		t.Fatalf("server name = %q, want node-a", name)
	}
	if server.Host != "10.0.0.1" {
		t.Fatalf("server host = %q, want 10.0.0.1", server.Host)
	}
}

func TestResolveServerRequiresRequestedServerInEnvironment(t *testing.T) {
	cfg := resolverConfig()

	if _, _, err := resolveServer(cfg, "production", "node-c"); err == nil {
		t.Fatal("resolveServer should reject a server outside the environment")
	}
}

func resolverConfig() *config.Config {
	return &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
			"node-b": {Host: "10.0.0.2"},
			"node-c": {Host: "10.0.0.3"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b"},
			},
		},
	}
}
