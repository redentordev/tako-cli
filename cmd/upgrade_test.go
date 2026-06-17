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

func TestValidateUpgradeServersOptionsAllowsDevDryRun(t *testing.T) {
	if err := validateUpgradeServersOptions("dev", "", true); err != nil {
		t.Fatalf("dev dry-run should be allowed: %v", err)
	}
}

func TestValidateUpgradeServersOptionsAllowsReleaseApply(t *testing.T) {
	if err := validateUpgradeServersOptions("v0.4.5", "", false); err != nil {
		t.Fatalf("release apply should be allowed: %v", err)
	}
}
