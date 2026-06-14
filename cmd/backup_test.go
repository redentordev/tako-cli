package cmd

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestCollectBackupNodesRunsConcurrentlyAndKeepsSortedOrder(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-c": {Host: "node-c.example.test"},
		"node-a": {Host: "node-a.example.test"},
		"node-b": {Host: "node-b.example.test"},
	}
	started := make(chan string, len(servers))
	release := make(chan struct{})

	resultsDone := make(chan []backupNodeResult, 1)
	go func() {
		resultsDone <- collectBackupNodes(servers, func(serverName string, _ config.ServerConfig) (backupNodeActionResult, error) {
			started <- serverName
			<-release
			return backupNodeActionResult{
				backups: []takod.BackupInfo{{Volume: "data", ID: "20240101-120000", Size: int64(len(serverName))}},
			}, nil
		})
	}()

	waitForBackupStarts(t, started, len(servers))
	close(release)

	results := <-resultsDone
	wantNames := []string{"node-a", "node-b", "node-c"}
	for i, want := range wantNames {
		if results[i].serverName != want {
			t.Fatalf("result %d server = %q, want %q", i, results[i].serverName, want)
		}
		if len(results[i].backups) != 1 {
			t.Fatalf("result %d backups = %#v", i, results[i].backups)
		}
	}
}

func TestCollectBackupNodesRecordsErrorsWithPayload(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-a": {Host: "node-a.example.test"},
		"node-b": {Host: "node-b.example.test"},
	}

	results := collectBackupNodes(servers, func(serverName string, _ config.ServerConfig) (backupNodeActionResult, error) {
		if serverName == "node-a" {
			return backupNodeActionResult{
				skipped: []string{"data: volume not present on node"},
			}, fmt.Errorf("backup failed")
		}
		return backupNodeActionResult{
			backups: []takod.BackupInfo{{Volume: "data", ID: "20240101-120000"}},
		}, nil
	})

	if results[0].serverName != "node-a" || results[0].err == nil || len(results[0].skipped) != 1 {
		t.Fatalf("node-a should record error and payload: %#v", results[0])
	}
	if results[1].serverName != "node-b" || results[1].err != nil || len(results[1].backups) != 1 {
		t.Fatalf("node-b should succeed: %#v", results[1])
	}
}

func TestEnsureSingleBackupRestoreTargetRequiresServerForMultiNode(t *testing.T) {
	if err := ensureSingleBackupRestoreTarget([]string{"node-a", "node-b"}, ""); err == nil {
		t.Fatal("expected restore without --server to fail for multi-node target")
	}
	if err := ensureSingleBackupRestoreTarget([]string{"node-a", "node-b"}, "node-a"); err != nil {
		t.Fatalf("restore with --server should be allowed: %v", err)
	}
	if err := ensureSingleBackupRestoreTarget([]string{"node-a"}, ""); err != nil {
		t.Fatalf("single-node restore should be allowed without --server: %v", err)
	}
}

func TestNewBackupIDFormat(t *testing.T) {
	id := newBackupID()
	if _, err := time.Parse("20060102-150405", id); err != nil {
		t.Fatalf("backup ID %q is not parseable: %v", id, err)
	}
}

func TestBackupVolumeNameFromSpecDistinguishesNamedVolumesFromBinds(t *testing.T) {
	tests := []struct {
		name   string
		spec   string
		want   string
		wantOK bool
	}{
		{name: "shorthand target becomes named volume", spec: "/data", want: "/data", wantOK: true},
		{name: "named source with target", spec: "cache:/cache", want: "cache", wantOK: true},
		{name: "host bind is skipped", spec: "/srv/uploads:/uploads", wantOK: false},
		{name: "empty target is skipped", spec: "cache:", wantOK: false},
		{name: "nfs volume is skipped", spec: "nfs:/exports/data", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := backupVolumeNameFromSpec(tt.spec)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("backupVolumeNameFromSpec(%q) = %q, %v; want %q, %v", tt.spec, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestBackupVolumesFromConfigIncludesTakoVolumeShorthand(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Services: map[string]config.ServiceConfig{
					"web": {
						Volumes: []string{"/data", "cache:/cache", "/srv/uploads:/uploads"},
					},
					"worker": {
						Volumes: []string{"queue:/queue"},
					},
				},
			},
		},
	}

	got, err := backupVolumesFromConfig(cfg, "production")
	if err != nil {
		t.Fatalf("backupVolumesFromConfig returned error: %v", err)
	}
	want := []backupVolumeSpec{
		{name: "/data", service: "web"},
		{name: "cache", service: "web"},
		{name: "queue", service: "worker"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("backup volumes = %#v, want %#v", got, want)
	}
}

func TestConnectBackupNodeUsesProvidedPool(t *testing.T) {
	provider := &fakeSSHClientProvider{}
	server := config.ServerConfig{
		Host:     "node-a.example.test",
		Port:     2222,
		User:     "deploy",
		SSHKey:   "/tmp/id_ed25519",
		Password: "fallback",
	}

	if _, err := connectBackupNode(provider, server); err != nil {
		t.Fatalf("connectBackupNode returned error: %v", err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("pool requests = %#v, want one", provider.requests)
	}
	got := provider.requests[0]
	if got.host != server.Host || got.port != server.Port || got.user != server.User || got.sshKey != server.SSHKey || got.password != server.Password {
		t.Fatalf("pool request = %#v, want server config", got)
	}
}

func TestConnectBackupNodeReturnsPoolConnectionError(t *testing.T) {
	provider := &fakeSSHClientProvider{err: fmt.Errorf("dial failed")}

	_, err := connectBackupNode(provider, config.ServerConfig{Host: "node-a.example.test"})
	if err == nil {
		t.Fatal("connectBackupNode returned nil, want connection error")
	}
	if got := err.Error(); got != "dial failed" {
		t.Fatalf("error = %q", got)
	}
}

func waitForBackupStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for backup fanout; saw %v", seen)
		}
	}
}
