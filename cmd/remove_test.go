package cmd

import (
	"slices"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestRemoveCommandExposesServerFlag(t *testing.T) {
	if flag := removeCmd.Flags().Lookup("server"); flag == nil {
		t.Fatal("remove command should expose a server flag")
	}
}

func TestResolveRemoveTargetServersFiltersEnvironmentServers(t *testing.T) {
	got, err := resolveRemoveTargetServers("production", []string{"node-a", "node-b", "node-c"}, []string{"node-c", "node-a", "node-c"})
	if err != nil {
		t.Fatalf("resolveRemoveTargetServers returned error: %v", err)
	}
	want := []string{"node-a", "node-c"}
	if !slices.Equal(got, want) {
		t.Fatalf("targets = %#v, want %#v", got, want)
	}
}

func TestResolveRemoveTargetServersRejectsOutsideEnvironment(t *testing.T) {
	_, err := resolveRemoveTargetServers("production", []string{"node-a"}, []string{"node-b"})
	if err == nil {
		t.Fatal("resolveRemoveTargetServers should reject servers outside environment")
	}
}

func TestRemoveCleanupRequestTargetsEnvironmentState(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
	}
	services := map[string]config.ServiceConfig{
		"web": {
			Proxy: &config.ProxyConfig{Domain: "example.com"},
		},
		"db": {
			Image: "postgres:16",
		},
	}

	request := removeCleanupRequest(cfg, "production", services)
	if request.Project != "demo" {
		t.Fatalf("project = %q, want demo", request.Project)
	}
	if request.Environment != "production" {
		t.Fatalf("environment = %q, want production", request.Environment)
	}
	if !request.RemoveContainers || !request.RemoveImages || !request.RemoveNetworks || !request.RemoveDeployFiles || !request.RemoveTakodState {
		t.Fatalf("request did not enable full project cleanup: %#v", request)
	}
	for _, want := range []string{
		runtimeid.ProxyConfigFileName("demo", "production"),
		runtimeid.MaintenanceProxyConfigFileName("demo", "production", "web"),
	} {
		if !slices.Contains(request.ProxyFiles, want) {
			t.Fatalf("proxy files = %#v, want %s", request.ProxyFiles, want)
		}
	}
	if !slices.Equal(request.ImageRepositories, []string{"demo/web"}) {
		t.Fatalf("image repositories = %#v, want only Tako-owned web repository", request.ImageRepositories)
	}
}
