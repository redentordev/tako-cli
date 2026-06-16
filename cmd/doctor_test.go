package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

func TestCheckConfigHonorsConfigFlag(t *testing.T) {
	tempDir := t.TempDir()
	keyPath := filepath.Join(tempDir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(tempDir, "custom-tako.yaml")
	if err := os.WriteFile(configPath, []byte(`
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 127.0.0.1
    user: root
    sshKey: `+keyPath+`
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
        port: 80
`), 0644); err != nil {
		t.Fatal(err)
	}

	oldCfgFile := cfgFile
	cfgFile = configPath
	t.Cleanup(func() {
		cfgFile = oldCfgFile
	})

	var results []checkResult
	cfg, err := checkConfig(func(result checkResult) {
		results = append(results, result)
	})
	if err != nil {
		t.Fatalf("checkConfig returned error: %v", err)
	}
	if cfg.Project.Name != "demo" {
		t.Fatalf("project name = %q, want demo", cfg.Project.Name)
	}
	if len(results) < 1 || results[0].status != "PASS" || !strings.Contains(results[0].message, configPath) {
		t.Fatalf("first result = %#v, want config flag path pass", results)
	}
}

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

func TestConfigLoadFixDistinguishesParseAndValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "yaml parse",
			err:  errString("failed to parse YAML config: yaml: line 1"),
			want: "Fix syntax errors",
		},
		{
			name: "missing host",
			err:  errString("invalid config: server production: host is required"),
			want: "SERVER_HOST",
		},
		{
			name: "missing env",
			err:  errString("failed to expand config environment variables: missing environment variable(s): SERVER_HOST"),
			want: "missing variable",
		},
		{
			name: "ssh key",
			err:  errString("invalid config: server production: SSH key not found"),
			want: "sshKey",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := configLoadFix(tt.err); !strings.Contains(got, tt.want) {
				t.Fatalf("configLoadFix() = %q, want containing %q", got, tt.want)
			}
		})
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

type errString string

func (e errString) Error() string {
	return string(e)
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
