package cmd

import (
	"fmt"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestCollectPrometheusNodesWithRunsConcurrentlyAndKeepsOrder(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testPrometheusServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	resultsDone := make(chan []prometheusNodeResult, 1)
	go func() {
		resultsDone <- collectPrometheusNodesWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (*MetricsData, []takod.ContainerStat, error) {
			started <- serverName
			<-release
			return &MetricsData{CPUPercent: serverName}, []takod.ContainerStat{{Name: serverName + "-web"}}, nil
		})
	}()

	waitForPrometheusStarts(t, started, len(serverNames))
	close(release)

	results := <-resultsDone
	if len(results) != len(serverNames) {
		t.Fatalf("results = %d, want %d", len(results), len(serverNames))
	}
	for i, serverName := range serverNames {
		if results[i].serverName != serverName {
			t.Fatalf("result %d server = %q, want %q", i, results[i].serverName, serverName)
		}
		if results[i].metrics == nil || results[i].metrics.CPUPercent != serverName {
			t.Fatalf("result %d metrics = %#v, want %s", i, results[i].metrics, serverName)
		}
		if len(results[i].stats) != 1 || results[i].stats[0].Name != serverName+"-web" {
			t.Fatalf("result %d stats = %#v", i, results[i].stats)
		}
	}
}

func TestCollectPrometheusNodesWithRecordsErrors(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "missing"}
	servers := testPrometheusServers(serverNames[:2])

	results := collectPrometheusNodesWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (*MetricsData, []takod.ContainerStat, error) {
		if serverName == "node-a" {
			return nil, nil, fmt.Errorf("connect refused")
		}
		return &MetricsData{CPUPercent: "1"}, nil, nil
	})

	if results[0].err == nil {
		t.Fatalf("node-a should record collector error")
	}
	if results[1].err != nil || results[1].metrics == nil {
		t.Fatalf("node-b should succeed: %#v", results[1])
	}
	if results[2].err == nil {
		t.Fatalf("missing node should record configuration error")
	}
}

func waitForPrometheusStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for prometheus fanout; saw %v", seen)
		}
	}
}

func testPrometheusServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name + ".example.test", User: "root"}
	}
	return servers
}
