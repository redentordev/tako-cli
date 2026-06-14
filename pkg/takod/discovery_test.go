package takod

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestResolveDiscoveryReturnsHealthyProjectNetworkEndpoints(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	networkName := runtimeid.NetworkName("demo", "production")
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "web-2\nweb-1\nworker-1\n")
	t.Setenv("TAKO_FAKE_INSPECT_OUTPUT", `[
  {
    "Id": "web-unhealthy",
    "Name": "/web-1",
    "Config": {"Labels": {"tako.service": "web"}},
    "State": {"Running": true, "Health": {"Status": "starting"}},
    "NetworkSettings": {"Networks": {"`+networkName+`": {"IPAddress": "172.20.0.10"}}}
  },
  {
    "Id": "worker-1",
    "Name": "/worker-1",
    "Config": {"Labels": {"tako.service": "worker"}},
    "State": {"Running": true},
    "NetworkSettings": {"Networks": {"`+networkName+`": {"IPAddress": "172.20.0.30"}}}
  },
  {
    "Id": "web-healthy",
    "Name": "/web-2",
    "Config": {"Labels": {"tako.service": "web"}},
    "State": {"Running": true, "Health": {"Status": "healthy"}},
    "NetworkSettings": {"Networks": {"other": {"IPAddress": "172.21.0.20"}, "`+networkName+`": {"IPAddress": "172.20.0.20"}}}
  }
]`)

	response, err := ResolveDiscovery(context.Background(), DiscoveryRequest{
		Project:     "demo",
		Environment: "production",
		Port:        3000,
	}, "node-a")
	if err != nil {
		t.Fatalf("ResolveDiscovery returned error: %v", err)
	}
	if response.Node != "node-a" {
		t.Fatalf("node = %q, want node-a", response.Node)
	}
	if len(response.Services) != 2 {
		t.Fatalf("service count = %d, want 2: %#v", len(response.Services), response.Services)
	}
	if response.Services[0].Service != "web" || response.Services[1].Service != "worker" {
		t.Fatalf("services not sorted as expected: %#v", response.Services)
	}
	web := response.Services[0].Endpoints
	if len(web) != 1 {
		t.Fatalf("web endpoint count = %d, want 1: %#v", len(web), web)
	}
	if web[0].Host != "172.20.0.20" || web[0].Address != "172.20.0.20:3000" || !web[0].Healthy {
		t.Fatalf("unexpected web endpoint: %#v", web[0])
	}

	entries := readCommandLog(t, logPath)
	if !strings.Contains(strings.Join(entries, "\n"), "docker ps --filter label=tako.project=demo --filter label=tako.environment=production --format {{.Names}}") {
		t.Fatalf("docker log missing project discovery ps: %#v", entries)
	}
}

func TestResolveDiscoveryFiltersRequestedService(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	networkName := runtimeid.NetworkName("demo", "production")
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "api-1\n")
	t.Setenv("TAKO_FAKE_INSPECT_OUTPUT", `[
  {
    "Id": "api-1",
    "Name": "/api-1",
    "Config": {"Labels": {"tako.service": "api"}},
    "State": {"Running": true},
    "NetworkSettings": {"Networks": {"`+networkName+`": {"IPAddress": "172.20.0.40"}}}
  }
]`)

	response, err := ResolveDiscovery(context.Background(), DiscoveryRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "api",
	}, "node-a")
	if err != nil {
		t.Fatalf("ResolveDiscovery returned error: %v", err)
	}
	if len(response.Endpoints) != 1 || response.Endpoints[0].Address != "" {
		t.Fatalf("unexpected response endpoints: %#v", response.Endpoints)
	}

	entries := readCommandLog(t, logPath)
	want := "docker ps --filter label=tako.project=demo --filter label=tako.environment=production --filter label=tako.service=api --format {{.Names}}"
	if !slices.Contains(entries, want) {
		t.Fatalf("docker log missing service discovery ps %q in %#v", want, entries)
	}
}

func TestResolveDiscoveryPrefersMeshPublishedEndpoint(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	networkName := runtimeid.NetworkName("demo", "production")
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "api-1\n")
	t.Setenv("TAKO_FAKE_INSPECT_OUTPUT", `[
  {
    "Id": "api-1",
    "Name": "/api-1",
    "Config": {"Labels": {"tako.service": "api"}},
    "State": {"Running": true},
    "NetworkSettings": {
      "Networks": {"`+networkName+`": {"IPAddress": "172.20.0.40"}},
      "Ports": {"3000/tcp": [{"HostIp": "10.210.0.2", "HostPort": "24567"}]}
    }
  }
]`)

	response, err := ResolveDiscovery(context.Background(), DiscoveryRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "api",
		Port:        3000,
	}, "node-a")
	if err != nil {
		t.Fatalf("ResolveDiscovery returned error: %v", err)
	}
	if len(response.Endpoints) != 1 {
		t.Fatalf("unexpected response endpoints: %#v", response.Endpoints)
	}
	endpoint := response.Endpoints[0]
	if endpoint.Scope != "mesh" || endpoint.Host != "10.210.0.2" || endpoint.Address != "10.210.0.2:24567" || endpoint.Port != 3000 {
		t.Fatalf("unexpected mesh endpoint: %#v", endpoint)
	}
}

func TestResolveDiscoveryRejectsInvalidRequest(t *testing.T) {
	_, err := ResolveDiscovery(context.Background(), DiscoveryRequest{
		Project:     "../demo",
		Environment: "production",
	}, "")
	if err == nil {
		t.Fatal("expected invalid project to be rejected")
	}

	_, err = ResolveDiscovery(context.Background(), DiscoveryRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "bad.service",
	}, "")
	if err == nil {
		t.Fatal("expected invalid service to be rejected")
	}
}

func TestResolveDiscoveryRoundRobinRotatesEndpoints(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	resetDiscoveryRoundRobin(t)

	networkName := runtimeid.NetworkName("demo", "production")
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "api-1\napi-2\n")
	t.Setenv("TAKO_FAKE_INSPECT_OUTPUT", `[
  {
    "Id": "api-1",
    "Name": "/api-1",
    "Config": {"Labels": {"tako.service": "api"}},
    "State": {"Running": true},
    "NetworkSettings": {"Networks": {"`+networkName+`": {"IPAddress": "172.20.0.11"}}}
  },
  {
    "Id": "api-2",
    "Name": "/api-2",
    "Config": {"Labels": {"tako.service": "api"}},
    "State": {"Running": true},
    "NetworkSettings": {"Networks": {"`+networkName+`": {"IPAddress": "172.20.0.12"}}}
  }
]`)

	first, err := ResolveDiscovery(context.Background(), DiscoveryRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "api",
		RoundRobin:  true,
	}, "node-a")
	if err != nil {
		t.Fatalf("first ResolveDiscovery returned error: %v", err)
	}
	second, err := ResolveDiscovery(context.Background(), DiscoveryRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "api",
		RoundRobin:  true,
	}, "node-a")
	if err != nil {
		t.Fatalf("second ResolveDiscovery returned error: %v", err)
	}

	if got := []string{first.Endpoints[0].Container, first.Endpoints[1].Container}; !slices.Equal(got, []string{"api-1", "api-2"}) {
		t.Fatalf("first endpoint order = %#v, want api-1/api-2", got)
	}
	if got := []string{second.Endpoints[0].Container, second.Endpoints[1].Container}; !slices.Equal(got, []string{"api-2", "api-1"}) {
		t.Fatalf("second endpoint order = %#v, want api-2/api-1", got)
	}
}

func TestRotateDiscoveryEndpoints(t *testing.T) {
	endpoints := []DiscoveryEndpoint{
		{Container: "a"},
		{Container: "b"},
		{Container: "c"},
	}
	rotated := rotateDiscoveryEndpoints(endpoints, 4)
	if got := []string{rotated[0].Container, rotated[1].Container, rotated[2].Container}; !slices.Equal(got, []string{"b", "c", "a"}) {
		t.Fatalf("rotated containers = %#v, want b/c/a", got)
	}
}

func resetDiscoveryRoundRobin(t *testing.T) {
	t.Helper()
	discoveryRoundRobinMu.Lock()
	defer discoveryRoundRobinMu.Unlock()
	discoveryRoundRobinOffsets = map[string]int{}
}
