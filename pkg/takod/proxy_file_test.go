package takod

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
