package takod

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestResolveProxyTargetSelectsHealthyContainerOnProjectNetwork(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	networkName := runtimeid.NetworkName("demo", "production")
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "web-2\nweb-1\n")
	t.Setenv("TAKO_FAKE_INSPECT_OUTPUT", `[
  {
    "Id": "container-1",
    "Name": "/web-1",
    "State": {"Running": true, "Health": {"Status": "starting"}},
    "NetworkSettings": {"Networks": {"`+networkName+`": {"IPAddress": "172.20.0.10"}}}
  },
  {
    "Id": "container-2",
    "Name": "/web-2",
    "State": {"Running": true, "Health": {"Status": "healthy"}},
    "NetworkSettings": {"Networks": {"other": {"IPAddress": "172.21.0.20"}, "`+networkName+`": {"IPAddress": "172.20.0.20"}}}
  }
]`)

	response, err := ResolveProxyTarget(context.Background(), ProxyTargetRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Port:        3000,
	})
	if err != nil {
		t.Fatalf("ResolveProxyTarget returned error: %v", err)
	}
	if response.Container != "web-2" || response.ContainerID != "container-2" {
		t.Fatalf("unexpected container response: %#v", response)
	}
	if response.Host != "172.20.0.20" || response.Address != "172.20.0.20:3000" {
		t.Fatalf("unexpected proxy address: %#v", response)
	}

	entries := readCommandLog(t, logPath)
	if !strings.Contains(strings.Join(entries, "\n"), "docker ps --filter label=tako.project=demo --filter label=tako.environment=production --filter label=tako.service=web --format {{.Names}}") {
		t.Fatalf("docker log missing service-filtered ps: %#v", entries)
	}
}

func TestResolveProxyTargetAcceptsContainerWithoutHealthcheck(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	networkName := runtimeid.NetworkName("demo", "production")
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "worker-1\n")
	t.Setenv("TAKO_FAKE_INSPECT_OUTPUT", `[
  {
    "Id": "container-1",
    "Name": "/worker-1",
    "State": {"Running": true},
    "NetworkSettings": {"Networks": {"`+networkName+`": {"IPAddress": "172.20.0.5"}}}
  }
]`)

	response, err := ResolveProxyTarget(context.Background(), ProxyTargetRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "worker",
		Port:        9000,
	})
	if err != nil {
		t.Fatalf("ResolveProxyTarget returned error: %v", err)
	}
	if response.Address != "172.20.0.5:9000" {
		t.Fatalf("address = %q, want 172.20.0.5:9000", response.Address)
	}
}

func TestResolveProxyTargetRejectsContainerOnlyOnUnrelatedNetwork(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	t.Setenv("TAKO_FAKE_PS_OUTPUT", "worker-1\n")
	t.Setenv("TAKO_FAKE_INSPECT_OUTPUT", `[
  {
    "Id": "container-1",
    "Name": "/worker-1",
    "State": {"Running": true},
    "NetworkSettings": {"Networks": {"bridge": {"IPAddress": "172.17.0.5"}}}
  }
]`)

	_, err := ResolveProxyTarget(context.Background(), ProxyTargetRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "worker",
		Port:        9000,
	})
	if err == nil {
		t.Fatal("expected container on unrelated network to be rejected")
	}
}

func TestResolveProxyTargetRejectsInvalidRequest(t *testing.T) {
	_, err := ResolveProxyTarget(context.Background(), ProxyTargetRequest{
		Project:     "../demo",
		Environment: "production",
		Service:     "web",
		Port:        3000,
	})
	if err == nil {
		t.Fatal("expected invalid project to be rejected")
	}

	_, err = ResolveProxyTarget(context.Background(), ProxyTargetRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Port:        0,
	})
	if err == nil {
		t.Fatal("expected invalid port to be rejected")
	}
}
