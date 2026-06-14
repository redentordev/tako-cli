package takod

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestBuildProxyContainerArgs(t *testing.T) {
	got := buildProxyContainerArgs(ReconcileProxyRequest{
		Network: "tako_demo_production",
		Email:   "ops@example.com",
		Image:   "traefik:v3.6.1",
	})
	want := []string{
		"run", "-d",
		"--name", "tako-proxy",
		"--restart", "unless-stopped",
		"--network", "tako_demo_production",
		"--publish", "80:80",
		"--publish", "443:443",
		"--volume", "/etc/tako/proxy/acme:/acme",
		"--volume", "/etc/tako/proxy/dynamic:/etc/traefik/dynamic:ro",
		"--volume", "/var/log/tako/proxy:/var/log/traefik",
		"--label", "tako.runtime=takod",
		"--label", "tako.component=proxy",
		"traefik:v3.6.1",
		"--api.dashboard=false",
		"--providers.file.directory=/etc/traefik/dynamic",
		"--providers.file.watch=true",
		"--entryPoints.web.address=:80",
		"--entryPoints.websecure.address=:443",
		"--certificatesResolvers.letsencrypt.acme.email=ops@example.com",
		"--certificatesResolvers.letsencrypt.acme.storage=/acme/acme.json",
		"--certificatesResolvers.letsencrypt.acme.httpChallenge.entryPoint=web",
		"--log.level=INFO",
		"--accessLog.filePath=/var/log/traefik/access.log",
		"--accessLog.format=json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected proxy args:\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestValidateReconcileProxyRequestAllowsDefaults(t *testing.T) {
	req := ReconcileProxyRequest{Network: "tako_demo_production"}

	normalizeReconcileProxyRequest(&req)
	if err := validateReconcileProxyRequest(req); err != nil {
		t.Fatalf("expected defaulted proxy request to validate: %v", err)
	}
	if req.Email != defaultProxyEmail {
		t.Fatalf("email = %q, want %q", req.Email, defaultProxyEmail)
	}
	if req.Image != defaultProxyImage {
		t.Fatalf("image = %q, want %q", req.Image, defaultProxyImage)
	}
}

func TestValidateReconcileProxyRequestRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReconcileProxyRequest)
		want   string
	}{
		{
			name: "network",
			mutate: func(req *ReconcileProxyRequest) {
				req.Network = "tako;rm"
			},
			want: "invalid network name",
		},
		{
			name: "image",
			mutate: func(req *ReconcileProxyRequest) {
				req.Image = "--help"
			},
			want: "image must not start",
		},
		{
			name: "email",
			mutate: func(req *ReconcileProxyRequest) {
				req.Email = "ops@example.com\nbad"
			},
			want: "invalid proxy email",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ReconcileProxyRequest{Network: "tako_demo_production"}
			normalizeReconcileProxyRequest(&req)
			tt.mutate(&req)

			err := validateReconcileProxyRequest(req)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestReconcileProxyReusesRunningProxyWithRuntimePorts(t *testing.T) {
	withProxyDirs(t)
	logPath := t.TempDir() + "/commands.log"
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "tako-proxy\n")
	t.Setenv("TAKO_FAKE_INSPECT_ARGS", `["--providers.file.directory=/etc/traefik/dynamic"]`)
	t.Setenv("TAKO_FAKE_INSPECT_NETWORK_PORTS", `{"80/tcp":[{"HostIp":"","HostPort":"80"}],"443/tcp":[{"HostIp":"","HostPort":"443"}]}`)

	response, err := ReconcileProxy(context.Background(), ReconcileProxyRequest{
		Network: "tako_demo_production",
		Email:   "ops@example.com",
	})
	if err != nil {
		t.Fatalf("ReconcileProxy returned error: %v", err)
	}
	if response.Container != "tako-proxy" {
		t.Fatalf("response = %#v, want tako-proxy", response)
	}

	entries := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Contains(entries, "docker rm -f tako-proxy") || strings.Contains(entries, "docker run -d") {
		t.Fatalf("expected existing proxy to be reused, commands:\n%s", entries)
	}
	if !strings.Contains(entries, "docker network connect tako_demo_production tako-proxy") {
		t.Fatalf("expected proxy to connect to requested network, commands:\n%s", entries)
	}
}

func TestReconcileProxyRecreatesRunningProxyWithoutRuntimePorts(t *testing.T) {
	withProxyDirs(t)
	logPath := t.TempDir() + "/commands.log"
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "tako-proxy\n")
	t.Setenv("TAKO_FAKE_INSPECT_ARGS", `["--providers.file.directory=/etc/traefik/dynamic"]`)
	t.Setenv("TAKO_FAKE_INSPECT_NETWORK_PORTS", `{}`)

	_, err := ReconcileProxy(context.Background(), ReconcileProxyRequest{
		Network: "tako_demo_production",
		Email:   "ops@example.com",
	})
	if err != nil {
		t.Fatalf("ReconcileProxy returned error: %v", err)
	}

	entries := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(entries, "docker rm -f tako-proxy") {
		t.Fatalf("expected broken proxy to be removed, commands:\n%s", entries)
	}
	if !strings.Contains(entries, "docker run -d") || !strings.Contains(entries, "--publish 80:80") || !strings.Contains(entries, "--publish 443:443") {
		t.Fatalf("expected proxy to be recreated with published ports, commands:\n%s", entries)
	}
}

func TestDisableProxyRejectsExistingRouteFiles(t *testing.T) {
	dir := withProxyDynamicDir(t)
	if err := os.WriteFile(dir+"/demo-production.yml", []byte("http:\n"), 0600); err != nil {
		t.Fatalf("failed to write route file: %v", err)
	}
	if err := os.WriteFile(dir+"/notes.txt", []byte("ignored\n"), 0600); err != nil {
		t.Fatalf("failed to write ignored file: %v", err)
	}

	_, err := DisableProxy(context.Background())
	if err == nil {
		t.Fatal("expected active route files to block proxy disable")
	}
	if !strings.Contains(err.Error(), "demo-production.yml") || strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("error = %q, want only proxy route files listed", err.Error())
	}
}

func TestDisableProxyRemovesContainerWhenNoRoutesExist(t *testing.T) {
	withProxyDynamicDir(t)
	logPath := t.TempDir() + "/commands.log"
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "tako-proxy\n")

	response, err := DisableProxy(context.Background())
	if err != nil {
		t.Fatalf("DisableProxy returned error: %v", err)
	}
	if !response.Removed || response.Container != "tako-proxy" {
		t.Fatalf("response = %#v, want removed tako-proxy", response)
	}
	entries := readCommandLog(t, logPath)
	if !strings.Contains(strings.Join(entries, "\n"), "docker rm -f tako-proxy") {
		t.Fatalf("expected docker rm in command log, got %#v", entries)
	}
}

func TestListProxyRouteFilesIgnoresMissingDirectory(t *testing.T) {
	oldDir := proxyDynamicDir
	proxyDynamicDir = t.TempDir() + "/missing"
	t.Cleanup(func() { proxyDynamicDir = oldDir })

	files, err := listProxyRouteFiles()
	if err != nil {
		t.Fatalf("listProxyRouteFiles returned error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("route files = %#v, want empty", files)
	}
}

func withProxyDynamicDir(t *testing.T) string {
	t.Helper()
	oldDir := proxyDynamicDir
	dir := t.TempDir()
	proxyDynamicDir = dir
	t.Cleanup(func() { proxyDynamicDir = oldDir })
	return dir
}

func withProxyDirs(t *testing.T) {
	t.Helper()
	oldAcmeDir := proxyAcmeDir
	oldDynamicDir := proxyDynamicDir
	oldLogDir := proxyLogDir
	root := t.TempDir()
	proxyAcmeDir = root + "/acme"
	proxyDynamicDir = root + "/dynamic"
	proxyLogDir = root + "/logs"
	t.Cleanup(func() {
		proxyAcmeDir = oldAcmeDir
		proxyDynamicDir = oldDynamicDir
		proxyLogDir = oldLogDir
	})
}
