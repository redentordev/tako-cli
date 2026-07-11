package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestResolveStatusTargetServerNames(t *testing.T) {
	cfg := statusTestConfig()

	servers, err := ResolveStatusTargetServerNames(cfg, "production", "")
	if err != nil {
		t.Fatalf("ResolveStatusTargetServerNames default returned error: %v", err)
	}
	if !slices.Equal(servers, []string{"node-a", "node-b"}) {
		t.Fatalf("servers = %#v, want environment nodes", servers)
	}

	servers, err = ResolveStatusTargetServerNames(cfg, "production", "node-b")
	if err != nil {
		t.Fatalf("ResolveStatusTargetServerNames override returned error: %v", err)
	}
	if !slices.Equal(servers, []string{"node-b"}) {
		t.Fatalf("servers = %#v, want node-b", servers)
	}

	if _, err := ResolveStatusTargetServerNames(cfg, "production", "node-c"); err == nil {
		t.Fatal("expected outside-environment server to be rejected")
	}
}

func TestGatherStatusActualStateWithRunsConcurrentlyAndMergesErrors(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := statusTestServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	actualDone := make(chan map[string]*takod.ActualService, 1)
	errDone := make(chan error, 1)
	go func() {
		actual, err := GatherStatusActualStateWith(context.Background(), servers, serverNames, func(serverName string, _ config.ServerConfig) (map[string]*takod.ActualService, error) {
			started <- serverName
			<-release
			return map[string]*takod.ActualService{
				"web": {
					Name:              "web",
					Image:             "image-" + serverName,
					Replicas:          1,
					Containers:        []string{serverName + "-web"},
					CurrentRevision:   "abcdef1234567890",
					DeployStrategy:    "rolling",
					ActiveContainers:  []string{serverName + "-web"},
					WarmingContainers: []string{serverName + "-warm"},
				},
			}, nil
		})
		actualDone <- actual
		errDone <- err
	}()

	waitForEngineStatusStarts(t, started, len(serverNames))
	close(release)

	if err := <-errDone; err != nil {
		t.Fatalf("GatherStatusActualStateWith returned error: %v", err)
	}
	actual := <-actualDone
	web := actual["web"]
	if web == nil {
		t.Fatal("missing merged web service")
	}
	if web.Replicas != 3 || web.Image != "image-node-a" {
		t.Fatalf("merged web replicas/image = %d/%q", web.Replicas, web.Image)
	}
	if !slices.Equal(web.Containers, []string{"node-a-web", "node-b-web", "node-c-web"}) {
		t.Fatalf("containers = %#v", web.Containers)
	}
	if !slices.Equal(web.WarmingContainers, []string{"node-a-warm", "node-b-warm", "node-c-warm"}) {
		t.Fatalf("warming containers = %#v", web.WarmingContainers)
	}

	_, err := GatherStatusActualStateWith(context.Background(), servers, []string{"node-b", "node-a"}, func(serverName string, _ config.ServerConfig) (map[string]*takod.ActualService, error) {
		return nil, fmt.Errorf("unavailable")
	})
	if err == nil {
		t.Fatal("expected aggregate status error")
	}
	if got, want := err.Error(), "node-a: unavailable; node-b: unavailable"; !strings.Contains(got, want) {
		t.Fatalf("error = %q, want to contain %q", got, want)
	}
}

func TestBuildStatusServiceInfoShowsRevisionWarmingAndPlacement(t *testing.T) {
	servers := statusTestServers([]string{"node-a", "node-b", "node-c"})
	services := map[string]config.ServiceConfig{
		"web": {
			Image:    "nginx:alpine",
			Port:     80,
			Replicas: 5,
			Placement: &config.PlacementConfig{
				Strategy:    "spread",
				Constraints: []string{"node.labels.role==web"},
			},
		},
	}
	servers["node-a"] = config.ServerConfig{Host: "node-a", Labels: map[string]string{"role": "web"}}
	servers["node-b"] = config.ServerConfig{Host: "node-b", Labels: map[string]string{"role": "worker"}}
	servers["node-c"] = config.ServerConfig{Host: "node-c", Labels: map[string]string{"role": "web"}}
	actual := map[string]*takod.ActualService{
		"web": {
			Name:              "web",
			Replicas:          2,
			CurrentRevision:   "abcdef1234567890",
			WarmingContainers: []string{"green-1"},
		},
	}

	infos, err := BuildStatusServiceInfo(servers, services, actual, nil, []string{"node-a", "node-b", "node-c"}, []string{"node-a"}, "")
	if err != nil {
		t.Fatalf("BuildStatusServiceInfo returned error: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("service infos = %#v, want one", infos)
	}
	if infos[0].Desired != 3 || infos[0].Running != 2 || infos[0].Status != "degraded" {
		t.Fatalf("desired/running/status = %d/%d/%q", infos[0].Desired, infos[0].Running, infos[0].Status)
	}
	if infos[0].Revision != "abcdef123456" || infos[0].Warming != 1 || infos[0].Ports != "internal" {
		t.Fatalf("revision/warming/ports = %q/%d/%q", infos[0].Revision, infos[0].Warming, infos[0].Ports)
	}
}

func TestBuildStatusServiceInfoRepresentsRunAsDeployStep(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"migrate": {Kind: config.ServiceKindRun, Image: "busybox", Command: config.ListValue("true")},
	}
	infos, err := BuildStatusServiceInfo(map[string]config.ServerConfig{"node-a": {}}, services, nil, nil, []string{"node-a"}, []string{"node-a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Kind != config.ServiceKindRun || infos[0].Status != "deploy-step" || infos[0].Desired != 0 || infos[0].Running != 0 {
		t.Fatalf("run status = %#v", infos)
	}
}

func TestMergeStatusActualStatesAggregatesHealthWorstWins(t *testing.T) {
	nodeStates := []StatusNodeActualState{
		{Server: "node-a", Services: map[string]*takod.ActualService{
			"web": {Name: "web", Replicas: 1, Health: takod.HealthStateHealthy},
		}},
		{Server: "node-b", Services: map[string]*takod.ActualService{
			"web": {Name: "web", Replicas: 1, Health: takod.HealthStateUnhealthy},
		}},
	}
	merged := MergeStatusActualStates(nodeStates)
	web := merged["web"]
	if web == nil {
		t.Fatal("missing merged web service")
	}
	if web.Health != takod.HealthStateUnhealthy {
		t.Fatalf("merged health = %q, want unhealthy", web.Health)
	}
	if web.Replicas != 2 {
		t.Fatalf("merged replicas = %d, want 2", web.Replicas)
	}
}

func TestBuildStatusServiceInfoSurfacesImageStrategyAndHealth(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web": {Image: "nginx:alpine", Port: 80},
	}
	actual := map[string]*takod.ActualService{
		"web": {
			Name:            "web",
			Image:           "nginx:alpine",
			Replicas:        1,
			CurrentRevision: "abcdef1234567890",
			DeployStrategy:  "rolling",
			Health:          takod.HealthStateHealthy,
		},
	}
	infos, err := BuildStatusServiceInfo(map[string]config.ServerConfig{"node-a": {Host: "node-a"}}, services, actual, nil, []string{"node-a"}, []string{"node-a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("service infos = %#v, want one", infos)
	}
	if infos[0].Image != "nginx:alpine" || infos[0].Strategy != "rolling" || infos[0].Health != takod.HealthStateHealthy {
		t.Fatalf("image/strategy/health = %q/%q/%q", infos[0].Image, infos[0].Strategy, infos[0].Health)
	}
}

func TestAttachStatusServiceNodesBreaksDownPlacement(t *testing.T) {
	infos := []StatusService{
		{Name: "web"},
		{Name: "reporter", Kind: config.ServiceKindJob},
	}
	nodeStates := []StatusNodeActualState{
		{Server: "node-a", Services: map[string]*takod.ActualService{
			"web": {Name: "web", Replicas: 2, WarmingContainers: []string{"warm-1"}, Health: takod.HealthStateHealthy},
		}},
		{Server: "node-b", Services: map[string]*takod.ActualService{}},
		{Server: "node-c", Services: map[string]*takod.ActualService{
			"web": {Name: "web", Replicas: 1, Health: takod.HealthStateStarting},
		}},
	}

	AttachStatusServiceNodes(infos, nodeStates)

	web := infos[0]
	if len(web.Nodes) != 2 {
		t.Fatalf("web nodes = %#v, want node-a and node-c only", web.Nodes)
	}
	if web.Nodes[0].Name != "node-a" || web.Nodes[0].Running != 2 || web.Nodes[0].Warming != 1 || web.Nodes[0].Health != takod.HealthStateHealthy {
		t.Fatalf("node-a breakdown = %#v", web.Nodes[0])
	}
	if web.Nodes[1].Name != "node-c" || web.Nodes[1].Running != 1 || web.Nodes[1].Health != takod.HealthStateStarting {
		t.Fatalf("node-c breakdown = %#v", web.Nodes[1])
	}
	if infos[1].Nodes != nil {
		t.Fatalf("job rows must not gain a node breakdown, got %#v", infos[1].Nodes)
	}
}

func TestStatusResultJSONShape(t *testing.T) {
	result := StatusResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindStatusResult,
		Project:     "demo",
		Environment: "production",
		Server:      "node-a",
		Servers:     []string{"node-a"},
		Service:     "web",
		Services: []StatusService{{
			Name:     "web",
			Desired:  2,
			Running:  1,
			Status:   "degraded",
			Ports:    "80",
			Revision: "abcdef123456",
			Warming:  1,
			Image:    "nginx:alpine",
			Strategy: "rolling",
			Health:   takod.HealthStateHealthy,
			Nodes:    []StatusServiceNode{{Name: "node-a", Running: 1, Health: takod.HealthStateHealthy}},
		}},
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(payload)
	for _, want := range []string{`"kind":"StatusResult"`, `"project":"demo"`, `"servers":["node-a"]`, `"services":[{"name":"web"`, `"desired":2`, `"running":1`, `"image":"nginx:alpine"`, `"strategy":"rolling"`, `"health":"healthy"`, `"nodes":[{"name":"node-a","running":1,"health":"healthy"}]`} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("status JSON missing %s: %s", want, jsonText)
		}
	}
}

func waitForEngineStatusStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for status fanout; saw %v", seen)
		}
	}
}

func statusTestConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
			"node-b": {Host: "10.0.0.2"},
			"node-c": {Host: "10.0.0.3"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b"},
				Services: map[string]config.ServiceConfig{
					"web": {Image: "nginx:alpine", Port: 80},
				},
			},
		},
	}
}

func statusTestServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name, User: "root"}
	}
	return servers
}
