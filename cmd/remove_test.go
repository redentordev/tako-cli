package cmd

import (
	"slices"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestRemoveCommandDoesNotExposeServerFlag(t *testing.T) {
	if flag := removeCmd.Flags().Lookup("server"); flag != nil {
		t.Fatal("remove command should not expose a server flag")
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
	if !slices.Contains(request.ProxyFiles, "demo-production.yml") {
		t.Fatalf("proxy files = %#v, want demo-production.yml", request.ProxyFiles)
	}
	if !slices.Equal(request.ImageRepositories, []string{"demo/web"}) {
		t.Fatalf("image repositories = %#v, want only Tako-owned web repository", request.ImageRepositories)
	}
}
