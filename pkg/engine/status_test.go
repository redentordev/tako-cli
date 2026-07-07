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
		}},
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(payload)
	for _, want := range []string{`"kind":"StatusResult"`, `"project":"demo"`, `"servers":["node-a"]`, `"services":[{"name":"web"`, `"desired":2`, `"running":1`} {
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
