package cmd

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
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
			got := desiredReplicasForSelection(tt.service, envServers, tt.selected)
			if got != tt.wantCount {
				t.Fatalf("desiredReplicasForSelection() = %d, want %d", got, tt.wantCount)
			}
		})
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
