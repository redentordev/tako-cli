package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/health"
)

func TestDomainExpectedTargetsUseEnvironmentProxyPlacement(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"app":  {Host: "app.example.com"},
			"edge": {Host: "edge.example.com"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"app", "edge"},
				Proxy: &config.EnvironmentProxyConfig{
					Placement: &config.PlacementConfig{
						Strategy: "pinned",
						Servers:  []string{"edge"},
					},
				},
			},
		},
	}

	targets, err := domainExpectedTargets(cfg, "production", nil)
	if err != nil {
		t.Fatalf("domainExpectedTargets returned error: %v", err)
	}
	if got := strings.Join(targets, ","); got != "edge.example.com" {
		t.Fatalf("targets = %q, want edge.example.com", got)
	}
}

func TestDomainExpectedTargetsAllowOverrides(t *testing.T) {
	cfg := &config.Config{}

	targets, err := domainExpectedTargets(cfg, "production", []string{"sites.example.com", "203.0.113.10"})
	if err != nil {
		t.Fatalf("domainExpectedTargets returned error: %v", err)
	}
	if got := strings.Join(targets, ","); got != "sites.example.com,203.0.113.10" {
		t.Fatalf("targets = %q", got)
	}
}

func TestCollectConfiguredDomainSpecsIncludesRedirectDomains(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web": {
			Proxy: &config.ProxyConfig{
				Domain:       "app.example.com",
				RedirectFrom: []string{"www.example.com"},
			},
		},
	}

	specs := collectConfiguredDomainSpecs(services, "")
	if len(specs) != 2 {
		t.Fatalf("specs = %#v, want 2 entries", specs)
	}
	if specs[0].Domain != "app.example.com" || specs[0].Role != "serving" {
		t.Fatalf("serving spec = %#v", specs[0])
	}
	if specs[1].Domain != "www.example.com" || specs[1].Role != "redirect" {
		t.Fatalf("redirect spec = %#v", specs[1])
	}
}

func TestDomainStatusStrictErrorOnlyFailsPending(t *testing.T) {
	active := []health.DomainStatus{{Domain: "app.example.com", State: health.DomainStateActive}}
	if err := domainStatusStrictError(active, true); err != nil {
		t.Fatalf("active strict status returned error: %v", err)
	}

	pending := []health.DomainStatus{{Domain: "app.example.com", State: health.DomainStatePendingDNS}}
	err := domainStatusStrictError(pending, true)
	if err == nil {
		t.Fatal("pending strict status returned nil")
	}
	if !strings.Contains(err.Error(), "app.example.com=pending_dns") {
		t.Fatalf("error = %q", err)
	}
}
