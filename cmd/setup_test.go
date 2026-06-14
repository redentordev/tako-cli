package cmd

import (
	"errors"
	"slices"
	"strings"
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

func TestSetupRegistersTakodBinaryFlag(t *testing.T) {
	flag := setupCmd.Flags().Lookup("takod-binary")
	if flag == nil {
		t.Fatal("setup command should expose --takod-binary")
	}
	if !strings.Contains(flag.Usage, "development/testing") {
		t.Fatalf("takod-binary flag usage = %q, want development/testing context", flag.Usage)
	}
}

func TestSetupVersionWriteErrorFailsSuccessfulProvisioning(t *testing.T) {
	err := setupVersionWriteError("node-a", errors.New("permission denied"))
	if err == nil {
		t.Fatal("setupVersionWriteError returned nil")
	}
	for _, want := range []string{"node-a", "setup completed", "failed to write setup version metadata", "permission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}
