package takod

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestBuildProxyContainerArgs(t *testing.T) {
	got := buildProxyContainerArgs(ReconcileProxyRequest{
		Network: "tako_demo_production",
		Email:   "ops@example.com",
		Image:   "caddy:2.9-alpine",
	})
	want := []string{
		"run", "-d",
		"--name", "tako-proxy",
		"--restart", "unless-stopped",
		"--network", "tako_demo_production",
		"--publish", "80:80",
		"--publish", "443:443",
		"--publish", "443:443/udp",
		"--env", "TAKO_PROXY_EMAIL=ops@example.com",
		"--volume", "/etc/tako/proxy/caddy:/etc/caddy:ro",
		"--volume", "/etc/tako/proxy/caddy-data:/data",
		"--volume", "/etc/tako/proxy/caddy-config:/config",
		"--volume", "/var/log/tako/proxy:/var/log/caddy",
		"--volume", "/var/lib/tako/certs:/var/lib/tako/certs:ro",
		"--label", "tako.runtime=takod",
		"--label", "tako.component=proxy",
		"caddy:2.9-alpine",
		"caddy",
		"run",
		"--config", "/etc/caddy/Caddyfile",
		"--adapter", "caddyfile",
		"--watch",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected proxy args:\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestProxyContainerIsCurrentRequiresCaddyRuntime(t *testing.T) {
	req := ReconcileProxyRequest{
		Network: "tako_demo_production",
		Email:   "ops@example.com",
		Image:   "caddy:2.9-alpine",
	}
	args := currentProxyArgsJSON(req.Email)
	ports := currentProxyPortsJSON()
	hostPorts := currentProxyHostPortBindingsJSON()
	mounts := currentProxyMountsJSON()
	env := currentProxyEnvJSON(req.Email)

	if !proxyContainerIsCurrent(req, args, ports, hostPorts, req.Image, mounts, env) {
		t.Fatal("expected Caddy proxy with UDP 443 publish to be current")
	}
	if !proxyContainerIsCurrent(req, args, `{}`, hostPorts, req.Image, mounts, env) {
		t.Fatal("expected HostConfig port bindings to prove published ports when network ports are empty")
	}
	if proxyContainerIsCurrent(req, `["run","--config","/etc/caddy/Caddyfile"]`, ports, hostPorts, req.Image, mounts, env) {
		t.Fatal("expected proxy without Caddy adapter/watch args to require replacement")
	}
	if proxyContainerIsCurrent(req, args, `{"80/tcp":[{"HostPort":"80"}],"443/tcp":[{"HostPort":"443"}]}`, `{}`, req.Image, mounts, env) {
		t.Fatal("expected proxy without UDP 443 publish to require replacement")
	}
}

func TestProxyContainerIsCurrentRejectsStaleImageEmailAndMounts(t *testing.T) {
	req := ReconcileProxyRequest{
		Network: "tako_demo_production",
		Email:   "ops@example.com",
		Image:   "caddy:2.9-alpine",
	}
	args := currentProxyArgsJSON(req.Email)
	ports := currentProxyPortsJSON()
	hostPorts := currentProxyHostPortBindingsJSON()
	mounts := currentProxyMountsJSON()
	env := currentProxyEnvJSON(req.Email)

	tests := []struct {
		name   string
		args   string
		image  string
		mounts string
		env    string
	}{
		{
			name:   "stale image",
			args:   args,
			image:  "nginx:alpine",
			mounts: mounts,
			env:    env,
		},
		{
			name:   "stale acme email",
			args:   args,
			image:  req.Image,
			mounts: mounts,
			env:    currentProxyEnvJSON("old@example.com"),
		},
		{
			name:   "missing access log mount",
			args:   args,
			image:  req.Image,
			mounts: `[{"Source":"/etc/tako/proxy/caddy","Destination":"/etc/caddy"},{"Source":"/etc/tako/proxy/caddy-data","Destination":"/data"},{"Source":"/etc/tako/proxy/caddy-config","Destination":"/config"}]`,
			env:    env,
		},
		{
			name:   "wrong caddy config mount source",
			args:   args,
			image:  req.Image,
			mounts: `[{"Source":"/tmp/proxy/caddy","Destination":"/etc/caddy"},{"Source":"/etc/tako/proxy/caddy-data","Destination":"/data"},{"Source":"/etc/tako/proxy/caddy-config","Destination":"/config"},{"Source":"/var/log/tako/proxy","Destination":"/var/log/caddy"}]`,
			env:    env,
		},
		{
			name:   "writable certificate store mount",
			args:   args,
			image:  req.Image,
			mounts: strings.Replace(mounts, `"Destination":"/var/lib/tako/certs"`, `"Destination":"/var/lib/tako/certs","RW":true`, 1),
			env:    env,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if proxyContainerIsCurrent(req, tt.args, ports, hostPorts, tt.image, tt.mounts, tt.env) {
				t.Fatal("expected stale proxy container to require replacement")
			}
		})
	}
}

func TestReconcileProxyConnectsRecreatedProxyToAllRouteManifestNetworks(t *testing.T) {
	useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	demoNetwork := runtimeid.NetworkName("demo", "production")
	otherNetwork := runtimeid.NetworkName("other", "production")
	writeRouteManifestForNetworkTest(t, "demo.json", "demo", "production", "demo.example.com", "http://demo-web:3000")
	writeRouteManifestForNetworkTest(t, "other.json", "other", "production", "other.example.com", "http://other-web:3000")

	if _, err := ReconcileProxy(context.Background(), ReconcileProxyRequest{Network: demoNetwork}); err != nil {
		t.Fatalf("ReconcileProxy returned error: %v", err)
	}

	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(commands, "--env TAKO_PROXY_EMAIL="+defaultProxyEmail+" --volume") || !strings.Contains(commands, "--entrypoint caddy "+defaultProxyImage+" adapt") {
		t.Fatalf("target-image validation did not use proxy runtime environment:\n%s", commands)
	}
	for _, network := range []string{demoNetwork, otherNetwork} {
		want := "docker network connect " + network + " tako-proxy"
		if !strings.Contains(commands, want) {
			t.Fatalf("expected proxy to connect to %s after recreate, got:\n%s", network, commands)
		}
	}
}

func TestReconcileProxyReplacesStaleProxyWithoutManagedCaddyArguments(t *testing.T) {
	useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	demoNetwork := runtimeid.NetworkName("demo", "production")
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "tako-proxy\n")
	t.Setenv("TAKO_FAKE_INSPECT_ARGS", `["legacy-proxy"]`)
	t.Setenv("TAKO_FAKE_INSPECT_PORTS", "{}")
	t.Setenv("TAKO_FAKE_INSPECT_PORT_BINDINGS", currentProxyHostPortBindingsJSON())
	t.Setenv("TAKO_FAKE_INSPECT_IMAGE", "legacy-proxy:1")
	t.Setenv("TAKO_FAKE_INSPECT_MOUNTS", `[]`)
	t.Setenv("TAKO_FAKE_INSPECT_ENV", `[]`)

	if _, err := ReconcileProxy(context.Background(), ReconcileProxyRequest{Network: demoNetwork}); err != nil {
		t.Fatalf("ReconcileProxy returned error for stale proxy: %v", err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(commands, "docker rename tako-proxy tako-proxy-rollback-") || !strings.Contains(commands, "docker run -d --name tako-proxy") {
		t.Fatalf("stale proxy was not replaced:\n%s", commands)
	}
}

func TestReconcileProxyConnectsCurrentProxyToAllRouteManifestNetworks(t *testing.T) {
	useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	demoNetwork := runtimeid.NetworkName("demo", "production")
	otherNetwork := runtimeid.NetworkName("other", "production")
	writeRouteManifestForNetworkTest(t, "demo.json", "demo", "production", "demo.example.com", "http://demo-web:3000")
	writeRouteManifestForNetworkTest(t, "other.json", "other", "production", "other.example.com", "http://other-web:3000")
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "tako-proxy\n")
	t.Setenv("TAKO_FAKE_INSPECT_ARGS", currentProxyArgsJSON(defaultProxyEmail))
	t.Setenv("TAKO_FAKE_INSPECT_PORTS", "{}")
	t.Setenv("TAKO_FAKE_INSPECT_PORT_BINDINGS", currentProxyHostPortBindingsJSON())
	t.Setenv("TAKO_FAKE_INSPECT_IMAGE", defaultProxyImage)
	t.Setenv("TAKO_FAKE_INSPECT_MOUNTS", currentProxyMountsJSON())
	t.Setenv("TAKO_FAKE_INSPECT_ENV", currentProxyEnvJSON(defaultProxyEmail))

	if _, err := ReconcileProxy(context.Background(), ReconcileProxyRequest{Network: demoNetwork}); err != nil {
		t.Fatalf("ReconcileProxy returned error: %v", err)
	}

	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Contains(commands, "docker rm -f tako-proxy") {
		t.Fatalf("expected current proxy to be reused, got:\n%s", commands)
	}
	for _, network := range []string{demoNetwork, otherNetwork} {
		want := "docker network connect " + network + " tako-proxy"
		if !strings.Contains(commands, want) {
			t.Fatalf("expected current proxy to connect to %s, got:\n%s", network, commands)
		}
	}
}

func TestReconcileProxyRecreatesCurrentProxyWhenNetworkAttachFails(t *testing.T) {
	useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	demoNetwork := runtimeid.NetworkName("demo", "production")
	writeRouteManifestForNetworkTest(t, "demo.json", "demo", "production", "demo.example.com", "http://demo-web:3000")
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "tako-proxy\n")
	t.Setenv("TAKO_FAKE_INSPECT_ARGS", currentProxyArgsJSON(defaultProxyEmail))
	t.Setenv("TAKO_FAKE_INSPECT_PORTS", "{}")
	t.Setenv("TAKO_FAKE_INSPECT_PORT_BINDINGS", currentProxyHostPortBindingsJSON())
	t.Setenv("TAKO_FAKE_INSPECT_IMAGE", defaultProxyImage)
	t.Setenv("TAKO_FAKE_INSPECT_MOUNTS", currentProxyMountsJSON())
	t.Setenv("TAKO_FAKE_INSPECT_ENV", currentProxyEnvJSON(defaultProxyEmail))
	t.Setenv("TAKO_FAKE_FAIL_NETWORK_CONNECT_ONCE_FILE", filepath.Join(t.TempDir(), "failed-once"))

	if _, err := ReconcileProxy(context.Background(), ReconcileProxyRequest{Network: demoNetwork}); err != nil {
		t.Fatalf("ReconcileProxy returned error: %v", err)
	}

	commands := strings.Join(readCommandLog(t, logPath), "\n")
	for _, want := range []string{
		"docker rename tako-proxy tako-proxy-rollback-",
		"docker stop tako-proxy-rollback-",
		"docker run -d --name tako-proxy",
		"docker network connect " + demoNetwork + " tako-proxy",
	} {
		if !strings.Contains(commands, want) {
			t.Fatalf("expected command %q after failed attach, got:\n%s", want, commands)
		}
	}
}

func TestReconcileProxyRestoresPreviousProxyWhenReplacementExits(t *testing.T) {
	useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	demoNetwork := runtimeid.NetworkName("demo", "production")
	writeRouteManifestForNetworkTest(t, "demo.json", "demo", "production", "demo.example.com", "http://demo-web:3000")
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "tako-proxy\n")
	t.Setenv("TAKO_FAKE_INSPECT_ARGS", currentProxyArgsJSON(defaultProxyEmail))
	t.Setenv("TAKO_FAKE_INSPECT_PORTS", "{}")
	t.Setenv("TAKO_FAKE_INSPECT_PORT_BINDINGS", currentProxyHostPortBindingsJSON())
	t.Setenv("TAKO_FAKE_INSPECT_IMAGE", "stale-caddy:2")
	t.Setenv("TAKO_FAKE_INSPECT_MOUNTS", currentProxyMountsJSON())
	t.Setenv("TAKO_FAKE_INSPECT_ENV", currentProxyEnvJSON(defaultProxyEmail))
	t.Setenv("TAKO_FAKE_PROXY_RUNNING_FALSE_ONCE_FILE", filepath.Join(t.TempDir(), "exited-once"))

	if _, err := ReconcileProxy(context.Background(), ReconcileProxyRequest{Network: demoNetwork}); err == nil || !strings.Contains(err.Error(), "did not remain running") {
		t.Fatalf("ReconcileProxy error = %v, want replacement startup failure", err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	for _, want := range []string{"docker rename tako-proxy tako-proxy-rollback-", "docker rename tako-proxy-rollback-", "tako-proxy", "docker start tako-proxy", "docker exec tako-proxy caddy reload"} {
		if !strings.Contains(commands, want) {
			t.Fatalf("expected rollback command %q, got:\n%s", want, commands)
		}
	}
}

func TestReconcileProxyForceClearsFailedCandidateBeforeRestoringPreviousProxy(t *testing.T) {
	useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	demoNetwork := runtimeid.NetworkName("demo", "production")
	marker := filepath.Join(t.TempDir(), "proxy-running")
	if err := os.WriteFile(marker, []byte("running"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAKO_FAKE_PROXY_RUNNING_MARKER", marker)
	t.Setenv("TAKO_FAKE_INSPECT_ARGS", currentProxyArgsJSON(defaultProxyEmail))
	t.Setenv("TAKO_FAKE_INSPECT_PORTS", "{}")
	t.Setenv("TAKO_FAKE_INSPECT_PORT_BINDINGS", currentProxyHostPortBindingsJSON())
	t.Setenv("TAKO_FAKE_INSPECT_IMAGE", "stale-caddy:2")
	t.Setenv("TAKO_FAKE_INSPECT_MOUNTS", currentProxyMountsJSON())
	t.Setenv("TAKO_FAKE_INSPECT_ENV", currentProxyEnvJSON(defaultProxyEmail))
	t.Setenv("TAKO_FAKE_PROXY_RUNNING_FALSE_ONCE_FILE", filepath.Join(t.TempDir(), "exited-once"))
	t.Setenv("TAKO_FAKE_DOCKER_STOP_FAIL_EXACT", "tako-proxy")

	if _, err := ReconcileProxy(context.Background(), ReconcileProxyRequest{Network: demoNetwork}); err == nil || !strings.Contains(err.Error(), "did not remain running") {
		t.Fatalf("ReconcileProxy error = %v, want replacement startup failure", err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	for _, want := range []string{"docker rm -f tako-proxy", "docker rename tako-proxy-rollback-", "docker start tako-proxy"} {
		if !strings.Contains(commands, want) {
			t.Fatalf("expected fail-closed rollback command %q, got:\n%s", want, commands)
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("previous proxy was not restored: %v", err)
	}
}

func TestReconcileProxyStopsRestoredProxyWhenRollbackReloadFails(t *testing.T) {
	useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	demoNetwork := runtimeid.NetworkName("demo", "production")
	marker := filepath.Join(t.TempDir(), "proxy-running")
	if err := os.WriteFile(marker, []byte("running"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAKO_FAKE_PROXY_RUNNING_MARKER", marker)
	t.Setenv("TAKO_FAKE_INSPECT_ARGS", currentProxyArgsJSON(defaultProxyEmail))
	t.Setenv("TAKO_FAKE_INSPECT_PORTS", "{}")
	t.Setenv("TAKO_FAKE_INSPECT_PORT_BINDINGS", currentProxyHostPortBindingsJSON())
	t.Setenv("TAKO_FAKE_INSPECT_IMAGE", "stale-caddy:2")
	t.Setenv("TAKO_FAKE_INSPECT_MOUNTS", currentProxyMountsJSON())
	t.Setenv("TAKO_FAKE_INSPECT_ENV", currentProxyEnvJSON(defaultProxyEmail))
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_FAIL_CONTAINS", "caddy reload")

	if _, err := ReconcileProxy(context.Background(), ReconcileProxyRequest{Network: demoNetwork}); err == nil || !strings.Contains(err.Error(), "reactivate restored proxy") {
		t.Fatalf("ReconcileProxy error = %v, want rollback activation failure", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unverified restored proxy was left running: %v", err)
	}
	if !strings.Contains(strings.Join(readCommandLog(t, logPath), "\n"), "docker rm -f tako-proxy") {
		t.Fatal("rollback activation failure did not stop proxy fail-closed")
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

func writeRouteManifestForNetworkTest(t *testing.T, name string, project string, environment string, domain string, upstream string) {
	t.Helper()
	content := `{
  "version": 1,
  "project": "` + project + `",
  "environment": "` + environment + `",
  "network": "` + runtimeid.NetworkName(project, environment) + `",
  "routes": [
    {
      "service": "web",
      "domains": ["` + domain + `"],
      "upstreams": ["` + upstream + `"]
    }
  ]
}`
	if err := os.MkdirAll(proxyRoutesDir, 0755); err != nil {
		t.Fatalf("failed to create proxy route dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proxyRoutesDir, name), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write proxy route manifest: %v", err)
	}
}

func currentProxyArgsJSON(email string) string {
	return `[` +
		`"run",` +
		`"--config",` +
		`"/etc/caddy/Caddyfile",` +
		`"--adapter",` +
		`"caddyfile",` +
		`"--watch"` +
		`]`
}

func currentProxyPortsJSON() string {
	return `{"80/tcp":[{"HostPort":"80"}],"443/tcp":[{"HostPort":"443"}],"443/udp":[{"HostPort":"443"}]}`
}

func currentProxyHostPortBindingsJSON() string {
	return currentProxyPortsJSON()
}

func currentProxyMountsJSON() string {
	return `[` +
		`{"Source":"` + filepath.Dir(proxyCaddyfilePath) + `","Destination":"/etc/caddy"},` +
		`{"Source":"` + proxyCaddyDataDir + `","Destination":"/data"},` +
		`{"Source":"` + proxyCaddyConfigDir + `","Destination":"/config"},` +
		`{"Source":"` + proxyLogDir + `","Destination":"/var/log/caddy"},` +
		`{"Source":"` + proxyCertStoreDir + `","Destination":"` + proxyCertContainerDir + `"}` +
		`]`
}

func currentProxyEnvJSON(email string) string {
	return `["PATH=/usr/bin","TAKO_PROXY_EMAIL=` + email + `"]`
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
