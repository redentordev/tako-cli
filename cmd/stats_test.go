package cmd

import (
	"fmt"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestCollectStatsOnceWithRunsConcurrentlyAndKeepsOrder(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testStatsServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	resultsDone := make(chan []statsNodeResult, 1)
	go func() {
		resultsDone <- collectStatsOnceWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (*takod.StatsResponse, error) {
			started <- serverName
			<-release
			return &takod.StatsResponse{
				Stats: []takod.ContainerStat{{Name: serverName + "-web"}},
			}, nil
		})
	}()

	waitForStatsStarts(t, started, len(serverNames))
	close(release)

	results := <-resultsDone
	if len(results) != len(serverNames) {
		t.Fatalf("results = %d, want %d", len(results), len(serverNames))
	}
	for i, serverName := range serverNames {
		if results[i].serverName != serverName {
			t.Fatalf("result %d server = %q, want %q", i, results[i].serverName, serverName)
		}
		if len(results[i].stats) != 1 || results[i].stats[0].Name != serverName+"-web" {
			t.Fatalf("result %d stats = %#v, want %s-web", i, results[i].stats, serverName)
		}
	}
}

func TestCollectStatsOnceWithClassifiesErrors(t *testing.T) {
	serverNames := []string{"node-a", "node-b"}
	servers := testStatsServers(serverNames)

	results := collectStatsOnceWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (*takod.StatsResponse, error) {
		if serverName == "node-a" {
			return nil, fmt.Errorf("connect: refused")
		}
		return nil, fmt.Errorf("takod unavailable")
	})

	if results[0].connectErr == nil || results[0].statsErr != nil {
		t.Fatalf("node-a errors = connect:%v stats:%v, want connect error only", results[0].connectErr, results[0].statsErr)
	}
	if results[1].connectErr != nil || results[1].statsErr == nil {
		t.Fatalf("node-b errors = connect:%v stats:%v, want stats error only", results[1].connectErr, results[1].statsErr)
	}
}

func waitForStatsStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for stats fanout; saw %v", seen)
		}
	}
}

func testStatsServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name + ".example.test", User: "root"}
	}
	return servers
}
