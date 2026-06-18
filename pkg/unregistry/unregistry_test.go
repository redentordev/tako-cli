package unregistry

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestTakodImageStreamCommandsUseTakodEndpoints(t *testing.T) {
	exportCmd := takodImageExportCommand("/run/tako/takod.sock", "demo/web:abc123")
	importCmd := takodImageImportCommand("/run/tako/takod.sock", "demo/web:abc123")

	for _, expected := range []string{
		"--unix-socket '/run/tako/takod.sock'",
		"/v1/images/export?image=demo%2Fweb%3Aabc123",
		"-X 'GET'",
	} {
		if !strings.Contains(exportCmd, expected) {
			t.Fatalf("export command missing %q: %s", expected, exportCmd)
		}
	}
	for _, expected := range []string{
		"--unix-socket '/run/tako/takod.sock'",
		"/v1/images/import?image=demo%2Fweb%3Aabc123",
		"-X 'POST'",
		"--upload-file -",
	} {
		if !strings.Contains(importCmd, expected) {
			t.Fatalf("import command missing %q: %s", expected, importCmd)
		}
	}
	for _, cmd := range []string{exportCmd, importCmd} {
		for _, unexpected := range []string{"docker save", "docker load", "docker image", " ssh ", "StrictHostKeyChecking", "/home/deploy/.ssh", "--data-binary"} {
			if strings.Contains(cmd, unexpected) {
				t.Fatalf("stream command should not contain %q: %s", unexpected, cmd)
			}
		}
	}
}

func TestReadLimitedStreamTextCapturesPrefixAndDrainsReader(t *testing.T) {
	reader := strings.NewReader("abcdefghijklmnopqrstuvwxyz")

	got := readLimitedStreamText(reader, 5)
	if got != "abcde" {
		t.Fatalf("captured text = %q, want prefix", got)
	}
	if rest := reader.Len(); rest != 0 {
		t.Fatalf("reader still has %d byte(s), want drained", rest)
	}
}

func TestUnregistryPeerServersExcludeSourceHost(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
			"node-b": {Host: "10.0.0.2"},
			"node-c": {Host: "10.0.0.3"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b", "node-c"},
			},
		},
	}

	peers, err := unregistryPeerServers(cfg, "production", "10.0.0.2")
	if err != nil {
		t.Fatalf("unregistryPeerServers returned error: %v", err)
	}

	want := []string{"node-a", "node-c"}
	if len(peers) != len(want) {
		t.Fatalf("peers = %v, want %v", peers, want)
	}
	for i := range want {
		if peers[i] != want[i] {
			t.Fatalf("peers = %v, want %v", peers, want)
		}
	}
}

func TestUnregistryPeerServersReportsMissingEnvironmentServer(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "missing"},
			},
		},
	}

	if _, err := unregistryPeerServers(cfg, "production", ""); err == nil {
		t.Fatal("unregistryPeerServers should reject a missing environment server")
	}
}
