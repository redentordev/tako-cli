package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestDestroyCommandDoesNotExposeServerFlag(t *testing.T) {
	if flag := destroyCmd.Flags().Lookup("server"); flag != nil {
		t.Fatal("destroy command should not expose a server flag")
	}
}

func TestDestroyEnvironmentTargetsUsesAllEnvironmentServers(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "a.example.test"},
			"node-b": {Host: "b.example.test"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {Servers: []string{"node-a", "node-b"}},
		},
	}

	servers, names, err := destroyEnvironmentTargets(cfg, "production")
	if err != nil {
		t.Fatalf("destroyEnvironmentTargets returned error: %v", err)
	}
	if strings.Join(names, ",") != "node-a,node-b" {
		t.Fatalf("target names = %v, want node-a,node-b", names)
	}
	if servers["node-a"].Host != "a.example.test" || servers["node-b"].Host != "b.example.test" {
		t.Fatalf("servers = %#v, want both environment servers", servers)
	}
}

func TestDestroyEnvironmentTargetsRejectsMissingServerConfig(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "a.example.test"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {Servers: []string{"node-a", "node-b"}},
		},
	}

	_, _, err := destroyEnvironmentTargets(cfg, "production")
	if err == nil {
		t.Fatal("destroyEnvironmentTargets returned nil, want missing server error")
	}
	if !strings.Contains(err.Error(), "node-b") {
		t.Fatalf("error = %q, want missing node-b context", err)
	}
}
