package cmd

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestRenderMaintenanceProxyConfigUsesFileProviderRouters(t *testing.T) {
	data, err := renderMaintenanceProxyConfig(
		"demo",
		"production",
		"web",
		&config.ProxyConfig{
			Domain:       "example.com",
			RedirectFrom: []string{"www.example.com"},
		},
		"demo_web_maintenance",
	)
	if err != nil {
		t.Fatalf("renderMaintenanceProxyConfig returned error: %v", err)
	}

	configText := string(data)
	for _, expected := range []string{
		"rule: Host(`example.com`) || Host(`www.example.com`)",
		"entryPoints:",
		"- websecure",
		"priority: 100",
		"certResolver: letsencrypt",
		"url: http://demo_web_maintenance:80",
	} {
		if !strings.Contains(configText, expected) {
			t.Fatalf("maintenance proxy config missing %q:\n%s", expected, configText)
		}
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
		"demo-app-production-1-api-maintenance.yml",
		"demo-app-production-1-web-maintenance.yml",
		"demo-app-production-1.yml",
	}
	if len(files) != len(want) {
		t.Fatalf("cleanupProxyFiles returned %d files, want %d: %#v", len(files), len(want), files)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("cleanupProxyFiles[%d] = %q, want %q (all: %#v)", i, files[i], want[i], files)
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

	errors := printMaintenanceNodeResults("Testing", "done", results)
	if !slices.Equal(errors, []string{"node-a: takod unavailable", "missing: server not found in configuration"}) {
		t.Fatalf("errors = %#v", errors)
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
