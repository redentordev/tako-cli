package deployer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
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

	remotePort, err := deploy.meshUpstreamPort("web", 2)
	if err != nil {
		t.Fatalf("meshUpstreamPort returned error: %v", err)
	}
	manifest := parseProxyManifest(t, data)
	if manifest.Network != runtimeid.NetworkName("demo", "production") {
		t.Fatalf("manifest network = %q, want %q", manifest.Network, runtimeid.NetworkName("demo", "production"))
	}
	route := onlyProxyRoute(t, manifest)
	assertStringsEqual(t, route.Domains, []string{"example.com"})
	assertStringsEqual(t, route.Upstreams, []string{
		"http://" + runtimeid.ContainerAlias("demo", "production", "web", 1) + ":3000",
		fmt.Sprintf("http://10.210.0.2:%d", remotePort),
	})
	if route.HealthCheck == nil || route.HealthCheck.Path != "/health" || route.HealthCheck.Interval != "15s" {
		t.Fatalf("healthCheck = %#v, want /health every 15s", route.HealthCheck)
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

	remotePort, err := deploy.meshUpstreamPort("web", 1)
	if err != nil {
		t.Fatalf("meshUpstreamPort returned error: %v", err)
	}
	route := onlyProxyRoute(t, parseProxyManifest(t, data))
	assertStringsEqual(t, route.Upstreams, []string{
		fmt.Sprintf("http://10.210.0.1:%d", remotePort),
		"http://" + runtimeid.ContainerAlias("demo", "production", "web", 2) + ":3000",
	})
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

	route := onlyProxyRoute(t, parseProxyManifest(t, data))
	assertStringsEqual(t, route.Upstreams, []string{
		"http://" + runtimeid.ContainerAlias("demo", "production", "web", 1) + ":3000",
	})
}

func TestRenderTakodProxyDynamicConfigIncludesInternalProxyRoute(t *testing.T) {
	deploy := testProxyDeployer()
	deploy.config.Environments["production"] = config.EnvironmentConfig{
		Servers: []string{"node-a"},
		Services: map[string]config.ServiceConfig{
			"web": {
				Port:     3000,
				Replicas: 1,
				Proxy: &config.ProxyConfig{
					Host:       "web.production.demo.tako.internal",
					Visibility: config.ProxyVisibilityInternal,
				},
			},
		},
	}
	services := deploy.config.Environments["production"].Services

	data, hasProxy, err := deploy.renderTakodProxyDynamicConfigForNode(services, "node-a")
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNode returned error: %v", err)
	}
	if !hasProxy {
		t.Fatal("expected proxied service to be detected")
	}

	route := onlyProxyRoute(t, parseProxyManifest(t, data))
	assertStringsEqual(t, route.Domains, []string{"web.production.demo.tako.internal"})
	if route.Visibility != config.ProxyVisibilityInternal {
		t.Fatalf("visibility = %q, want internal", route.Visibility)
	}
	assertStringsEqual(t, route.Upstreams, []string{
		"http://" + runtimeid.ContainerAlias("demo", "production", "web", 1) + ":3000",
	})
}

func TestRenderTakodProxyDynamicConfigCanRouteActiveRevision(t *testing.T) {
	deploy := testProxyDeployer()
	deploy.meshPortAllocator = func(_ string, serviceName string, revision string, slot int, _ int) (int, error) {
		if serviceName == "web" && revision == "rev-green" {
			return 43000 + slot, nil
		}
		return deploy.meshUpstreamPort(serviceName, slot)
	}
	services := deploy.config.Environments["production"].Services

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfigForNodeWithOptions(services, "node-a", takodProxyRenderOptions{
		ActiveRevisions: map[string]string{"web": "rev-green"},
	})
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNodeWithOptions returned error: %v", err)
	}
	if !hasPublic {
		t.Fatal("expected public services to be detected")
	}

	route := onlyProxyRoute(t, parseProxyManifest(t, data))
	if route.Revision != "rev-green" {
		t.Fatalf("route revision = %q, want rev-green", route.Revision)
	}
	assertStringsEqual(t, route.Upstreams, []string{
		"http://" + runtimeid.RevisionContainerAlias("demo", "production", "web", "rev-green", 1) + ":3000",
		"http://10.210.0.2:43002",
	})
	for _, upstream := range route.Upstreams {
		if strings.Contains(upstream, runtimeid.ContainerAlias("demo", "production", "web", 1)) {
			t.Fatalf("revision route should not include stable alias: %#v", route.Upstreams)
		}
	}
}

func TestNormalizeTakodProxyActiveRevisions(t *testing.T) {
	deploy := testProxyDeployer()
	services := deploy.config.Environments["production"].Services

	got, err := normalizeTakodProxyActiveRevisions(services, map[string]string{
		"web": " rev-green ",
		"api": "rev_api",
	})
	if err != nil {
		t.Fatalf("normalizeTakodProxyActiveRevisions returned error: %v", err)
	}
	if got["web"] != "rev-green" || got["api"] != "rev_api" {
		t.Fatalf("normalized revisions = %#v, want trimmed safe values", got)
	}

	got, err = normalizeTakodProxyActiveRevisions(services, nil)
	if err != nil {
		t.Fatalf("nil normalizeTakodProxyActiveRevisions returned error: %v", err)
	}
	if got != nil {
		t.Fatalf("nil active revisions normalized to %#v, want nil", got)
	}
}

func TestNormalizeTakodProxyActiveRevisionsRejectsInvalidValues(t *testing.T) {
	deploy := testProxyDeployer()
	services := deploy.config.Environments["production"].Services

	tests := []struct {
		name            string
		activeRevisions map[string]string
		want            string
	}{
		{
			name:            "unknown service",
			activeRevisions: map[string]string{"worker": "rev-green"},
			want:            "unknown service",
		},
		{
			name:            "empty revision",
			activeRevisions: map[string]string{"web": "  "},
			want:            "is required",
		},
		{
			name:            "unsafe revision",
			activeRevisions: map[string]string{"web": "../rev"},
			want:            "unsafe characters",
		},
		{
			name:            "long revision",
			activeRevisions: map[string]string{"web": strings.Repeat("a", 64)},
			want:            "unsafe characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalizeTakodProxyActiveRevisions(services, tt.activeRevisions)
			if err == nil {
				t.Fatal("expected normalizeTakodProxyActiveRevisions to reject invalid active revision")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestRenderTakodProxyDynamicConfigPrunesRemovedNodeUpstreams(t *testing.T) {
	deploy := testProxyDeployer()
	production := deploy.config.Environments["production"]
	production.Servers = []string{"node-a"}
	web := production.Services["web"]
	web.Replicas = 2
	production.Services["web"] = web
	deploy.config.Environments["production"] = production

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfigForNode(production.Services, "node-a")
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNode returned error: %v", err)
	}
	if !hasPublic {
		t.Fatal("expected public services to be detected")
	}

	route := onlyProxyRoute(t, parseProxyManifest(t, data))
	for _, expected := range []string{
		"http://" + runtimeid.ContainerAlias("demo", "production", "web", 1) + ":3000",
		"http://" + runtimeid.ContainerAlias("demo", "production", "web", 2) + ":3000",
	} {
		if !containsString(route.Upstreams, expected) {
			t.Fatalf("dynamic config missing upstream %q after node removal: %#v", expected, route.Upstreams)
		}
	}
	for _, upstream := range route.Upstreams {
		if strings.Contains(upstream, "10.210.0.2") || strings.Contains(upstream, "node-b") {
			t.Fatalf("dynamic config should not keep removed node upstreams: %#v", route.Upstreams)
		}
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

	route := onlyProxyRoute(t, parseProxyManifest(t, data))
	assertStringsEqual(t, route.RedirectFrom, []string{"www.example.com"})
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

	route := onlyProxyRoute(t, parseProxyManifest(t, data))
	if route.HealthCheck == nil || route.HealthCheck.Path != "/ready" || route.HealthCheck.Interval != "20s" {
		t.Fatalf("healthCheck = %#v, want /ready every 20s", route.HealthCheck)
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

	route := onlyProxyRoute(t, parseProxyManifest(t, data))
	if !route.Sticky {
		t.Fatalf("expected route to use sticky load balancing: %#v", route)
	}
}

func TestRenderTakodProxyDynamicConfigSupportsDynamicDomainOnlyRoute(t *testing.T) {
	deploy := testProxyDeployer()
	production := deploy.config.Environments["production"]
	production.Services["web"] = config.ServiceConfig{
		Port:     3000,
		Replicas: 1,
		Proxy: &config.ProxyConfig{
			DynamicDomains: &config.DynamicDomainsConfig{Ask: "api:/api/domains/authorize"},
		},
	}
	deploy.config.Environments["production"] = production

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfigForNode(production.Services, "node-a")
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNode returned error: %v", err)
	}
	if !hasPublic {
		t.Fatal("expected dynamic-domain public service to be detected")
	}

	route := onlyProxyRoute(t, parseProxyManifest(t, data))
	if len(route.Domains) != 0 {
		t.Fatalf("domains = %#v, want dynamic-only route", route.Domains)
	}
	if route.DynamicDomain == nil {
		t.Fatalf("dynamicDomain missing in route: %#v", route)
	}
	wantAsk := "http://api:4000/api/domains/authorize"
	if route.DynamicDomain.AskURL != wantAsk {
		t.Fatalf("askUrl = %q, want %q", route.DynamicDomain.AskURL, wantAsk)
	}
}

func TestRenderTakodProxyDynamicConfigUsesMeshAskURLForRemoteAskService(t *testing.T) {
	deploy := testProxyDeployer()
	production := deploy.config.Environments["production"]
	production.Services["web"] = config.ServiceConfig{
		Port:     3000,
		Replicas: 1,
		Proxy: &config.ProxyConfig{
			DynamicDomains: &config.DynamicDomainsConfig{Ask: "api:/api/domains/authorize"},
		},
	}
	api := production.Services["api"]
	api.Replicas = 1
	api.Placement = &config.PlacementConfig{Strategy: "pinned", Servers: []string{"node-b"}}
	production.Services["api"] = api
	deploy.config.Environments["production"] = production

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfigForNode(production.Services, "node-a")
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNode returned error: %v", err)
	}
	if !hasPublic {
		t.Fatal("expected dynamic-domain public service to be detected")
	}

	route := onlyProxyRoute(t, parseProxyManifest(t, data))
	remotePort, err := deploy.meshUpstreamPort("api", 1)
	if err != nil {
		t.Fatalf("meshUpstreamPort returned error: %v", err)
	}
	wantAsk := fmt.Sprintf("http://10.210.0.2:%d/api/domains/authorize", remotePort)
	if route.DynamicDomain == nil || route.DynamicDomain.AskURL != wantAsk {
		t.Fatalf("dynamicDomain = %#v, want ask %q", route.DynamicDomain, wantAsk)
	}
}

func TestRenderTakodProxyDynamicConfigUsesActiveRevisionForDynamicAskService(t *testing.T) {
	deploy := testProxyDeployer()
	production := deploy.config.Environments["production"]
	production.Servers = []string{"node-a"}
	production.Services["web"] = config.ServiceConfig{
		Port:     3000,
		Replicas: 1,
		Proxy: &config.ProxyConfig{
			DynamicDomains: &config.DynamicDomainsConfig{Ask: "api:/api/domains/authorize"},
		},
	}
	api := production.Services["api"]
	api.Replicas = 1
	production.Services["api"] = api
	deploy.config.Environments["production"] = production

	data, hasPublic, err := deploy.renderTakodProxyDynamicConfigForNodeWithOptions(production.Services, "node-a", takodProxyRenderOptions{
		ActiveRevisions: map[string]string{"api": "rev-api"},
	})
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNodeWithOptions returned error: %v", err)
	}
	if !hasPublic {
		t.Fatal("expected dynamic-domain public service to be detected")
	}

	route := onlyProxyRoute(t, parseProxyManifest(t, data))
	wantAsk := "http://" + runtimeid.RevisionContainerAlias("demo", "production", "api", "rev-api", 1) + ":4000/api/domains/authorize"
	if route.DynamicDomain == nil || route.DynamicDomain.AskURL != wantAsk {
		t.Fatalf("dynamicDomain = %#v, want ask %q", route.DynamicDomain, wantAsk)
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

func TestProxyHostRuleRejectsWildcardDomain(t *testing.T) {
	_, err := explicitProxyDomains(&config.ProxyConfig{Domain: " *.example.com "})
	if err == nil {
		t.Fatal("explicitProxyDomains should reject wildcard domains")
	}
	if !strings.Contains(err.Error(), "wildcard proxy domain") {
		t.Fatalf("error = %q, want wildcard proxy domain", err)
	}
}

func TestRedirectProxyDomainsRejectsWildcardDomain(t *testing.T) {
	_, err := redirectProxyDomains(&config.ProxyConfig{
		Domain:       "example.com",
		RedirectFrom: []string{"*.old.example.com"},
	})
	if err == nil {
		t.Fatal("redirectProxyDomains should reject wildcard domains")
	}
	if !strings.Contains(err.Error(), "wildcard proxy domain") {
		t.Fatalf("error = %q, want wildcard proxy domain", err)
	}
}

func TestProxyHostRuleRejectsRuleInjection(t *testing.T) {
	_, err := explicitProxyDomains(&config.ProxyConfig{Domain: "example.com`) || PathPrefix(`/"})
	if err == nil {
		t.Fatal("explicitProxyDomains should reject rule injection characters")
	}
	if !strings.Contains(err.Error(), "invalid domain") {
		t.Fatalf("error = %q, want invalid domain", err)
	}
}

func TestRenderTakodProxyDynamicConfigCarriesAccessControls(t *testing.T) {
	deploy := testProxyDeployer()
	services := deploy.config.Environments["production"].Services
	web := services["web"]
	// bcrypt("s3cret", cost 10) — a fixed fixture, not a secret.
	hash := "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	web.Proxy = &config.ProxyConfig{
		Domain:         "example.com",
		BasicAuth:      &config.ProxyBasicAuthConfig{Username: "admin", PasswordBcrypt: hash},
		AllowIps:       []string{"203.0.113.0/24", "10.0.0.1"},
		TrustedProxies: []string{"10.0.0.0/8", "2001:db8::/32"},
	}
	services["web"] = web

	data, _, err := deploy.renderTakodProxyDynamicConfigForNode(services, "node-a")
	if err != nil {
		t.Fatalf("renderTakodProxyDynamicConfigForNode returned error: %v", err)
	}
	route := onlyProxyRoute(t, parseProxyManifest(t, data))
	if route.BasicAuth == nil || route.BasicAuth.Username != "admin" || route.BasicAuth.PasswordBcrypt != hash {
		t.Fatalf("basicAuth = %#v, want admin + hash carried into manifest", route.BasicAuth)
	}
	assertStringsEqual(t, route.AllowIPs, []string{"203.0.113.0/24", "10.0.0.1"})
	assertStringsEqual(t, route.TrustedProxies, []string{"10.0.0.0/8", "2001:db8::/32"})
}

func TestProxyServicesUseTrustedProxiesOnlyForProxiedServices(t *testing.T) {
	if proxyServicesUseTrustedProxies(map[string]config.ServiceConfig{
		"web": {Port: 3000, Proxy: &config.ProxyConfig{Domain: "example.com", TrustedProxies: []string{"10.0.0.0/8"}}},
	}) != true {
		t.Fatal("proxied service with trusted proxies must require the daemon capability")
	}
	if proxyServicesUseTrustedProxies(map[string]config.ServiceConfig{
		"web":    {Port: 3000, Proxy: &config.ProxyConfig{Domain: "example.com"}},
		"worker": {},
	}) {
		t.Fatal("services without trusted proxies must not require the daemon capability")
	}
}

func TestPreflightTakodProxyCapabilitiesRunsBeforeMutationAndSkipsWhenUnset(t *testing.T) {
	trusted := map[string]config.ServiceConfig{
		"web": {Port: 3000, Proxy: &config.ProxyConfig{Domain: "example.com", TrustedProxies: []string{"203.0.113.0/24"}}},
	}
	var phases []string
	err := preflightTakodProxyCapabilitiesWithCheck(trusted, []string{"node-a"}, func(string) error {
		phases = append(phases, "status")
		return &takodclient.CapabilityRequiredError{Server: "node-a", Capability: takod.CapabilityProxyTrustedProxiesV1, Feature: "proxy trusted proxies"}
	})
	if err == nil {
		phases = append(phases, "mutation")
	}
	if got := strings.Join(phases, ","); got != "status" {
		t.Fatalf("phases = %q, want status only", got)
	}

	checks := 0
	unset := map[string]config.ServiceConfig{
		"web": {Port: 3000, Proxy: &config.ProxyConfig{Domain: "example.com"}},
	}
	if err := preflightTakodProxyCapabilitiesWithCheck(unset, []string{"node-a"}, func(string) error {
		checks++
		return nil
	}); err != nil {
		t.Fatalf("unset preflight returned error: %v", err)
	}
	if checks != 0 {
		t.Fatalf("unset trusted proxies performed %d status check(s), want 0", checks)
	}
}

func TestRunTakodProxyReconcileStaleAgentStopsBeforeMutation(t *testing.T) {
	mutations := 0
	err := runTakodProxyReconcile(func() error {
		return &takodclient.CapabilityRequiredError{Server: "node-a", Capability: takod.CapabilityProxyTrustedProxiesV1, Feature: "proxy trusted proxies"}
	}, func() error {
		mutations++
		return nil
	})
	var capabilityErr *takodclient.CapabilityRequiredError
	if !errors.As(err, &capabilityErr) {
		t.Fatalf("error = %v, want CapabilityRequiredError", err)
	}
	if mutations != 0 {
		t.Fatalf("mutations = %d, want 0", mutations)
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
	deploy.meshPortAllocator = func(_ string, serviceName string, _ string, slot int, _ int) (int, error) {
		return deploy.meshUpstreamPort(serviceName, slot)
	}
	return deploy
}

func parseProxyManifest(t *testing.T, data []byte) takod.ProxyRouteManifest {
	t.Helper()
	var manifest takod.ProxyRouteManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("failed to parse proxy route manifest: %v\n%s", err, string(data))
	}
	return manifest
}

func onlyProxyRoute(t *testing.T, manifest takod.ProxyRouteManifest) takod.ProxyRoute {
	t.Helper()
	if len(manifest.Routes) != 1 {
		t.Fatalf("routes = %#v, want exactly one route", manifest.Routes)
	}
	return manifest.Routes[0]
}

func assertStringsEqual(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
