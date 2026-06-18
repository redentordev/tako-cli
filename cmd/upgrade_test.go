package cmd

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestUpgradeServersCommandRegistered(t *testing.T) {
	cmd, _, err := upgradeCmd.Find([]string{"servers"})
	if err != nil {
		t.Fatalf("Find(upgrade servers) returned error: %v", err)
	}
	if !upgradeCmd.SilenceUsage {
		t.Fatal("upgrade command should silence usage for execution errors")
	}
	if cmd != upgradeServersCmd {
		t.Fatalf("upgrade servers command was not registered")
	}
	if !upgradeServersCmd.SilenceUsage {
		t.Fatal("upgrade servers command should silence usage for execution errors")
	}
	for _, flagName := range []string{"server", "dry-run", "takod-binary"} {
		if flag := upgradeServersCmd.Flags().Lookup(flagName); flag == nil {
			t.Fatalf("upgrade servers missing --%s flag", flagName)
		}
	}
}

func TestValidateUpgradeServersOptionsRequiresBinaryForDevApply(t *testing.T) {
	err := validateUpgradeServersOptions("dev", "", false)
	if err == nil {
		t.Fatal("validateUpgradeServersOptions should reject dev apply without --takod-binary")
	}
	if !strings.Contains(err.Error(), "--takod-binary") {
		t.Fatalf("error = %q, want --takod-binary guidance", err)
	}
}

func TestValidateUpgradeServersOptionsRequiresBinaryForNonReleaseApply(t *testing.T) {
	for _, version := range []string{
		"v0.4.13-1-gabcdef0",
		"v0.4.13-1-gabcdef0-dirty",
		"v0.4.13-dirty",
	} {
		if err := validateUpgradeServersOptions(version, "", false); err == nil {
			t.Fatalf("validateUpgradeServersOptions(%q) should reject non-release apply without --takod-binary", version)
		}
		if err := validateUpgradeServersOptions(version, "/tmp/tako-linux-amd64", false); err != nil {
			t.Fatalf("validateUpgradeServersOptions(%q) should allow explicit --takod-binary: %v", version, err)
		}
	}
}

func TestValidateUpgradeServersOptionsAllowsDevDryRun(t *testing.T) {
	if err := validateUpgradeServersOptions("dev", "", true); err != nil {
		t.Fatalf("dev dry-run should be allowed: %v", err)
	}
}

func TestValidateUpgradeServersOptionsAllowsReleaseApply(t *testing.T) {
	for _, version := range []string{"v0.4.5", "v0.5.0-rc.1"} {
		if err := validateUpgradeServersOptions(version, "", false); err != nil {
			t.Fatalf("release apply should be allowed for %q: %v", version, err)
		}
	}
}

func TestIsGitDescribeSnapshot(t *testing.T) {
	tests := map[string]bool{
		"v0.4.13":                  false,
		"v0.5.0-rc.1":              false,
		"v0.4.13-1-gabcdef0":       true,
		"v0.4.13-12-gABCDEF0":      true,
		"v0.5.0-rc.1-2-gabcdef0":   true,
		"v0.4.13-dirty":            true,
		"v0.4.13-1-gabcdef0-dirty": true,
	}
	for version, want := range tests {
		if got := isGitDescribeSnapshot(version); got != want {
			t.Fatalf("isGitDescribeSnapshot(%q) = %v, want %v", version, got, want)
		}
	}
}

func TestWaitForTakodAgentVersionRetriesUntilTargetVersion(t *testing.T) {
	client := &upgradeStatusExecutor{
		outputs: []string{
			`{"runtime":"takod","version":"v0.4.40"}` + "\n__TAKO_HTTP_STATUS__:200",
			`{"runtime":"takod","version":"v0.4.41"}` + "\n__TAKO_HTTP_STATUS__:200",
		},
	}

	status, err := waitForTakodAgentVersion(client, &config.Config{}, "v0.4.41", 100*time.Millisecond, time.Millisecond, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("waitForTakodAgentVersion returned error: %v", err)
	}
	if status.Version != "v0.4.41" {
		t.Fatalf("verified version = %q, want target version", status.Version)
	}
	if client.contextCalls != 2 {
		t.Fatalf("status probes = %d, want stale then target", client.contextCalls)
	}
	if !strings.Contains(client.commands[0], "/v1/status") {
		t.Fatalf("status probe command = %q, want /v1/status request", client.commands[0])
	}
}

func TestWaitForTakodAgentVersionReportsLastVersion(t *testing.T) {
	client := &upgradeStatusExecutor{
		outputs: []string{
			`{"runtime":"takod","version":"v0.4.40"}` + "\n__TAKO_HTTP_STATUS__:200",
		},
	}

	_, err := waitForTakodAgentVersion(client, &config.Config{}, "v0.4.41", time.Millisecond, time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("waitForTakodAgentVersion should fail when status never reports the target version")
	}
	for _, want := range []string{"expected version v0.4.41", "last v0.4.40"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	}
}

func TestWaitForTakodAgentVersionReportsUnavailableStatus(t *testing.T) {
	client := &upgradeStatusExecutor{err: errors.New("socket unavailable")}

	_, err := waitForTakodAgentVersion(client, &config.Config{}, "v0.4.41", time.Millisecond, time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("waitForTakodAgentVersion should fail when status is unavailable")
	}
	for _, want := range []string{"expected version v0.4.41", "status unavailable", "socket unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	}
}

type upgradeStatusExecutor struct {
	outputs      []string
	err          error
	contextCalls int
	inputCalls   int
	commands     []string
}

func (u *upgradeStatusExecutor) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
	u.contextCalls++
	u.commands = append(u.commands, cmd)
	return u.next()
}

func (u *upgradeStatusExecutor) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
	u.inputCalls++
	u.commands = append(u.commands, cmd)
	return u.next()
}

func (u *upgradeStatusExecutor) next() (string, error) {
	if u.err != nil {
		return "", u.err
	}
	if len(u.outputs) == 0 {
		return `{"runtime":"takod","version":"v0.4.40"}` + "\n__TAKO_HTTP_STATUS__:200", nil
	}
	output := u.outputs[0]
	if len(u.outputs) > 1 {
		u.outputs = u.outputs[1:]
	}
	return output, nil
}
