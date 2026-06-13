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
