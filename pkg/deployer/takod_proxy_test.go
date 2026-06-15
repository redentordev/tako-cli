package deployer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestRenderTakodProxyDynamicConfigUsesLocalAndMeshUpstreams(t *testing.T) {
	deploy := testProxyDeployer()
	services := deploy.config.Environments["production"].Services

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfigForNode(services, "node-a")
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNode returned error: %v", err)
	}
	if !hasPublic {
		t.Fatal("expected public services to be detected")
	}

	configText := string(data)
	remotePort, err := deploy.meshUpstreamPort("web", 2, 3000)
	if err != nil {
		t.Fatalf("meshUpstreamPort returned error: %v", err)
	}
	for _, expected := range []string{
		"rule: Host(`example.com`)",
		"entryPoints:",
		"certResolver: letsencrypt",
		"url: http://" + runtimeid.ContainerNetworkAlias("demo", "production", "web", 1) + ":3000",
		fmt.Sprintf("url: http://10.210.0.2:%d", remotePort),
		"path: /health",
		"interval: 15s",
	} {
		if !strings.Contains(configText, expected) {
			t.Fatalf("dynamic config missing %q:\n%s", expected, configText)
		}
		unsafeName := runtimeid.ContainerName("demo", "production", "web", 1)
		if strings.Contains(configText, unsafeName) {
			t.Fatalf("local proxy upstream should use DNS-safe network alias, not container name %q:\n%s", unsafeName, configText)
		}
	}
}

func TestRenderTakodProxyDynamicConfigUsesLocalUpstreamForCurrentNode(t *testing.T) {
	deploy := testProxyDeployer()
	services := deploy.config.Environments["production"].Services

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfigForNode(services, "node-b")
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNode returned error: %v", err)
	}
	if !hasPublic {
		t.Fatal("expected public services to be detected")
	}

	configText := string(data)
	remotePort, err := deploy.meshUpstreamPort("web", 1, 3000)
	if err != nil {
		t.Fatalf("meshUpstreamPort returned error: %v", err)
	}
	for _, expected := range []string{
		fmt.Sprintf("url: http://10.210.0.1:%d", remotePort),
		"url: http://" + runtimeid.ContainerNetworkAlias("demo", "production", "web", 2) + ":3000",
	} {
		if !strings.Contains(configText, expected) {
			t.Fatalf("dynamic config missing %q:\n%s", expected, configText)
		}
	}
	unsafeName := runtimeid.ContainerName("demo", "production", "web", 2)
	if strings.Contains(configText, unsafeName) {
		t.Fatalf("local proxy upstream should use DNS-safe network alias, not container name %q:\n%s", unsafeName, configText)
	}
}

func TestRenderTakodProxyDynamicConfigUsesOnlyLocalUpstreamForOneNode(t *testing.T) {
	deploy := testProxyDeployer()
	deploy.config.Environments["production"] = config.EnvironmentConfig{
		Servers: []string{"node-a"},
		Services: map[string]config.ServiceConfig{
			"web": {
				Port:     3000,
				Replicas: 1,
				Proxy:    &config.ProxyConfig{Domain: "example.com"},
			},
		},
	}
	services := deploy.config.Environments["production"].Services

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfigForNode(services, "node-a")
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNode returned error: %v", err)
	}
	if !hasPublic {
		t.Fatal("expected public services to be detected")
	}

	configText := string(data)
	if !strings.Contains(configText, "url: http://"+runtimeid.ContainerNetworkAlias("demo", "production", "web", 1)+":3000") {
		t.Fatalf("dynamic config missing local upstream:\n%s", configText)
	}
	if strings.Contains(configText, runtimeid.ContainerName("demo", "production", "web", 1)) {
		t.Fatalf("one-node proxy config should use DNS-safe aliases, not Docker container names:\n%s", configText)
	}
	if strings.Contains(configText, "10.210.0.") {
		t.Fatalf("one-node proxy config should not route through mesh IPs:\n%s", configText)
	}
}

func TestRenderTakodProxyDynamicConfigAddsGlobalUpstreamForNewNode(t *testing.T) {
	deploy := testProxyDeployer()
	deploy.config.Servers["node-c"] = config.ServerConfig{Host: "203.0.113.12", User: "root", Port: 22}
	env := deploy.config.Environments["production"]
	env.Servers = []string{"node-a", "node-b", "node-c"}
	web := env.Services["web"]
	web.Replicas = 0
	web.Placement = &config.PlacementConfig{Strategy: "global"}
	env.Services["web"] = web
	deploy.config.Environments["production"] = env

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfigForNode(env.Services, "node-a")
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNode returned error: %v", err)
	}
	if !hasPublic {
		t.Fatal("expected public services to be detected")
	}

	configText := string(data)
	nodeCPort, err := deploy.meshUpstreamPort("web", 3, 3000)
	if err != nil {
		t.Fatalf("meshUpstreamPort returned error: %v", err)
	}
	if !strings.Contains(configText, fmt.Sprintf("url: http://10.210.0.3:%d", nodeCPort)) {
		t.Fatalf("dynamic config missing new node upstream:\n%s", configText)
	}
}

func TestRenderTakodProxyDynamicConfigSupportsMultipleProxyPorts(t *testing.T) {
	deploy := testProxyDeployer()
	deploy.config.Environments["production"] = config.EnvironmentConfig{
		Servers: []string{"node-a"},
		Services: map[string]config.ServiceConfig{
			"web": {
				Replicas: 1,
				Ports: []config.PortConfig{
					{Name: "http", Target: 3000, Protocol: "http", Mode: "proxy", Proxy: &config.ProxyConfig{Domain: "example.com"}},
					{Name: "admin", Target: 9000, Protocol: "http", Mode: "proxy", Proxy: &config.ProxyConfig{Domain: "admin.example.com"}},
				},
			},
		},
	}

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfigForNode(deploy.config.Environments["production"].Services, "node-a")
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNode returned error: %v", err)
	}
	if !hasPublic {
		t.Fatal("expected public services")
	}
	configText := string(data)
	for _, expected := range []string{
		"rule: Host(`example.com`)",
		"rule: Host(`admin.example.com`)",
		"url: http://" + runtimeid.ContainerNetworkAlias("demo", "production", "web", 1) + ":3000",
		"url: http://" + runtimeid.ContainerNetworkAlias("demo", "production", "web", 1) + ":9000",
		"demo-production-web-admin",
	} {
		if !strings.Contains(configText, expected) {
			t.Fatalf("dynamic config missing %q:\n%s", expected, configText)
		}
	}
	if strings.Contains(configText, runtimeid.ContainerName("demo", "production", "web", 1)) {
		t.Fatalf("multi-port proxy config should use DNS-safe aliases, not Docker container names:\n%s", configText)
	}
}

func TestRenderTakodProxyDynamicConfigSkipsScaleToZero(t *testing.T) {
	deploy := testProxyDeployer()
	services := deploy.config.Environments["production"].Services
	web := services["web"]
	web.Replicas = 0
	services["web"] = web

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfigForNode(services, "node-a")
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNode returned error: %v", err)
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

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfigForNode(services, "node-a")
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNode returned error: %v", err)
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
	if _, err := deploy.meshUpstreamPort("web", meshUpstreamPortSlotLimit+1, 3000); err == nil {
		t.Fatal("expected slot at per-service range boundary to be rejected")
	}
}

func TestMeshUpstreamPortIsScopedByProjectEnvironmentAndService(t *testing.T) {
	deploy := testProxyDeployer()
	basePort, err := deploy.meshUpstreamPort("web", 1, 3000)
	if err != nil {
		t.Fatalf("meshUpstreamPort returned error: %v", err)
	}

	otherProject := testProxyDeployer()
	otherProject.config.Project.Name = "other"
	otherProjectPort, err := otherProject.meshUpstreamPort("web", 1, 3000)
	if err != nil {
		t.Fatalf("other project meshUpstreamPort returned error: %v", err)
	}

	otherEnvironment := testProxyDeployer()
	otherEnvironment.environment = "staging"
	otherEnvironment.config.Environments["staging"] = otherEnvironment.config.Environments["production"]
	otherEnvironmentPort, err := otherEnvironment.meshUpstreamPort("web", 1, 3000)
	if err != nil {
		t.Fatalf("other environment meshUpstreamPort returned error: %v", err)
	}

	apiPort, err := deploy.meshUpstreamPort("api", 1, 4000)
	if err != nil {
		t.Fatalf("api meshUpstreamPort returned error: %v", err)
	}

	if basePort == otherProjectPort || basePort == otherEnvironmentPort || basePort == apiPort {
		t.Fatalf("mesh upstream ports should differ by app/stage/service: base=%d otherProject=%d otherEnv=%d api=%d", basePort, otherProjectPort, otherEnvironmentPort, apiPort)
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
	deploy := &Deployer{config: cfg, environment: "production"}
	deploy.meshPortAllocator = func(_ string, serviceName string, slot int, containerPort int) (int, error) {
		return deploy.meshUpstreamPort(serviceName, slot, containerPort)
	}
	return deploy
}
