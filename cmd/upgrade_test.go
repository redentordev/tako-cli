package cmd

import (
	"strings"
	"testing"
)

func TestUpgradeServersCommandRegistered(t *testing.T) {
	cmd, _, err := upgradeCmd.Find([]string{"servers"})
	if err != nil {
		t.Fatalf("Find(upgrade servers) returned error: %v", err)
	}
	if cmd != upgradeServersCmd {
		t.Fatalf("upgrade servers command was not registered")
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
