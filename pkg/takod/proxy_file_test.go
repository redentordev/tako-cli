package takod

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaddyfileConformanceMatrix(t *testing.T) {
	hash := takodTestBcryptHash
	tests := []struct {
		name      string
		route     ProxyRoute
		wantHash  string
		contains  []string
		forbidden []string
	}{
		{
			name:      "public baseline",
			route:     ProxyRoute{Service: "web", Domains: []string{"example.com"}, Upstreams: []string{"http://web:3000"}},
			wantHash:  "2513ff975e992f48aca49ea69612b422c909227b22dff2553666d872a0b1385c",
			forbidden: []string{"trusted_proxies", "client_ip", "basic_auth"},
		},
		{
			name:      "internal",
			route:     ProxyRoute{Service: "admin", Domains: []string{"admin.production.demo.tako.internal"}, Upstreams: []string{"http://admin:3000"}, Visibility: proxyRouteVisibilityInternal},
			wantHash:  "ebc4578f22e9cfff8bbe992c8c32098e90f607c1679dc9eab84448fad148cd64",
			contains:  []string{"http://admin.production.demo.tako.internal {"},
			forbidden: []string{"trusted_proxies", "client_ip"},
		},
		{
			name:      "dynamic domains",
			route:     ProxyRoute{Service: "renderer", Upstreams: []string{"http://renderer:3000"}, DynamicDomain: &ProxyDynamicDomain{AskURL: "http://admin:3000/api/domains/authorize"}},
			wantHash:  "8ec4fe390767814fb199bc1a450ed0c98b873c81d1ee2d587daaab686c701e50",
			contains:  []string{"on_demand_tls", "ask http://admin:3000/api/domains/authorize", ":443 {"},
			forbidden: []string{"trusted_proxies", "client_ip"},
		},
		{
			name:      "auth allowlist redirects and multiple domains",
			route:     ProxyRoute{Service: "web", Domains: []string{"example.com", "app.example.com"}, RedirectFrom: []string{"www.example.com"}, Upstreams: []string{"http://web:3000"}, BasicAuth: &ProxyRouteBasicAuth{Username: "admin", PasswordBcrypt: hash}, AllowIPs: []string{"198.51.100.0/24"}},
			wantHash:  "4809415a93d6e12e97eff76f3d31cf9d099a295ddecf037bd58fa98f5a60659c",
			contains:  []string{"basic_auth {", "@tako_allowed remote_ip 198.51.100.0/24", "redir https://example.com{uri} 308"},
			forbidden: []string{"trusted_proxies", "client_ip"},
		},
		{
			name:      "trusted proxies with auth allowlist redirects and multiple domains",
			route:     ProxyRoute{Service: "web", Domains: []string{"example.com", "app.example.com"}, RedirectFrom: []string{"www.example.com"}, Upstreams: []string{"http://web:3000"}, BasicAuth: &ProxyRouteBasicAuth{Username: "admin", PasswordBcrypt: hash}, AllowIPs: []string{"198.51.100.0/24"}, TrustedProxies: []string{"10.0.0.0/8", "203.0.113.0/24"}},
			wantHash:  "6acd3bc8534bf8b222764737e8aaadb3ef66f4d8eef542577e50a8a1f42658bb",
			contains:  []string{"trusted_proxies static 10.0.0.0/8 203.0.113.0/24", "trusted_proxies_strict", "@tako_allowed client_ip 198.51.100.0/24", "basic_auth {", "redir https://example.com{uri} 308"},
			forbidden: []string{"@tako_allowed remote_ip"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderCaddyfile([]ProxyRouteManifest{{
				Version: 1, Project: "demo", Environment: "production", Routes: []ProxyRoute{tt.route},
			}})
			if err != nil {
				t.Fatalf("renderCaddyfile returned error: %v", err)
			}
			gotHash := fmt.Sprintf("%x", sha256.Sum256([]byte(got)))
			if gotHash != tt.wantHash {
				t.Errorf("Caddyfile golden hash = %s, want %s\n%s", gotHash, tt.wantHash, got)
			}
			for _, fragment := range tt.contains {
				if !strings.Contains(got, fragment) {
					t.Errorf("Caddyfile missing %q:\n%s", fragment, got)
				}
			}
			for _, fragment := range tt.forbidden {
				if strings.Contains(got, fragment) {
					t.Errorf("Caddyfile unexpectedly contains %q:\n%s", fragment, got)
				}
			}
		})
	}
}

func TestWriteAndRemoveProxyFile(t *testing.T) {
	useTempProxyPaths(t)
	content := `{
  "version": 1,
  "project": "demo",
  "environment": "production",
  "routes": [
    {
      "service": "web",
      "domains": ["example.com"],
      "upstreams": ["http://demo-web:3000"]
    }
  ]
}`

	response, err := WriteProxyFile(context.Background(), ProxyFileRequest{
		Name:    "demo-production.json",
		Content: content,
	})
	if err != nil {
		t.Fatalf("WriteProxyFile returned error: %v", err)
	}
	wantPath := filepath.Join(proxyRoutesDir, "demo-production.json")
	if response.Path != wantPath {
		t.Fatalf("path = %q, want %q", response.Path, wantPath)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("failed to read proxy file: %v", err)
	}
	if string(data) != content {
		t.Fatalf("unexpected proxy file content: %q", string(data))
	}
	caddyfile, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil {
		t.Fatalf("failed to read rendered Caddyfile: %v", err)
	}
	if string(caddyfile) == "" {
		t.Fatal("expected Caddyfile to be rendered")
	}
	if !strings.Contains(string(caddyfile), "output file /var/log/caddy/access.log") {
		t.Fatalf("expected Caddyfile to enable access logs:\n%s", string(caddyfile))
	}

	if _, err := RemoveProxyFile(context.Background(), "demo-production.json"); err != nil {
		t.Fatalf("RemoveProxyFile returned error: %v", err)
	}
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Fatalf("expected proxy file to be removed, stat err=%v", err)
	}
}

func useTempProxyPaths(t *testing.T) string {
	t.Helper()
	oldRoutesDir := proxyRoutesDir
	oldCaddyfilePath := proxyCaddyfilePath
	oldCaddyDataDir := proxyCaddyDataDir
	oldCaddyConfigDir := proxyCaddyConfigDir
	oldLogDir := proxyLogDir
	root := t.TempDir()
	proxyRoutesDir = filepath.Join(root, "routes")
	proxyCaddyfilePath = filepath.Join(root, "caddy", "Caddyfile")
	proxyCaddyDataDir = filepath.Join(root, "caddy-data")
	proxyCaddyConfigDir = filepath.Join(root, "caddy-config")
	proxyLogDir = filepath.Join(root, "logs")
	t.Cleanup(func() {
		proxyRoutesDir = oldRoutesDir
		proxyCaddyfilePath = oldCaddyfilePath
		proxyCaddyDataDir = oldCaddyDataDir
		proxyCaddyConfigDir = oldCaddyConfigDir
		proxyLogDir = oldLogDir
	})
	return root
}

func TestWriteProxyFileValidatesStagedCaddyfileWhenProxyRunsCaddy(t *testing.T) {
	useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "tako-proxy\n")
	t.Setenv("TAKO_FAKE_INSPECT_ARGS", currentProxyArgsJSON(defaultProxyEmail))

	content := `{
  "version": 1,
  "project": "demo",
  "environment": "production",
  "routes": [
    {
      "service": "web",
      "domains": ["example.com"],
      "upstreams": ["http://demo-web:3000"]
    }
  ]
}`

	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{
		Name:    "demo-production.json",
		Content: content,
	}); err != nil {
		t.Fatalf("WriteProxyFile returned error: %v", err)
	}

	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(commands, "docker exec tako-proxy caddy adapt --adapter caddyfile --config /etc/caddy/Caddyfile.next") {
		t.Fatalf("expected staged Caddyfile validation command, got:\n%s", commands)
	}
}

func TestWriteProxyFileRollsBackRouteManifestWhenCaddyValidationFails(t *testing.T) {
	useTempProxyPaths(t)
	oldContent := `{
  "version": 1,
  "project": "demo",
  "environment": "production",
  "routes": [
    {
      "service": "web",
      "domains": ["old.example.com"],
      "upstreams": ["http://demo-web:3000"]
    }
  ]
}`
	newContent := strings.ReplaceAll(oldContent, "old.example.com", "new.example.com")

	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{
		Name:    "demo-production.json",
		Content: oldContent,
	}); err != nil {
		t.Fatalf("initial WriteProxyFile returned error: %v", err)
	}
	oldCaddyfile, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil {
		t.Fatalf("failed to read initial Caddyfile: %v", err)
	}

	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "tako-proxy\n")
	t.Setenv("TAKO_FAKE_INSPECT_ARGS", currentProxyArgsJSON(defaultProxyEmail))
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_ERROR", "bad caddyfile")

	_, err = WriteProxyFile(context.Background(), ProxyFileRequest{
		Name:    "demo-production.json",
		Content: newContent,
	})
	if err == nil {
		t.Fatal("expected WriteProxyFile to fail when staged Caddyfile validation fails")
	}
	if !strings.Contains(err.Error(), "generated Caddyfile failed validation") {
		t.Fatalf("error = %q, want validation failure", err)
	}

	routePath := filepath.Join(proxyRoutesDir, "demo-production.json")
	data, err := os.ReadFile(routePath)
	if err != nil {
		t.Fatalf("failed to read restored route manifest: %v", err)
	}
	if string(data) != oldContent {
		t.Fatalf("route manifest was not rolled back:\n%s", string(data))
	}
	caddyfile, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil {
		t.Fatalf("failed to read Caddyfile after rollback: %v", err)
	}
	if string(caddyfile) != string(oldCaddyfile) {
		t.Fatalf("Caddyfile changed despite validation failure:\n%s", string(caddyfile))
	}
}

func TestValidateProxyFileNameRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{
		"",
		"../demo.json",
		"nested/demo.json",
		`nested\demo.json`,
		"demo.txt",
		"demo;rm.json",
	} {
		if _, err := validateProxyFileName(name); err == nil {
			t.Fatalf("expected %q to be rejected", name)
		}
	}
}

func TestRenderCaddyfileWithNoRoutesRespondsNotFound(t *testing.T) {
	caddyfile, err := renderCaddyfile(nil)
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	if !strings.Contains(caddyfile, `respond "tako-proxy has no routes" 404`) {
		t.Fatalf("empty Caddyfile should respond 404, got:\n%s", caddyfile)
	}
	if strings.Contains(caddyfile, "redir https://") {
		t.Fatalf("empty Caddyfile should not redirect to HTTPS:\n%s", caddyfile)
	}
}

func TestRenderCaddyfileUsesHTTPOnlyInternalRoutes(t *testing.T) {
	caddyfile, err := renderCaddyfile([]ProxyRouteManifest{
		{
			Version:     1,
			Project:     "demo",
			Environment: "production",
			Routes: []ProxyRoute{
				{
					Service:    "web",
					Domains:    []string{"web.production.demo.tako.internal"},
					Upstreams:  []string{"http://demo-web:3000"},
					Visibility: proxyRouteVisibilityInternal,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	if !strings.Contains(caddyfile, "http://web.production.demo.tako.internal {") {
		t.Fatalf("internal route should be HTTP-only:\n%s", caddyfile)
	}
	if strings.Contains(caddyfile, "web.production.demo.tako.internal {\n\ttls") {
		t.Fatalf("internal route should not request ACME TLS:\n%s", caddyfile)
	}
	if !strings.Contains(caddyfile, "reverse_proxy http://demo-web:3000") {
		t.Fatalf("internal route missing upstream:\n%s", caddyfile)
	}
}

func TestRenderCaddyfileServesMultipleDomainsForOneRoute(t *testing.T) {
	caddyfile, err := renderCaddyfile([]ProxyRouteManifest{
		{
			Version:     1,
			Project:     "demo",
			Environment: "production",
			Routes: []ProxyRoute{
				{
					Service:      "web",
					Domains:      []string{"example.com", "app.example.com", "example.app"},
					RedirectFrom: []string{"www.example.com"},
					Upstreams:    []string{"http://demo-web:3000"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	for _, domain := range []string{"example.com", "app.example.com", "example.app"} {
		if !strings.Contains(caddyfile, "\n"+domain+" {") {
			t.Fatalf("missing serving site for %s:\n%s", domain, caddyfile)
		}
	}
	if !strings.Contains(caddyfile, "redir https://example.com{uri} 308") {
		t.Fatalf("redirect should target the primary domain:\n%s", caddyfile)
	}
	if strings.Count(caddyfile, "reverse_proxy http://demo-web:3000") != 3 {
		t.Fatalf("each serving domain should proxy the same upstreams:\n%s", caddyfile)
	}
}

func TestParseProxyRouteManifestRejectsUnsafeRevision(t *testing.T) {
	_, err := ParseProxyRouteManifest(`{
		"version": 1,
		"project": "demo",
		"environment": "production",
		"routes": [
			{
				"service": "web",
				"revision": "../green",
				"domains": ["example.com"],
				"upstreams": ["http://demo-web:3000"]
			}
		]
	}`)
	if err == nil {
		t.Fatal("expected unsafe route revision to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid revision") {
		t.Fatalf("error = %q, want invalid revision", err)
	}
}

func TestRenderCaddyfileRejectsDuplicateDomains(t *testing.T) {
	_, err := renderCaddyfile([]ProxyRouteManifest{
		{
			Version:     1,
			Project:     "demo",
			Environment: "production",
			Routes: []ProxyRoute{
				{
					Service:   "web",
					Domains:   []string{"example.com"},
					Upstreams: []string{"http://demo-web:3000"},
				},
			},
		},
		{
			Version:     1,
			Project:     "other",
			Environment: "production",
			Routes: []ProxyRoute{
				{
					Service:      "app",
					Domains:      []string{"other.com"},
					RedirectFrom: []string{"example.com"},
					Upstreams:    []string{"http://other-app:3000"},
				},
			},
		},
	})
	if err == nil {
		t.Fatal("expected duplicate proxy domain to be rejected")
	}
	if !strings.Contains(err.Error(), `example.com`) {
		t.Fatalf("error = %q, want duplicate domain context", err)
	}
}

func TestRenderCaddyfileLetsHigherPriorityRouteOverrideDomain(t *testing.T) {
	caddyfile, err := renderCaddyfile([]ProxyRouteManifest{
		{
			Version:     1,
			Project:     "demo",
			Environment: "production",
			Routes: []ProxyRoute{
				{
					Service:   "web",
					Domains:   []string{"example.com"},
					Upstreams: []string{"http://demo-web:3000"},
				},
			},
		},
		{
			Version:     1,
			Project:     "demo",
			Environment: "production",
			Routes: []ProxyRoute{
				{
					Service:   "web-maintenance",
					Domains:   []string{"example.com"},
					Upstreams: []string{"http://demo-web-maintenance:80"},
					Priority:  100,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	if strings.Contains(caddyfile, "http://demo-web:3000") {
		t.Fatalf("lower priority upstream should be omitted:\n%s", caddyfile)
	}
	if !strings.Contains(caddyfile, "http://demo-web-maintenance:80") {
		t.Fatalf("higher priority upstream missing:\n%s", caddyfile)
	}
}

func TestRenderCaddyfileOrdersHigherPriorityRoutesFirst(t *testing.T) {
	caddyfile, err := renderCaddyfile([]ProxyRouteManifest{
		{
			Version:     1,
			Project:     "demo",
			Environment: "production",
			Routes: []ProxyRoute{
				{
					Service:   "alpha",
					Domains:   []string{"alpha.example.com"},
					Upstreams: []string{"http://alpha:3000"},
				},
				{
					Service:   "zeta-maintenance",
					Domains:   []string{"zeta.example.com"},
					Upstreams: []string{"http://zeta-maintenance:80"},
					Priority:  100,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	maintenanceIndex := strings.Index(caddyfile, "zeta.example.com {")
	alphaIndex := strings.Index(caddyfile, "alpha.example.com {")
	if maintenanceIndex < 0 || alphaIndex < 0 {
		t.Fatalf("expected both host blocks:\n%s", caddyfile)
	}
	if maintenanceIndex > alphaIndex {
		t.Fatalf("expected higher-priority route first:\n%s", caddyfile)
	}
}

func TestRenderCaddyfileRendersDynamicDomainAuthority(t *testing.T) {
	caddyfile, err := renderCaddyfile([]ProxyRouteManifest{
		{
			Version:     1,
			Project:     "cms",
			Environment: "production",
			Routes: []ProxyRoute{
				{
					Service:   "renderer",
					Upstreams: []string{"http://cms-renderer:3000"},
					DynamicDomain: &ProxyDynamicDomain{
						AskURL: "http://cms-admin:3000/api/domains/authorize",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	for _, expected := range []string{
		"on_demand_tls",
		"ask http://cms-admin:3000/api/domains/authorize",
		":443",
		"tls {\n\t\ton_demand",
		"reverse_proxy http://cms-renderer:3000",
	} {
		if !strings.Contains(caddyfile, expected) {
			t.Fatalf("Caddyfile missing %q:\n%s", expected, caddyfile)
		}
	}
}

func TestRenderCaddyfileSetsHealthHostHeader(t *testing.T) {
	caddyfile, err := renderCaddyfile([]ProxyRouteManifest{
		{
			Version:     1,
			Project:     "demo",
			Environment: "production",
			Routes: []ProxyRoute{
				{
					Service:   "admin",
					Domains:   []string{"admin.example.com"},
					Upstreams: []string{"http://demo-admin:3000"},
					HealthCheck: &ProxyRouteHealth{
						Path:     "/api/health",
						Interval: "10s",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	for _, expected := range []string{
		"health_uri /api/health",
		"health_headers {\n\t\t\tHost admin.example.com\n\t\t}",
	} {
		if !strings.Contains(caddyfile, expected) {
			t.Fatalf("Caddyfile missing %q:\n%s", expected, caddyfile)
		}
	}
}

func TestRenderCaddyfileSetsDynamicHealthHostHeader(t *testing.T) {
	caddyfile, err := renderCaddyfile([]ProxyRouteManifest{
		{
			Version:     1,
			Project:     "cms",
			Environment: "production",
			Routes: []ProxyRoute{
				{
					Service:   "renderer",
					Domains:   []string{"sites.example.com"},
					Upstreams: []string{"http://cms-renderer:3000"},
					HealthCheck: &ProxyRouteHealth{
						Path:     "/api/health",
						Interval: "10s",
					},
					DynamicDomain: &ProxyDynamicDomain{
						AskURL: "http://cms-admin:3000/api/domains/authorize",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	if got := strings.Count(caddyfile, "Host sites.example.com"); got != 2 {
		t.Fatalf("health Host header count = %d, want 2:\n%s", got, caddyfile)
	}
}
