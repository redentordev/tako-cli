package provisioner

import (
	"strings"
	"testing"
)

func TestFirewallAllowCommandsUseConfiguredMeshPort(t *testing.T) {
	commands := strings.Join(firewallAllowCommands(42420), "\n")

	if !strings.Contains(commands, "sudo ufw allow 42420/udp comment 'Tako mesh' || true") {
		t.Fatalf("firewall commands do not use configured mesh port:\n%s", commands)
	}
	if strings.Contains(commands, "sudo ufw allow 51820/udp comment 'Tako mesh' || true") {
		t.Fatalf("firewall commands should not hardcode default mesh port:\n%s", commands)
	}
}

func TestConfigureFirewallRejectsInvalidMeshPort(t *testing.T) {
	provisioner := NewProvisioner(nil, false)

	if err := provisioner.ConfigureFirewall(0); err == nil {
		t.Fatal("expected invalid mesh port to be rejected")
	}
	if err := provisioner.ConfigureFirewall(65536); err == nil {
		t.Fatal("expected oversized mesh port to be rejected")
	}
}

func TestShouldSkipUFWFirewallOnlyForNonDebianHosts(t *testing.T) {
	if shouldSkipUFWFirewall(&OSInfo{Family: OSFamilyDebian}) {
		t.Fatal("Debian-family hosts should still use UFW")
	}
	for _, family := range []OSFamily{OSFamilyRHEL, OSFamilySUSE, OSFamilyAlpine} {
		if !shouldSkipUFWFirewall(&OSInfo{Family: family}) {
			t.Fatalf("family %s should skip UFW when UFW is absent", family)
		}
	}
	if shouldSkipUFWFirewall(nil) {
		t.Fatal("unknown OS should not skip UFW")
	}
}
