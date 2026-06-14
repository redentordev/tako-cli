package reconcile

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestAggregateActualStateByServerCombinesReplicas(t *testing.T) {
	nodeAWeb := &ActualService{
		Name:       "web",
		Image:      "demo/web:1",
		Replicas:   1,
		Containers: []string{"a1"},
		ConfigHash: "hash-web",
	}
	actualByServer := map[string]map[string]*ActualService{
		"node-a": {
			"web": nodeAWeb,
		},
		"node-b": {
			"web": {
				Name:       "web",
				Image:      "demo/web:1",
				Replicas:   2,
				Containers: []string{"b1", "b2"},
				ConfigHash: "hash-web",
			},
			"worker": {
				Name:       "worker",
				Image:      "demo/worker:1",
				Replicas:   1,
				Containers: []string{"w1"},
			},
		},
	}

	aggregate := AggregateActualStateByServer(actualByServer)

	if got := aggregate["web"].Replicas; got != 3 {
		t.Fatalf("web replicas = %d, want 3", got)
	}
	if !slices.Equal(aggregate["web"].Containers, []string{"a1", "b1", "b2"}) {
		t.Fatalf("unexpected web containers: %#v", aggregate["web"].Containers)
	}
	if got := aggregate["worker"].Replicas; got != 1 {
		t.Fatalf("worker replicas = %d, want 1", got)
	}
	if got := aggregate["web"].ConfigHash; got != "hash-web" {
		t.Fatalf("web config hash = %q, want hash-web", got)
	}

	aggregate["web"].Containers[0] = "mutated"
	if nodeAWeb.Containers[0] != "a1" {
		t.Fatalf("aggregate aliased node state: %#v", nodeAWeb.Containers)
	}
}

func TestAggregateActualStateByServerClearsMixedConfigHashes(t *testing.T) {
	actualByServer := map[string]map[string]*ActualService{
		"node-a": {
			"web": {Name: "web", Image: "demo/web:1", Replicas: 1, ConfigHash: "hash-a"},
		},
		"node-b": {
			"web": {Name: "web", Image: "demo/web:1", Replicas: 1, ConfigHash: "hash-b"},
		},
	}

	aggregate := AggregateActualStateByServer(actualByServer)
	if got := aggregate["web"].ConfigHash; got != "" {
		t.Fatalf("mixed config hash = %q, want empty", got)
	}
}

func TestGatherActualStateByServerWithRunsConcurrently(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testActualStateServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	actualDone := make(chan map[string]map[string]*ActualService, 1)
	errDone := make(chan error, 1)
	go func() {
		actual, err := gatherActualStateByServerWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (map[string]*ActualService, error) {
			started <- serverName
			<-release
			return map[string]*ActualService{
				"web": {Name: "web", Replicas: 1, Containers: []string{serverName + "-web"}},
			}, nil
		})
		actualDone <- actual
		errDone <- err
	}()

	waitForActualStateStarts(t, started, len(serverNames))
	close(release)

	if err := <-errDone; err != nil {
		t.Fatalf("gatherActualStateByServerWith returned error: %v", err)
	}
	actual := <-actualDone
	for _, serverName := range serverNames {
		if got := actual[serverName]["web"].Containers[0]; got != serverName+"-web" {
			t.Fatalf("actual state for %s = %q", serverName, got)
		}
	}
}

func TestGatherActualStateByServerWithAggregatesSortedErrors(t *testing.T) {
	serverNames := []string{"node-b", "node-a"}
	servers := testActualStateServers(serverNames)

	_, err := gatherActualStateByServerWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (map[string]*ActualService, error) {
		return nil, fmt.Errorf("unavailable")
	})
	if err == nil {
		t.Fatal("expected aggregate gather error")
	}
	if got, want := err.Error(), "node-a: unavailable; node-b: unavailable"; !strings.Contains(got, want) {
		t.Fatalf("error = %q, want to contain %q", got, want)
	}
}

func waitForActualStateStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for actual state fanout; saw %v", seen)
		}
	}
}

func testActualStateServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name, User: "root"}
	}
	return servers
}
