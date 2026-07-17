package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestRenderMaintenanceProxyConfigUsesRouteManifest(t *testing.T) {
	data, err := renderMaintenanceProxyConfig(
		"demo",
		"production",
		"web",
		&config.ProxyConfig{
			Domain:       "example.com",
			RedirectFrom: []string{"www.example.com"},
		},
		runtimeid.ContainerAlias("demo", "production", "web-maintenance", 1),
	)
	if err != nil {
		t.Fatalf("renderMaintenanceProxyConfig returned error: %v", err)
	}

	var manifest takod.ProxyRouteManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("failed to parse maintenance proxy route manifest: %v\n%s", err, string(data))
	}
	if manifest.Project != "demo" || manifest.Environment != "production" {
		t.Fatalf("manifest identity = %s/%s, want demo/production", manifest.Project, manifest.Environment)
	}
	if len(manifest.Routes) != 1 {
		t.Fatalf("routes = %#v, want one", manifest.Routes)
	}
	route := manifest.Routes[0]
	if route.Service != "web-maintenance" {
		t.Fatalf("service = %q, want web-maintenance", route.Service)
	}
	if !slices.Equal(route.Domains, []string{"example.com", "www.example.com"}) {
		t.Fatalf("domains = %#v, want example.com/www.example.com", route.Domains)
	}
	wantUpstream := "http://" + runtimeid.ContainerAlias("demo", "production", "web-maintenance", 1) + ":80"
	if !slices.Equal(route.Upstreams, []string{wantUpstream}) {
		t.Fatalf("upstreams = %#v, want maintenance upstream", route.Upstreams)
	}
	if manifest.Version != 2 || len(route.Destinations) != 1 || route.Destinations[0].URL != wantUpstream {
		t.Fatalf("maintenance route lacks v2 destination proof: %#v", manifest)
	}
	if _, err := takod.ParseProxyRouteManifest(string(data)); err != nil {
		t.Fatalf("maintenance route manifest does not validate: %v", err)
	}
	if route.Priority != 100 {
		t.Fatalf("priority = %d, want 100", route.Priority)
	}
}

func TestRenderMaintenanceProxyConfigRejectsUnsafeDomain(t *testing.T) {
	_, err := renderMaintenanceProxyConfig(
		"demo",
		"production",
		"web",
		&config.ProxyConfig{Domain: "example.com`) || PathPrefix(`/"},
		"demo_web_maintenance",
	)
	if err == nil {
		t.Fatal("renderMaintenanceProxyConfig should reject unsafe domains")
	}
	if !strings.Contains(err.Error(), "invalid domain") {
		t.Fatalf("error = %q, want invalid domain", err)
	}
}

func TestCleanupProxyFilesIncludesRuntimeAndMaintenanceOverrides(t *testing.T) {
	files := cleanupProxyFiles("demo-app", "production_1", map[string]config.ServiceConfig{
		"api": {
			Proxy: &config.ProxyConfig{Domain: "api.example.com"},
		},
		"worker": {},
		"web": {
			Proxy: &config.ProxyConfig{Domain: "example.com"},
		},
	})

	want := []string{
		runtimeid.MaintenanceProxyConfigFileName("demo-app", "production_1", "api"),
		runtimeid.MaintenanceProxyConfigFileName("demo-app", "production_1", "web"),
		runtimeid.ProxyConfigFileName("demo-app", "production_1"),
	}
	slices.Sort(want)
	if len(files) != len(want) {
		t.Fatalf("cleanupProxyFiles returned %d files, want %d: %#v", len(files), len(want), files)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("cleanupProxyFiles[%d] = %q, want %q (all: %#v)", i, files[i], want[i], files)
		}
	}
}

func TestMaintenanceRuntimeNamesUseSharedRuntimeIDHelpers(t *testing.T) {
	project := "demo-app"
	environment := "prod_api"
	service := "web"

	if got, want := maintenanceNetworkName(project, environment), runtimeid.NetworkName(project, environment); got != want {
		t.Fatalf("maintenance network = %q, want %q", got, want)
	}
	if got, want := maintenanceContainerName(project, environment, service), runtimeid.ContainerName(project, environment, maintenanceTakodServiceName(service), 1); got != want {
		t.Fatalf("maintenance container = %q, want %q", got, want)
	}
	if strings.Contains(maintenanceContainerName(project, environment, service), project+"_"+service+"_maintenance") {
		t.Fatalf("maintenance container name should not use raw project/service formatting")
	}
}

func TestCleanupImageRepositoriesIncludesOnlyTakoOwnedImages(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo", Version: "v1"}}
	repositories := cleanupImageRepositories(cfg, "production", map[string]config.ServiceConfig{
		"api":    {Build: "./api"},
		"db":     {Image: "postgres:16"},
		"worker": {},
	})

	want := []string{"demo/api", "demo/worker"}
	if !slices.Equal(repositories, want) {
		t.Fatalf("repositories = %#v, want %#v", repositories, want)
	}
}

func TestImageRepositoryFromRefStripsTagsAndDigests(t *testing.T) {
	tests := map[string]string{
		"demo/web:v1":                                 "demo/web",
		"localhost:5000/demo/web:v1":                  "localhost:5000/demo/web",
		"registry.example.com/demo/web@sha256:abcdef": "registry.example.com/demo/web",
		"registry.example.com:5000/demo/web:v1-env":   "registry.example.com:5000/demo/web",
		"registry.example.com:5000/demo/web":          "registry.example.com:5000/demo/web",
		"  registry.example.com/demo/web:latest  ":    "registry.example.com/demo/web",
	}
	for ref, want := range tests {
		if got := imageRepositoryFromRef(ref); got != want {
			t.Fatalf("imageRepositoryFromRef(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestRunMaintenanceNodeActionsRunsConcurrentlyAndKeepsOrder(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testMaintenanceServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	resultsDone := make(chan []maintenanceNodeResult, 1)
	go func() {
		resultsDone <- runMaintenanceNodeActions(servers, serverNames, func(serverName string, _ config.ServerConfig) error {
			started <- serverName
			<-release
			return nil
		})
	}()

	waitForMaintenanceStarts(t, started, len(serverNames))
	close(release)

	results := <-resultsDone
	if len(results) != len(serverNames) {
		t.Fatalf("results = %d, want %d", len(results), len(serverNames))
	}
	for i, serverName := range serverNames {
		if results[i].serverName != serverName {
			t.Fatalf("result %d server = %q, want %q", i, results[i].serverName, serverName)
		}
		if results[i].err != nil {
			t.Fatalf("result %d err = %v, want nil", i, results[i].err)
		}
	}
}

func TestRunMaintenanceNodeActionsRecordsErrors(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "missing"}
	servers := testMaintenanceServers(serverNames[:2])

	results := runMaintenanceNodeActions(servers, serverNames, func(serverName string, _ config.ServerConfig) error {
		if serverName == "node-a" {
			return fmt.Errorf("takod unavailable")
		}
		return nil
	})

	if results[0].err == nil {
		t.Fatalf("node-a should record action error")
	}
	if results[1].err != nil {
		t.Fatalf("node-b should succeed, got %v", results[1].err)
	}
	if results[2].err == nil {
		t.Fatalf("missing node should record configuration error")
	}

	errors := printMaintenanceNodeResults(os.Stdout, "Testing", "done", results)
	if !slices.Equal(errors, []string{"node-a: takod unavailable", "missing: server not found in configuration"}) {
		t.Fatalf("errors = %#v", errors)
	}
}

func TestMaintenanceCommandsDoNotExposeServerFlag(t *testing.T) {
	if flag := maintenanceCmd.Flags().Lookup("server"); flag != nil {
		t.Fatal("maintenance command should not expose a server flag")
	}
	if flag := liveCmd.Flags().Lookup("server"); flag != nil {
		t.Fatal("live command should not expose a server flag")
	}
}

func TestRunMaintenanceWithClientUsesProvidedPool(t *testing.T) {
	provider := &fakeSSHClientProvider{}
	server := config.ServerConfig{
		Host:     "node-a.example.test",
		Port:     2222,
		User:     "deploy",
		SSHKey:   "/tmp/id_ed25519",
		Password: "fallback",
	}
	called := false

	err := runMaintenanceWithClient(provider, server, func(client *ssh.Client) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("runMaintenanceWithClient returned error: %v", err)
	}
	if !called {
		t.Fatal("executor was not called")
	}
	if len(provider.requests) != 1 {
		t.Fatalf("pool requests = %#v, want one", provider.requests)
	}
	got := provider.requests[0]
	if got.host != server.Host || got.port != server.Port || got.user != server.User || got.sshKey != server.SSHKey || got.password != server.Password {
		t.Fatalf("pool request = %#v, want server config", got)
	}
}

func TestRunMaintenanceWithClientReturnsPoolConnectionError(t *testing.T) {
	provider := &fakeSSHClientProvider{err: fmt.Errorf("dial failed")}
	called := false

	err := runMaintenanceWithClient(provider, config.ServerConfig{Host: "node-a.example.test"}, func(client *ssh.Client) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("runMaintenanceWithClient returned nil, want connection error")
	}
	if called {
		t.Fatal("executor should not run after pool connection error")
	}
	if got := err.Error(); got != "failed to connect to server: dial failed" {
		t.Fatalf("error = %q", got)
	}
}

func waitForMaintenanceStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for maintenance fanout; saw %v", seen)
		}
	}
}

func testMaintenanceServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name + ".example.test", User: "root"}
	}
	return servers
}
