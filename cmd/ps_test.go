package cmd

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

func TestDesiredReplicasForSelection(t *testing.T) {
	envServers := []string{"node-a", "node-b", "node-c"}

	tests := []struct {
		name      string
		service   config.ServiceConfig
		selected  []string
		wantCount int
	}{
		{
			name: "spread counts only selected nodes",
			service: config.ServiceConfig{
				Replicas: 5,
			},
			selected:  []string{"node-a", "node-c"},
			wantCount: 3,
		},
		{
			name: "global maps one replica per environment node",
			service: config.ServiceConfig{
				Placement: &config.PlacementConfig{Strategy: "global"},
			},
			selected:  []string{"node-b"},
			wantCount: 1,
		},
		{
			name: "pinned ignores unselected nodes",
			service: config.ServiceConfig{
				Replicas: 3,
				Placement: &config.PlacementConfig{
					Strategy: "pinned",
					Servers:  []string{"node-b"},
				},
			},
			selected:  []string{"node-a"},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			servers := testPSActualStateServers(envServers)
			got, err := desiredReplicasForSelection(servers, tt.service, envServers, tt.selected)
			if err != nil {
				t.Fatalf("desiredReplicasForSelection returned error: %v", err)
			}
			if got != tt.wantCount {
				t.Fatalf("desiredReplicasForSelection() = %d, want %d", got, tt.wantCount)
			}
		})
	}
}

func TestDesiredReplicasForSelectionHonorsPlacementConstraints(t *testing.T) {
	envServers := []string{"node-a", "node-b", "node-c"}
	servers := testPSActualStateServers(envServers)
	servers["node-a"] = config.ServerConfig{Host: "node-a", Labels: map[string]string{"role": "web"}}
	servers["node-b"] = config.ServerConfig{Host: "node-b", Labels: map[string]string{"role": "worker"}}
	servers["node-c"] = config.ServerConfig{Host: "node-c", Labels: map[string]string{"role": "web"}}

	got, err := desiredReplicasForSelection(servers, config.ServiceConfig{
		Replicas: 4,
		Placement: &config.PlacementConfig{
			Strategy:    "spread",
			Constraints: []string{"node.labels.role==web"},
		},
	}, envServers, []string{"node-a"})
	if err != nil {
		t.Fatalf("desiredReplicasForSelection returned error: %v", err)
	}
	if got != 2 {
		t.Fatalf("desiredReplicasForSelection() = %d, want 2", got)
	}
}

func TestBuildPSServiceInfoUsesRuntimeDesiredRevision(t *testing.T) {
	servers := testPSActualStateServers([]string{"node-a"})
	services := map[string]config.ServiceConfig{
		"web": {
			Replicas: 1,
			Port:     3000,
		},
	}
	actualServices := map[string]*takod.ActualService{
		"web": {
			Name:     "web",
			Replicas: 2,
		},
	}
	desired := &takodstate.DesiredRevision{
		Project:     "demo",
		Environment: "production",
		TargetNodes: []string{"node-a"},
		Services: map[string]takodstate.DesiredService{
			"web": {
				Name:     "web",
				Replicas: 2,
			},
		},
	}

	infos, err := buildPSServiceInfo(servers, services, actualServices, desired, []string{"node-a"}, []string{"node-a"}, "")
	if err != nil {
		t.Fatalf("buildPSServiceInfo returned error: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("service info count = %d, want 1", len(infos))
	}
	if got := infos[0].Desired; got != 2 {
		t.Fatalf("desired replicas = %d, want runtime desired count 2", got)
	}
	if got := infos[0].Status; got != "running" {
		t.Fatalf("status = %q, want running", got)
	}
}

func TestBuildPSServiceInfoReportsUnhealthyReplicas(t *testing.T) {
	servers := testPSActualStateServers([]string{"node-a"})
	services := map[string]config.ServiceConfig{
		"renderer": {
			Replicas: 2,
			Port:     3000,
		},
	}
	actualServices := map[string]*takod.ActualService{
		"renderer": {
			Name:              "renderer",
			Replicas:          2,
			HealthyReplicas:   1,
			UnhealthyReplicas: 1,
		},
	}

	infos, err := buildPSServiceInfo(servers, services, actualServices, nil, []string{"node-a"}, []string{"node-a"}, "")
	if err != nil {
		t.Fatalf("buildPSServiceInfo returned error: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("service info count = %d, want 1", len(infos))
	}
	if infos[0].Status != "unhealthy" {
		t.Fatalf("status = %q, want unhealthy", infos[0].Status)
	}
	if infos[0].Health != "1 healthy, 1 unhealthy" {
		t.Fatalf("health = %q, want health count breakdown", infos[0].Health)
	}
}

func TestServicePortsFormatsExplicitPorts(t *testing.T) {
	service := config.ServiceConfig{
		Ports: []config.PortConfig{
			{Name: "http", Target: 3000, Mode: "proxy", Protocol: "http", Proxy: &config.ProxyConfig{Domain: "example.com"}},
			{Name: "metrics", Target: 9090, Mode: "internal", Protocol: "tcp"},
			{Name: "dns", Target: 5353, Published: 15353, Mode: "host", Protocol: "udp"},
		},
	}

	got := servicePorts(service, false, 1)
	for _, want := range []string{"http:3000", "metrics:9090/internal", "dns:15353->5353/udp"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ports = %q, want %q", got, want)
		}
	}
}

func TestPSTargetServersUsesEnvironmentNodesByDefault(t *testing.T) {
	cfg := resolverConfig()
	psServer = ""
	t.Cleanup(func() { psServer = "" })

	servers, err := psTargetServers(cfg, "production")
	if err != nil {
		t.Fatalf("psTargetServers returned error: %v", err)
	}
	if !slices.Equal(servers, []string{"node-a", "node-b"}) {
		t.Fatalf("servers = %#v, want production nodes", servers)
	}
}

func TestPSTargetServersHonorsServerOverride(t *testing.T) {
	cfg := resolverConfig()
	psServer = "node-b"
	t.Cleanup(func() { psServer = "" })

	servers, err := psTargetServers(cfg, "production")
	if err != nil {
		t.Fatalf("psTargetServers returned error: %v", err)
	}
	if !slices.Equal(servers, []string{"node-b"}) {
		t.Fatalf("servers = %#v, want node-b", servers)
	}
}

func TestPSTargetServersRejectsServerOutsideEnvironment(t *testing.T) {
	cfg := resolverConfig()
	psServer = "node-c"
	t.Cleanup(func() { psServer = "" })

	if _, err := psTargetServers(cfg, "production"); err == nil {
		t.Fatal("psTargetServers should reject a server outside the environment")
	}
}

func TestGatherPSActualStateWithRunsConcurrentlyAndMergesInServerOrder(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testPSActualStateServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	actualDone := make(chan map[string]*takod.ActualService, 1)
	errDone := make(chan error, 1)
	go func() {
		actual, err := gatherPSActualStateWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (map[string]*takod.ActualService, error) {
			started <- serverName
			<-release
			return map[string]*takod.ActualService{
				"web": {
					Name:       "web",
					Image:      "image-" + serverName,
					Replicas:   1,
					Containers: []string{serverName + "-web"},
				},
			}, nil
		})
		actualDone <- actual
		errDone <- err
	}()

	waitForPSActualStateStarts(t, started, len(serverNames))
	close(release)

	if err := <-errDone; err != nil {
		t.Fatalf("gatherPSActualStateWith returned error: %v", err)
	}
	actual := <-actualDone
	web := actual["web"]
	if web == nil {
		t.Fatal("missing merged web service")
	}
	if web.Replicas != 3 {
		t.Fatalf("web replicas = %d, want 3", web.Replicas)
	}
	if web.Image != "image-node-a" {
		t.Fatalf("web image = %q, want selected source image", web.Image)
	}
	wantContainers := []string{"node-a-web", "node-b-web", "node-c-web"}
	if !slices.Equal(web.Containers, wantContainers) {
		t.Fatalf("containers = %#v, want %#v", web.Containers, wantContainers)
	}
}

func TestGatherPSActualStateWithAggregatesSortedErrors(t *testing.T) {
	serverNames := []string{"node-b", "node-a"}
	servers := testPSActualStateServers(serverNames)

	_, err := gatherPSActualStateWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (map[string]*takod.ActualService, error) {
		return nil, fmt.Errorf("unavailable")
	})
	if err == nil {
		t.Fatal("expected aggregate ps error")
	}
	if got, want := err.Error(), "node-a: unavailable; node-b: unavailable"; !strings.Contains(got, want) {
		t.Fatalf("error = %q, want to contain %q", got, want)
	}
}

func waitForPSActualStateStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for ps actual state fanout; saw %v", seen)
		}
	}
}

func testPSActualStateServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name, User: "root"}
	}
	return servers
}
