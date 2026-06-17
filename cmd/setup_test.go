package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/setup"
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

func TestSetupVersionManifestPreservesInstallMetadataOnRefresh(t *testing.T) {
	installedAt := time.Date(2026, 6, 14, 1, 2, 3, 0, time.UTC)
	lastUpgrade := time.Date(2026, 6, 15, 4, 5, 6, 0, time.UTC)
	existing := &setup.ServerVersion{
		Version:     setup.CurrentVersion,
		InstalledAt: installedAt,
		LastUpgrade: lastUpgrade,
		Components:  map[string]string{"docker": "29.1.3"},
		Features:    []string{"docker"},
	}

	got := setupVersionManifest(existing)
	if got.Version != setup.CurrentVersion {
		t.Fatalf("manifest version = %q, want %q", got.Version, setup.CurrentVersion)
	}
	if got.TakoCLIVersion != Version {
		t.Fatalf("cli version = %q, want %q", got.TakoCLIVersion, Version)
	}
	if !got.InstalledAt.Equal(installedAt) {
		t.Fatalf("installed_at = %s, want %s", got.InstalledAt, installedAt)
	}
	if !got.LastUpgrade.Equal(lastUpgrade) {
		t.Fatalf("last_upgrade = %s, want %s", got.LastUpgrade, lastUpgrade)
	}
	if got.Components["docker"] != "29.1.3" {
		t.Fatalf("components = %#v, want docker version preserved", got.Components)
	}

	got.Components["docker"] = "changed"
	if existing.Components["docker"] != "29.1.3" {
		t.Fatalf("setupVersionManifest should not mutate existing components, got %#v", existing.Components)
	}
}

func TestRefreshCurrentSetupRegrantsDeployUserAccessBeforeTakodRestart(t *testing.T) {
	oldTakodBinary := setupTakodBinary
	setupTakodBinary = ""
	t.Cleanup(func() {
		setupTakodBinary = oldTakodBinary
	})

	cfg := &config.Config{
		Runtime: &config.RuntimeConfig{
			Agent: &config.AgentConfig{
				Socket:  "/run/custom/takod.sock",
				DataDir: "/var/lib/custom-tako",
			},
		},
	}
	prov := &recordingSetupRefresher{}

	if err := refreshCurrentSetup(prov, cfg, "node-a", "deploy", 42420); err != nil {
		t.Fatalf("refreshCurrentSetup returned error: %v", err)
	}

	want := []string{
		"firewall:42420",
		"deploy-user:deploy",
		"install-release:" + Version,
		"service:/run/custom/takod.sock:/var/lib/custom-tako:node-a",
	}
	if !slices.Equal(prov.calls, want) {
		t.Fatalf("calls = %#v, want %#v", prov.calls, want)
	}
}

func TestRefreshCurrentSetupWrapsDeployUserAccessError(t *testing.T) {
	prov := &recordingSetupRefresher{deployUserErr: errors.New("usermod failed")}

	err := refreshCurrentSetup(prov, &config.Config{}, "node-a", "deploy", 51820)
	if err == nil {
		t.Fatal("refreshCurrentSetup returned nil error")
	}
	if !strings.Contains(err.Error(), "refresh deploy user access") || !strings.Contains(err.Error(), "usermod failed") {
		t.Fatalf("error = %q, want deploy access context", err)
	}
}

type recordingSetupRefresher struct {
	calls         []string
	firewallErr   error
	deployUserErr error
	releaseErr    error
	fileErr       error
	serviceErr    error
}

func (r *recordingSetupRefresher) ConfigureFirewall(meshListenPort int) error {
	r.calls = append(r.calls, fmt.Sprintf("firewall:%d", meshListenPort))
	return r.firewallErr
}

func (r *recordingSetupRefresher) SetupDeployUser(username string) error {
	r.calls = append(r.calls, "deploy-user:"+username)
	return r.deployUserErr
}

func (r *recordingSetupRefresher) InstallTakodBinary(version string) error {
	r.calls = append(r.calls, "install-release:"+version)
	return r.releaseErr
}

func (r *recordingSetupRefresher) InstallTakodBinaryFromFile(path string) error {
	r.calls = append(r.calls, "install-file:"+path)
	return r.fileErr
}

func (r *recordingSetupRefresher) InstallTakodService(socket string, dataDir string, nodeName string) error {
	r.calls = append(r.calls, "service:"+socket+":"+dataDir+":"+nodeName)
	return r.serviceErr
}
