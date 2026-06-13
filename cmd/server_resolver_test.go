package cmd

import (
	"fmt"
	"slices"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

func TestResolveServerUsesFirstEnvironmentNodeByDefault(t *testing.T) {
	cfg := resolverConfig()

	name, server, err := resolveServer(cfg, "production", "")
	if err != nil {
		t.Fatalf("resolveServer returned error: %v", err)
	}
	if name != "node-a" {
		t.Fatalf("server name = %q, want node-a", name)
	}
	if server.Host != "10.0.0.1" {
		t.Fatalf("server host = %q, want 10.0.0.1", server.Host)
	}
}

func TestResolveServerRequiresRequestedServerInEnvironment(t *testing.T) {
	cfg := resolverConfig()

	if _, _, err := resolveServer(cfg, "production", "node-c"); err == nil {
		t.Fatal("resolveServer should reject a server outside the environment")
	}
}

func TestConnectResolvedServerUsesFirstReachableNodeByDefault(t *testing.T) {
	cfg := resolverConfig()
	var attempts []string

	name, server, client, err := connectResolvedServerWith(cfg, "production", "", func(serverName string, _ config.ServerConfig) (*ssh.Client, error) {
		attempts = append(attempts, serverName)
		if serverName == "node-a" {
			return nil, fmt.Errorf("connection refused")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("connectResolvedServerWith returned error: %v", err)
	}
	if name != "node-b" {
		t.Fatalf("server name = %q, want node-b", name)
	}
	if server.Host != "10.0.0.2" {
		t.Fatalf("server host = %q, want 10.0.0.2", server.Host)
	}
	if client != nil {
		t.Fatalf("test connector should return nil client, got %#v", client)
	}
	if !slices.Equal(attempts, []string{"node-a", "node-b"}) {
		t.Fatalf("attempts = %#v, want node-a then node-b", attempts)
	}
}

func TestConnectResolvedServerHonorsRequestedNode(t *testing.T) {
	cfg := resolverConfig()
	var attempts []string

	name, _, _, err := connectResolvedServerWith(cfg, "production", "node-b", func(serverName string, _ config.ServerConfig) (*ssh.Client, error) {
		attempts = append(attempts, serverName)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("connectResolvedServerWith returned error: %v", err)
	}
	if name != "node-b" {
		t.Fatalf("server name = %q, want node-b", name)
	}
	if !slices.Equal(attempts, []string{"node-b"}) {
		t.Fatalf("attempts = %#v, want only requested node", attempts)
	}
}

func TestConnectResolvedServerReportsUnreachableNodes(t *testing.T) {
	cfg := resolverConfig()

	if _, _, _, err := connectResolvedServerWith(cfg, "production", "", func(serverName string, _ config.ServerConfig) (*ssh.Client, error) {
		return nil, fmt.Errorf("%s refused", serverName)
	}); err == nil {
		t.Fatal("connectResolvedServerWith should fail when all nodes are unreachable")
	}
}

func resolverConfig() *config.Config {
	return &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
			"node-b": {Host: "10.0.0.2"},
			"node-c": {Host: "10.0.0.3"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b"},
			},
		},
	}
}
