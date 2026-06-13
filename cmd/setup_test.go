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

func TestSetupNFSDecisionSkipsEnvironmentWithoutNFSVolumes(t *testing.T) {
	cfg := setupNFSDecisionConfig([]string{"data:/data"})

	shouldSetup, reason, showTip := setupNFSDecision(cfg, "production", "", 2)

	if shouldSetup {
		t.Fatal("setupNFSDecision should skip environments without NFS volumes")
	}
	if !strings.Contains(reason, "has no NFS volumes") {
		t.Fatalf("reason = %q, want no NFS volumes reason", reason)
	}
	if showTip {
		t.Fatal("single-server tip should not be shown for no-NFS-volume skip")
	}
}

func TestSetupNFSDecisionSetsUpMultiNodeEnvironmentWithNFSVolumes(t *testing.T) {
	cfg := setupNFSDecisionConfig([]string{"nfs:shared_data:/data:rw"})

	shouldSetup, reason, showTip := setupNFSDecision(cfg, "production", "", 2)

	if !shouldSetup {
		t.Fatalf("setupNFSDecision should set up NFS, reason = %q", reason)
	}
	if showTip {
		t.Fatal("single-server tip should not be shown for multi-node NFS setup")
	}
}

func TestSetupNFSDecisionSkipsSingleNodeEnvironmentWithNFSVolumes(t *testing.T) {
	cfg := setupNFSDecisionConfig([]string{"nfs:shared_data:/data:rw"})

	shouldSetup, reason, showTip := setupNFSDecision(cfg, "production", "", 1)

	if shouldSetup {
		t.Fatal("setupNFSDecision should skip single-node NFS setup")
	}
	if !strings.Contains(reason, "2+ servers") {
		t.Fatalf("reason = %q, want single-node reason", reason)
	}
	if !showTip {
		t.Fatal("single-server tip should be shown for single-node NFS volumes")
	}
}

func TestSetupNFSDecisionSkipsRequestedSingleServer(t *testing.T) {
	cfg := setupNFSDecisionConfig([]string{"nfs:shared_data:/data:rw"})

	shouldSetup, reason, showTip := setupNFSDecision(cfg, "production", "node-a", 1)

	if shouldSetup {
		t.Fatal("setupNFSDecision should skip NFS setup when --server is used")
	}
	if !strings.Contains(reason, "--server targets one node") {
		t.Fatalf("reason = %q, want requested-server reason", reason)
	}
	if showTip {
		t.Fatal("single-server tip should not be shown for requested-server skip")
	}
}

func setupNFSDecisionConfig(volumes []string) *config.Config {
	return &config.Config{
		Storage: &config.StorageConfig{
			NFS: &config.NFSConfig{Enabled: true, Server: "auto"},
		},
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
			"node-b": {Host: "10.0.0.2"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b"},
				Services: map[string]config.ServiceConfig{
					"web": {Image: "nginx:alpine", Volumes: volumes},
				},
			},
		},
	}
}
