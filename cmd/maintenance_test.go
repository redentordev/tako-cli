package cmd

import (
	"strings"
	"testing"

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
