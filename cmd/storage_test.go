package cmd

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/provisioner"
)

func TestStorageTargetServersIncludesEnvironmentAndExternalNFSServer(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a":  {Host: "10.0.0.1"},
			"node-b":  {Host: "10.0.0.2"},
			"storage": {Host: "10.0.0.9"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {Servers: []string{"node-b", "node-a"}},
		},
	}

	targets, err := storageTargetServers(cfg, "production", "storage")
	if err != nil {
		t.Fatalf("storageTargetServers returned error: %v", err)
	}
	if !slices.Equal(targets, []string{"node-a", "node-b", "storage"}) {
		t.Fatalf("targets = %#v", targets)
	}
}

func TestCollectStorageStatusNodesRunsConcurrentlyAndKeepsOrder(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testStorageServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	resultsDone := make(chan []storageStatusResult, 1)
	go func() {
		resultsDone <- collectStorageStatusNodes(servers, serverNames, "node-b", func(serverName string, _ config.ServerConfig, isServer bool) (*provisioner.NFSStatus, error) {
			started <- serverName
			<-release
			return &provisioner.NFSStatus{IsServer: isServer}, nil
		})
	}()

	waitForStorageStarts(t, started, len(serverNames))
	close(release)

	results := <-resultsDone
	for i, serverName := range serverNames {
		if results[i].serverName != serverName {
			t.Fatalf("result %d server = %q, want %q", i, results[i].serverName, serverName)
		}
		if results[i].status == nil {
			t.Fatalf("result %d status is nil", i)
		}
	}
	if !results[1].status.IsServer {
		t.Fatalf("node-b should be marked as NFS server")
	}
}

func TestCollectStorageRemountNodesUsesLocalhostForNFSServer(t *testing.T) {
	serverNames := []string{"node-a", "node-b"}
	servers := testStorageServers(serverNames)
	hosts := map[string]string{}
	var mu sync.Mutex

	results := collectStorageRemountNodes(servers, serverNames, "node-b", "10.0.0.2", func(serverName string, _ config.ServerConfig, nfsHost string) error {
		mu.Lock()
		hosts[serverName] = nfsHost
		mu.Unlock()
		if serverName == "node-a" {
			return fmt.Errorf("mount failed")
		}
		return nil
	})

	if results[0].err == nil {
		t.Fatalf("node-a should record remount error")
	}
	if results[1].err != nil {
		t.Fatalf("node-b should succeed, got %v", results[1].err)
	}
	mu.Lock()
	nodeAHost := hosts["node-a"]
	nodeBHost := hosts["node-b"]
	mu.Unlock()
	if nodeAHost != "10.0.0.2" {
		t.Fatalf("node-a nfs host = %q", nodeAHost)
	}
	if nodeBHost != "localhost" {
		t.Fatalf("node-b nfs host = %q, want localhost", nodeBHost)
	}
}

func waitForStorageStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for storage fanout; saw %v", seen)
		}
	}
}

func testStorageServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name + ".example.test", User: "root"}
	}
	return servers
}
