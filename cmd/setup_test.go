package cmd

import (
	"slices"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestSetupTargetServersUsesOnlyEnvironmentNodesByDefault(t *testing.T) {
	cfg := resolverConfig()

	names, servers, err := setupTargetServers(cfg, "production", "")
	if err != nil {
		t.Fatalf("setupTargetServers returned error: %v", err)
	}

	if !slices.Equal(names, []string{"node-a", "node-b"}) {
		t.Fatalf("server names = %#v, want production nodes", names)
	}
	if _, ok := servers["node-c"]; ok {
		t.Fatal("node-c should not be targeted because it is outside production")
	}
}

func TestSetupTargetServersRequiresRequestedServerInEnvironment(t *testing.T) {
	cfg := resolverConfig()

	if _, _, err := setupTargetServers(cfg, "production", "node-c"); err == nil {
		t.Fatal("setupTargetServers should reject a server outside the environment")
	}
}

func TestSetupMeshListenPort(t *testing.T) {
	if got := setupMeshListenPort(&config.Config{}); got != 51820 {
		t.Fatalf("default mesh listen port = %d, want 51820", got)
	}

	cfg := &config.Config{Mesh: &config.MeshConfig{ListenPort: 42420}}
	if got := setupMeshListenPort(cfg); got != 42420 {
		t.Fatalf("configured mesh listen port = %d, want 42420", got)
	}
}
