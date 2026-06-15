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

func TestFirewallAllowCommandsDoNotThrottleSSH(t *testing.T) {
	commands := strings.Join(firewallAllowCommands(42420), "\n")

	if strings.Contains(commands, "ufw limit 22/tcp comment") {
		t.Fatalf("firewall commands should not rate-limit Tako's SSH control plane:\n%s", commands)
	}
	if !strings.Contains(commands, "sudo ufw --force delete limit 22/tcp || true") {
		t.Fatalf("firewall commands should remove legacy SSH limit rules:\n%s", commands)
	}
	if !strings.Contains(commands, "sudo ufw allow 22/tcp comment 'SSH' || true") {
		t.Fatalf("firewall commands should allow SSH without UFW throttling:\n%s", commands)
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
