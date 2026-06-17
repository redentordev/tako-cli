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
	remotePort, err := deploy.meshUpstreamPort("web", 2)
	if err != nil {
		t.Fatalf("meshUpstreamPort returned error: %v", err)
	}
	for _, expected := range []string{
		"rule: Host(`example.com`)",
		"entryPoints:",
		"certResolver: letsencrypt",
		"url: http://" + runtimeid.ContainerAlias("demo", "production", "web", 1) + ":3000",
		fmt.Sprintf("url: http://10.210.0.2:%d", remotePort),
		"path: /health",
		"interval: 15s",
	} {
		if !strings.Contains(configText, expected) {
			t.Fatalf("dynamic config missing %q:\n%s", expected, configText)
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
	remotePort, err := deploy.meshUpstreamPort("web", 1)
	if err != nil {
		t.Fatalf("meshUpstreamPort returned error: %v", err)
	}
	for _, expected := range []string{
		fmt.Sprintf("url: http://10.210.0.1:%d", remotePort),
		"url: http://" + runtimeid.ContainerAlias("demo", "production", "web", 2) + ":3000",
	} {
		if !strings.Contains(configText, expected) {
			t.Fatalf("dynamic config missing %q:\n%s", expected, configText)
		}
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
	if !strings.Contains(configText, "url: http://"+runtimeid.ContainerAlias("demo", "production", "web", 1)+":3000") {
		t.Fatalf("dynamic config missing local upstream:\n%s", configText)
	}
	if strings.Contains(configText, "url: http://web:3000") {
		t.Fatalf("local proxy upstream must use project/stage-scoped container alias, not generic service DNS:\n%s", configText)
	}
	if strings.Contains(configText, "10.210.0.") {
		t.Fatalf("one-node proxy config should not route through mesh IPs:\n%s", configText)
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

func TestRenderTakodProxyDynamicConfigRendersRedirectFromRouters(t *testing.T) {
	deploy := testProxyDeployer()
	services := deploy.config.Environments["production"].Services
	web := services["web"]
	web.Proxy.RedirectFrom = []string{"www.example.com"}
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
		"rule: Host(`www.example.com`)",
		"redirectRegex:",
		`regex: ^https?://www\.example\.com(/.*)?$`,
		"replacement: https://example.com${1}",
		"permanent: true",
	} {
		if !strings.Contains(configText, expected) {
			t.Fatalf("dynamic config missing %q:\n%s", expected, configText)
		}
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

func TestRenderTakodProxyDynamicConfigUsesStickyStrategy(t *testing.T) {
	deploy := testProxyDeployer()
	services := deploy.config.Environments["production"].Services
	web := services["web"]
	web.LoadBalancer.Strategy = "sticky"
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
		"sticky:",
		"cookie:",
		"name: tako_",
		"secure: true",
		"httpOnly: true",
	} {
		if !strings.Contains(configText, expected) {
			t.Fatalf("dynamic config missing %q:\n%s", expected, configText)
		}
	}
}

func TestGetTakodProxyTargetServersDefaultsToEnvironmentServers(t *testing.T) {
	deploy := testProxyDeployer()

	got, err := deploy.getTakodProxyTargetServers()
	if err != nil {
		t.Fatalf("getTakodProxyTargetServers returned error: %v", err)
	}
	if len(got) != 2 || got[0] != "node-a" || got[1] != "node-b" {
		t.Fatalf("proxy targets = %#v, want node-a/node-b", got)
	}
}

func TestGetTakodProxyTargetServersUsesEnvironmentProxyPlacement(t *testing.T) {
	deploy := testProxyDeployer()
	deploy.config.Servers["node-a"] = config.ServerConfig{Host: "203.0.113.10", User: "root", Port: 22, Password: "${SSH_PASSWORD}", Labels: map[string]string{"role": "edge"}}
	deploy.config.Servers["node-b"] = config.ServerConfig{Host: "203.0.113.11", User: "root", Port: 22, Password: "${SSH_PASSWORD}", Labels: map[string]string{"role": "worker"}}
	production := deploy.config.Environments["production"]
	api := production.Services["api"]
	api.Image = "nginx:alpine"
	production.Services["api"] = api
	web := production.Services["web"]
	web.Image = "nginx:alpine"
	production.Services["web"] = web
	production.Proxy = &config.EnvironmentProxyConfig{
		Placement: &config.PlacementConfig{
			Constraints: []string{"node.labels.role==edge"},
		},
	}
	deploy.config.Environments["production"] = production
	if err := config.ValidateConfig(deploy.config); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}

	got, err := deploy.getTakodProxyTargetServers()
	if err != nil {
		t.Fatalf("getTakodProxyTargetServers returned error: %v", err)
	}
	if len(got) != 1 || got[0] != "node-a" {
		t.Fatalf("proxy targets = %#v, want node-a", got)
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
	if !strings.Contains(configText, runtimeid.ContainerAlias("demo", "production", "web", 1)) {
		t.Fatalf("edge proxy config missing local upstream:\n%s", configText)
	}
	remotePort, err := deploy.meshUpstreamPort("web", 2)
	if err != nil {
		t.Fatalf("meshUpstreamPort returned error: %v", err)
	}
	if !strings.Contains(configText, fmt.Sprintf("http://10.210.0.2:%d", remotePort)) {
		t.Fatalf("edge proxy config missing remote mesh upstream:\n%s", configText)
	}
}

func TestTakodProxyReconcileTargetsPrunesNonProxyNodes(t *testing.T) {
	got := takodProxyReconcileTargets(
		[]string{"node-a", "node-b", "node-c"},
		[]string{"node-b"},
	)
	want := []takodProxyReconcileTarget{
		{ServerName: "node-a", Reconcile: false},
		{ServerName: "node-b", Reconcile: true},
		{ServerName: "node-c", Reconcile: false},
	}
	if len(got) != len(want) {
		t.Fatalf("targets = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targets[%d] = %#v, want %#v (all: %#v)", i, got[i], want[i], got)
		}
	}
}

func TestMeshUpstreamPortRejectsSlotRangeCollision(t *testing.T) {
	deploy := testProxyDeployer()
	if _, err := deploy.meshUpstreamPort("web", meshUpstreamPortSlotLimit+1); err == nil {
		t.Fatal("expected slot at per-service range boundary to be rejected")
	}
}

func TestMeshUpstreamPortIsScopedByProjectEnvironmentAndService(t *testing.T) {
	deploy := testProxyDeployer()
	basePort, err := deploy.meshUpstreamPort("web", 1)
	if err != nil {
		t.Fatalf("meshUpstreamPort returned error: %v", err)
	}

	otherProject := testProxyDeployer()
	otherProject.config.Project.Name = "other"
	otherProjectPort, err := otherProject.meshUpstreamPort("web", 1)
	if err != nil {
		t.Fatalf("other project meshUpstreamPort returned error: %v", err)
	}

	otherEnvironment := testProxyDeployer()
	otherEnvironment.environment = "staging"
	otherEnvironment.config.Environments["staging"] = otherEnvironment.config.Environments["production"]
	otherEnvironmentPort, err := otherEnvironment.meshUpstreamPort("web", 1)
	if err != nil {
		t.Fatalf("other environment meshUpstreamPort returned error: %v", err)
	}

	apiPort, err := deploy.meshUpstreamPort("api", 1)
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
	deploy.meshPortAllocator = func(_ string, serviceName string, slot int, _ int) (int, error) {
		return deploy.meshUpstreamPort(serviceName, slot)
	}
	return deploy
}
