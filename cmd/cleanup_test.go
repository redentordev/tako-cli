package cmd

import (
	"fmt"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestCollectCleanupNodesRunsConcurrentlyAndKeepsSortedOrder(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-c": {Host: "node-c.example.test"},
		"node-a": {Host: "node-a.example.test"},
		"node-b": {Host: "node-b.example.test"},
	}
	started := make(chan string, len(servers))
	release := make(chan struct{})

	resultsDone := make(chan []cleanupNodeResult, 1)
	go func() {
		resultsDone <- collectCleanupNodes(servers, func(serverName string, _ config.ServerConfig) (*takod.CleanupResponse, error) {
			started <- serverName
			<-release
			return &takod.CleanupResponse{ImagesRemoved: len(serverName)}, nil
		})
	}()

	waitForCleanupStarts(t, started, len(servers))
	close(release)

	results := <-resultsDone
	wantNames := []string{"node-a", "node-b", "node-c"}
	for i, want := range wantNames {
		if results[i].serverName != want {
			t.Fatalf("result %d server = %q, want %q", i, results[i].serverName, want)
		}
		if results[i].response == nil || results[i].response.ImagesRemoved == 0 {
			t.Fatalf("result %d response = %#v", i, results[i].response)
		}
	}
}

func TestCollectCleanupNodesRecordsErrors(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-a": {Host: "node-a.example.test"},
		"node-b": {Host: "node-b.example.test"},
	}

	results := collectCleanupNodes(servers, func(serverName string, _ config.ServerConfig) (*takod.CleanupResponse, error) {
		if serverName == "node-a" {
			return nil, fmt.Errorf("cleanup failed")
		}
		return &takod.CleanupResponse{}, nil
	})

	if results[0].serverName != "node-a" || results[0].err == nil {
		t.Fatalf("node-a should record cleanup error: %#v", results[0])
	}
	if results[1].serverName != "node-b" || results[1].err != nil {
		t.Fatalf("node-b should succeed: %#v", results[1])
	}
}

func waitForCleanupStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for cleanup fanout; saw %v", seen)
		}
	}
}
