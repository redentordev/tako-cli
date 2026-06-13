package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

func TestCheckSSHKeysWarnsOnPasswordOnlyAuth(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"prod": {
				Host:     "example.com",
				User:     "deploy",
				Password: "${SSH_PASSWORD}",
			},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"prod"},
			},
		},
	}

	var results []checkResult
	checkSSHKeys(func(result checkResult) {
		results = append(results, result)
	}, cfg, "production")

	if len(results) != 1 {
		t.Fatalf("got %d result(s), want 1: %#v", len(results), results)
	}
	if results[0].status != "WARN" {
		t.Fatalf("password-only auth status = %q, want WARN", results[0].status)
	}
	if !strings.Contains(results[0].message, "Password auth configured") {
		t.Fatalf("unexpected warning message: %q", results[0].message)
	}
	if !strings.Contains(results[0].fix, "Prefer sshKey") {
		t.Fatalf("unexpected warning fix: %q", results[0].fix)
	}
}

func TestCollectServerConnectivityRunsConcurrentlyAndKeepsOrder(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testDoctorServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	resultsDone := make(chan []doctorConnectivityResult, 1)
	go func() {
		resultsDone <- collectServerConnectivity(servers, serverNames, func(serverName string, _ config.ServerConfig) doctorConnectivityResult {
			started <- serverName
			<-release
			return doctorConnectivityResult{
				serverName: serverName,
				client:     &ssh.Client{},
				result:     checkResult{"PASS", serverName + " connected", ""},
			}
		})
	}()

	waitForDoctorConnectivityStarts(t, started, len(serverNames))
	close(release)

	results := <-resultsDone
	if len(results) != len(serverNames) {
		t.Fatalf("results = %d, want %d", len(results), len(serverNames))
	}
	for i, serverName := range serverNames {
		if results[i].serverName != serverName {
			t.Fatalf("result %d server = %q, want %q", i, results[i].serverName, serverName)
		}
		if results[i].client == nil {
			t.Fatalf("result %d client is nil", i)
		}
		if results[i].result.message != serverName+" connected" {
			t.Fatalf("result %d message = %q", i, results[i].result.message)
		}
	}
}

func TestCollectServerConnectivityReportsMissingServerInOrder(t *testing.T) {
	results := collectServerConnectivity(
		map[string]config.ServerConfig{"node-a": {Host: "node-a.example.test"}},
		[]string{"node-a", "node-b"},
		func(serverName string, _ config.ServerConfig) doctorConnectivityResult {
			return doctorConnectivityResult{
				serverName: serverName,
				result:     checkResult{"PASS", serverName + " connected", ""},
			}
		},
	)

	if results[0].result.status != "PASS" {
		t.Fatalf("node-a status = %q, want PASS", results[0].result.status)
	}
	if results[1].serverName != "node-b" || results[1].result.status != "FAIL" {
		t.Fatalf("node-b result = %#v, want missing-server failure", results[1])
	}
	if !strings.Contains(results[1].result.message, "Not found in config") {
		t.Fatalf("node-b message = %q", results[1].result.message)
	}
}

func waitForDoctorConnectivityStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for doctor connectivity fanout; saw %v", seen)
		}
	}
}

func testDoctorServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name + ".example.test", User: "root"}
	}
	return servers
}
