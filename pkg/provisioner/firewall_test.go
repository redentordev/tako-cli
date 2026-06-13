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
