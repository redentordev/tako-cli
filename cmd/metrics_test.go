package cmd

import (
	"fmt"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestCollectMetricsOnceWithRunsConcurrentlyAndKeepsOrder(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testMetricsServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	resultsDone := make(chan []metricsNodeResult, 1)
	go func() {
		resultsDone <- collectMetricsOnceWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (*MetricsData, error) {
			started <- serverName
			<-release
			return &MetricsData{CPUPercent: serverName}, nil
		})
	}()

	waitForMetricsStarts(t, started, len(serverNames))
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
	}
}

func TestCollectMetricsOnceWithClassifiesErrors(t *testing.T) {
	serverNames := []string{"node-a", "node-b"}
	servers := testMetricsServers(serverNames)

	results := collectMetricsOnceWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (*MetricsData, error) {
		if serverName == "node-a" {
			return nil, fmt.Errorf("connect: refused")
		}
		return nil, fmt.Errorf("takod unavailable")
	})

	if results[0].connectErr == nil || results[0].metricsErr != nil {
		t.Fatalf("node-a errors = connect:%v metrics:%v, want connect error only", results[0].connectErr, results[0].metricsErr)
	}
	if results[1].connectErr != nil || results[1].metricsErr == nil {
		t.Fatalf("node-b errors = connect:%v metrics:%v, want metrics error only", results[1].connectErr, results[1].metricsErr)
	}
}

func waitForMetricsStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for metrics fanout; saw %v", seen)
		}
	}
}

func testMetricsServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name + ".example.test", User: "root"}
	}
	return servers
}
