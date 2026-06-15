package cmd

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/spf13/cobra"
)

func TestResolveInternalServerSSHUsesEnvironmentServer(t *testing.T) {
	cfg := internalResolverConfig()

	resolved, err := resolveInternalServerSSH(cfg, "production", "node-b")
	if err != nil {
		t.Fatalf("resolveInternalServerSSH returned error: %v", err)
	}

	if resolved.Host != "10.0.0.2" {
		t.Fatalf("host = %q, want node-b host", resolved.Host)
	}
	if resolved.User != "deploy" {
		t.Fatalf("user = %q, want deploy", resolved.User)
	}
	if resolved.Port != 2222 {
		t.Fatalf("port = %d, want 2222", resolved.Port)
	}
	if resolved.SSHKey != "/tmp/node-b-key" {
		t.Fatalf("ssh key = %q, want /tmp/node-b-key", resolved.SSHKey)
	}
}

func TestResolveInternalServerSSHRejectsServerOutsideEnvironment(t *testing.T) {
	cfg := internalResolverConfig()

	if _, err := resolveInternalServerSSH(cfg, "production", "node-c"); err == nil {
		t.Fatal("resolveInternalServerSSH should reject a server outside the environment")
	}
}

func TestInternalE2EServerSSHCommandIsHidden(t *testing.T) {
	if !internalCmd.Hidden {
		t.Fatal("internal command should be hidden")
	}
	if !internalE2EServerSSHCmd.Hidden {
		t.Fatal("internal E2E resolver command should be hidden")
	}
}

func TestPrintInternalServerSSHOutputShape(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	printInternalServerSSH(cmd, internalServerSSH{
		Host:   "10.0.0.2",
		User:   "deploy",
		Port:   2222,
		SSHKey: "/tmp/node-b-key",
	})

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	want := []string{"10.0.0.2", "deploy", "2222", "/tmp/node-b-key"}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines = %#v, want %#v", lines, want)
	}
}

func internalResolverConfig() *config.Config {
	return &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {
				Host:   "10.0.0.1",
				User:   "deploy",
				Port:   22,
				SSHKey: "/tmp/node-a-key",
			},
			"node-b": {
				Host:   "10.0.0.2",
				User:   "deploy",
				Port:   2222,
				SSHKey: "/tmp/node-b-key",
			},
			"node-c": {
				Host:   "10.0.0.3",
				User:   "deploy",
				Port:   22,
				SSHKey: "/tmp/node-c-key",
			},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b"},
			},
		},
	}
}
