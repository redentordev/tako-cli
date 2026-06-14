package cmd

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
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

func TestDestroySingleServerUsesProvidedPool(t *testing.T) {
	provider := &fakeSSHClientProvider{}
	server := config.ServerConfig{
		Host:     "node-a.example.test",
		Port:     2222,
		User:     "deploy",
		SSHKey:   "/tmp/id_ed25519",
		Password: "fallback",
	}
	var steps []string

	err := destroySingleServerWithHooks(provider, "node-a", server, &config.Config{}, "production", true, false,
		func(client *ssh.Client, cfg *config.Config, envName string, verbose bool) error {
			steps = append(steps, "decommission")
			if envName != "production" || !verbose {
				t.Fatalf("decommission env=%q verbose=%v", envName, verbose)
			}
			return nil
		},
		func(client *ssh.Client, cfg *config.Config, envName string, verbose bool) error {
			steps = append(steps, "purge")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("destroySingleServerWithHooks returned error: %v", err)
	}
	if !slices.Equal(steps, []string{"decommission"}) {
		t.Fatalf("steps = %#v", steps)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("pool requests = %#v, want one", provider.requests)
	}
	got := provider.requests[0]
	if got.host != server.Host || got.port != server.Port || got.user != server.User || got.sshKey != server.SSHKey || got.password != server.Password {
		t.Fatalf("pool request = %#v, want server config", got)
	}
}

func TestDestroySingleServerPurgesAfterDecommissionWhenRequested(t *testing.T) {
	provider := &fakeSSHClientProvider{}
	var steps []string

	err := destroySingleServerWithHooks(provider, "node-a", config.ServerConfig{Host: "node-a.example.test"}, &config.Config{}, "production", false, true,
		func(client *ssh.Client, cfg *config.Config, envName string, verbose bool) error {
			steps = append(steps, "decommission")
			return nil
		},
		func(client *ssh.Client, cfg *config.Config, envName string, verbose bool) error {
			if envName != "production" {
				t.Fatalf("purge env=%q, want production", envName)
			}
			steps = append(steps, "purge")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("destroySingleServerWithHooks returned error: %v", err)
	}
	if !slices.Equal(steps, []string{"decommission", "purge"}) {
		t.Fatalf("steps = %#v", steps)
	}
}

func TestDestroySingleServerDoesNotPurgeAfterDecommissionFailure(t *testing.T) {
	provider := &fakeSSHClientProvider{}
	purged := false

	err := destroySingleServerWithHooks(provider, "node-a", config.ServerConfig{Host: "node-a.example.test"}, &config.Config{}, "production", false, true,
		func(client *ssh.Client, cfg *config.Config, envName string, verbose bool) error {
			return errors.New("cleanup failed")
		},
		func(client *ssh.Client, cfg *config.Config, envName string, verbose bool) error {
			purged = true
			return nil
		},
	)
	if err == nil {
		t.Fatal("destroySingleServerWithHooks returned nil, want decommission error")
	}
	if purged {
		t.Fatal("purge should not run after decommission failure")
	}
	if !strings.Contains(err.Error(), "decommission failed: cleanup failed") {
		t.Fatalf("error = %q", err)
	}
}
