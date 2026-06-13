package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
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
