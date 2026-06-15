package cmd

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestServerConfigByNameReportsMissingServer(t *testing.T) {
	cfg := resolverConfig()

	if _, err := serverConfigByName(cfg, "missing"); err == nil {
		t.Fatal("serverConfigByName should reject missing server")
	}
}

func TestResolveEnvironmentServerSetUsesOnlyEnvironmentNodesByDefault(t *testing.T) {
	cfg := resolverConfig()

	servers, err := resolveEnvironmentServerSet(cfg, "production", "")
	if err != nil {
		t.Fatalf("resolveEnvironmentServerSet returned error: %v", err)
	}

	if _, ok := servers["node-a"]; !ok {
		t.Fatal("node-a should be included")
	}
	if _, ok := servers["node-b"]; !ok {
		t.Fatal("node-b should be included")
	}
	if _, ok := servers["node-c"]; ok {
		t.Fatal("node-c should not be included because it is outside production")
	}
}

func TestResolveEnvironmentServerSetRequiresRequestedServerInEnvironment(t *testing.T) {
	cfg := resolverConfig()

	if _, err := resolveEnvironmentServerSet(cfg, "production", "node-c"); err == nil {
		t.Fatal("resolveEnvironmentServerSet should reject a server outside the environment")
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
