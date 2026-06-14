package deployer

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestRenderTakodProxyDynamicConfigUsesMeshUpstreams(t *testing.T) {
	deploy := testProxyDeployer()
	services := deploy.config.Environments["production"].Services

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfig(services)
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfig returned error: %v", err)
	}
	if !hasPublic {
		t.Fatal("expected public services to be detected")
	}

	configText := string(data)
	for _, expected := range []string{
		"rule: Host(`example.com`)",
		"entryPoints:",
		"certResolver: letsencrypt",
		"url: http://10.210.0.1:21001",
		"url: http://10.210.0.2:21002",
		"path: /health",
		"interval: 15s",
	} {
		if !strings.Contains(configText, expected) {
			t.Fatalf("dynamic config missing %q:\n%s", expected, configText)
		}
	}
}

func TestRenderTakodProxyDynamicConfigSkipsScaleToZero(t *testing.T) {
	deploy := testProxyDeployer()
	services := deploy.config.Environments["production"].Services
	web := services["web"]
	web.Replicas = 0
	services["web"] = web

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfig(services)
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfig returned error: %v", err)
	}
	if hasPublic {
		t.Fatalf("expected no active public routes for scale-to-zero, got:\n%s", string(data))
	}
}

func TestRenderTakodProxyDynamicConfigUsesServiceHealthCheckFallback(t *testing.T) {
	deploy := testProxyDeployer()
	services := deploy.config.Environments["production"].Services
	web := services["web"]
	web.LoadBalancer.HealthCheck = config.LoadBalancerHealthCheck{}
	web.HealthCheck = config.HealthCheckConfig{Path: "/ready", Interval: "20s"}
	services["web"] = web

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfig(services)
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfig returned error: %v", err)
	}
	if !hasPublic {
		t.Fatal("expected public services to be detected")
	}

	configText := string(data)
	for _, expected := range []string{
		"path: /ready",
		"interval: 20s",
	} {
		if !strings.Contains(configText, expected) {
			t.Fatalf("dynamic config missing %q:\n%s", expected, configText)
		}
	}
}

func TestMeshUpstreamPortRejectsSlotRangeCollision(t *testing.T) {
	deploy := testProxyDeployer()
	if _, err := deploy.meshUpstreamPort("web", meshUpstreamPortStep); err == nil {
		t.Fatal("expected slot at per-service range boundary to be rejected")
	}
}

func TestProxyHostRuleNormalizesAndPreservesWildcard(t *testing.T) {
	rule, err := proxyHostRule(&config.ProxyConfig{Domain: " *.example.com "})
	if err != nil {
		t.Fatalf("proxyHostRule returned error: %v", err)
	}
	if rule != "Host(`*.example.com`)" {
		t.Fatalf("rule = %q, want wildcard host rule", rule)
	}
}

func TestProxyHostRuleRejectsRuleInjection(t *testing.T) {
	_, err := proxyHostRule(&config.ProxyConfig{Domain: "example.com`) || PathPrefix(`/"})
	if err == nil {
		t.Fatal("proxyHostRule should reject rule injection characters")
	}
	if !strings.Contains(err.Error(), "invalid domain") {
		t.Fatalf("error = %q, want invalid domain", err)
	}
}

func testProxyDeployer() *Deployer {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1.0.0"},
		Mesh: &config.MeshConfig{
			Enabled:      testBoolPointer(true),
			NetworkCIDR:  "10.210.0.0/16",
			Interface:    "tako",
			ListenPort:   51820,
			SubnetBits:   24,
			NATTraversal: true,
		},
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "203.0.113.10", User: "root", Port: 22},
			"node-b": {Host: "203.0.113.11", User: "root", Port: 22},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b"},
				Services: map[string]config.ServiceConfig{
					"api": {
						Port:     4000,
						Replicas: 1,
					},
					"web": {
						Port:     3000,
						Replicas: 2,
						Proxy:    &config.ProxyConfig{Domain: "example.com"},
						LoadBalancer: config.LoadBalancerConfig{
							HealthCheck: config.LoadBalancerHealthCheck{
								Enabled:  true,
								Path:     "/health",
								Interval: "15s",
							},
						},
					},
				},
			},
		},
	}
	return &Deployer{config: cfg, environment: "production"}
}
