package unregistry

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestBuildTakodImageTransferCommandsUseTakodEndpoints(t *testing.T) {
	exportCmd := buildTakodImageExportCommand("/run/tako/takod.sock", "demo/web:abc123")
	importCmd := buildTakodImageImportCommand("/run/tako/takod.sock", "demo/web:abc123")

	for _, cmd := range []string{exportCmd, importCmd} {
		for _, expected := range []string{
			"--unix-socket '/run/tako/takod.sock'",
		} {
			if !strings.Contains(cmd, expected) {
				t.Fatalf("transfer command missing %q: %s", expected, cmd)
			}
		}
		for _, unexpected := range []string{"ssh", "docker save", "docker load", "docker image"} {
			if strings.Contains(cmd, unexpected) {
				t.Fatalf("transfer command should not run %q directly: %s", unexpected, cmd)
			}
		}
	}

	if !strings.Contains(exportCmd, "/v1/images/export?image=demo%2Fweb%3Aabc123") {
		t.Fatalf("export command missing export endpoint: %s", exportCmd)
	}
	if strings.Contains(exportCmd, "--data-binary @-") {
		t.Fatalf("export command should not send stdin: %s", exportCmd)
	}
	if !strings.Contains(importCmd, "/v1/images/import?image=demo%2Fweb%3Aabc123") {
		t.Fatalf("import command missing import endpoint: %s", importCmd)
	}
	for _, expected := range []string{"--http1.1", "-H 'Transfer-Encoding: chunked'", "--upload-file -"} {
		if !strings.Contains(importCmd, expected) {
			t.Fatalf("import command should stream stdin with %q: %s", expected, importCmd)
		}
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
