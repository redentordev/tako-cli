package provisioner

import (
	"slices"
	"strings"
	"testing"
)

func TestFirewallAllowCommandsUseConfiguredMeshPort(t *testing.T) {
	commands := strings.Join(firewallAllowCommands(42420, []int{22}), "\n")

	if !strings.Contains(commands, "sudo ufw allow 42420/udp comment 'Tako mesh' || true") {
		t.Fatalf("firewall commands do not use configured mesh port:\n%s", commands)
	}
	if strings.Contains(commands, "sudo ufw allow 51820/udp comment 'Tako mesh' || true") {
		t.Fatalf("firewall commands should not hardcode default mesh port:\n%s", commands)
	}
	if !strings.Contains(commands, "sudo ufw allow 443/udp comment 'HTTPS HTTP/3' || true") {
		t.Fatalf("firewall commands should allow UDP 443 for HTTP/3:\n%s", commands)
	}
}

func TestFirewallAllowCommandsDoNotRateLimitSSH(t *testing.T) {
	commands := strings.Join(firewallAllowCommands(51820, nil), "\n")

	if strings.Contains(commands, "ufw limit") {
		t.Fatalf("ufw limit locks Tako out during connection bursts:\n%s", commands)
	}
	if !strings.Contains(commands, "sudo ufw allow 22/tcp comment 'SSH (fail2ban protected)'") {
		t.Fatalf("firewall commands must allow SSH:\n%s", commands)
	}
}

func TestFirewallAllowCommandsAllowCustomSSHPortWithoutMaskingErrors(t *testing.T) {
	commands := firewallAllowCommands(51820, []int{2222, 22})

	want := []string{
		"sudo ufw allow 22/tcp comment 'SSH (fail2ban protected)'",
		"sudo ufw allow 2222/tcp comment 'SSH (fail2ban protected)'",
	}
	if !slices.Equal(commands[:2], want) {
		t.Fatalf("SSH commands = %#v, want %#v", commands[:2], want)
	}
	for _, cmd := range sshFirewallAllowCommands([]int{22, 2222}) {
		if strings.Contains(cmd, "|| true") {
			t.Fatalf("SSH allow command must not mask errors: %s", cmd)
		}
	}
}

func TestFirewallAllowedPortsIncludeSSHPortSet(t *testing.T) {
	got := FirewallAllowedPorts(51820, []int{2222})
	want := []string{"22/tcp", "2222/tcp", "80/tcp", "443/tcp", "443/udp", "51820/udp"}
	if !slices.Equal(got, want) {
		t.Fatalf("FirewallAllowedPorts = %#v, want %#v", got, want)
	}
}

func TestConfigureFirewallRejectsInvalidMeshPort(t *testing.T) {
	provisioner := NewProvisioner(nil, false)

	if _, err := provisioner.ConfigureFirewall(FirewallOptions{MeshListenPort: 0}); err == nil {
		t.Fatal("expected invalid mesh port to be rejected")
	}
	if _, err := provisioner.ConfigureFirewall(FirewallOptions{MeshListenPort: 65536}); err == nil {
		t.Fatal("expected oversized mesh port to be rejected")
	}
	if _, err := provisioner.ConfigureFirewall(FirewallOptions{MeshListenPort: 51820, SSHPorts: []int{70000}}); err == nil {
		t.Fatal("expected invalid SSH port to be rejected")
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

func TestParseSSHConnectionServerPort(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{"default", "203.0.113.9 51234 10.0.0.5 22", 22},
		{"custom", "203.0.113.9 51234 10.0.0.5 2222", 2222},
		{"ipv6", "2001:db8::9 51234 2001:db8::5 2200", 2200},
		{"trailing newline", "203.0.113.9 51234 10.0.0.5 22\n", 22},
		{"empty", "", 0},
		{"truncated", "203.0.113.9 51234 10.0.0.5", 0},
		{"non-numeric", "203.0.113.9 51234 10.0.0.5 ssh", 0},
		{"out of range", "203.0.113.9 51234 10.0.0.5 70000", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseSSHConnectionServerPort(tt.value); got != tt.want {
				t.Fatalf("parseSSHConnectionServerPort(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}

func TestSSHPortSet(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		live       int
		want       []int
	}{
		{"default port", 22, 22, []int{22}},
		{"unset configured port", 0, 22, []int{22}},
		{"custom port", 2222, 2222, []int{22, 2222}},
		{"nat mismatch", 2200, 22, []int{22, 2200}},
		{"nat mismatch both custom", 40022, 2222, []int{22, 2222, 40022}},
		{"empty SSH_CONNECTION", 2222, 0, []int{22, 2222}},
		{"invalid live port", 2222, -1, []int{22, 2222}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sshPortSet(tt.configured, tt.live); !slices.Equal(got, tt.want) {
				t.Fatalf("sshPortSet(%d, %d) = %v, want %v", tt.configured, tt.live, got, tt.want)
			}
		})
	}
}

const ufwShowAddedSample = `Added user rules (see 'ufw status' for running firewall):
ufw allow 22/tcp comment 'SSH (fail2ban protected)'
ufw allow 2200/tcp comment 'SSH (fail2ban protected)'
ufw allow 80/tcp comment 'HTTP'
ufw allow 443/tcp comment 'HTTPS'
ufw allow 443/udp comment 'HTTPS HTTP/3'
ufw allow 51820/udp comment 'Tako mesh'
ufw allow 8080
ufw allow from 10.0.0.0/8 to any port 2222 proto tcp
ufw allow from 2001:db8::/32 to any port 3000 proto tcp
ufw allow in on wg0 to any port 9000
ufw limit 2022/tcp
ufw allow OpenSSH
`

func TestParseUFWAddedRules(t *testing.T) {
	rules := parseUFWAddedRules(ufwShowAddedSample)
	if len(rules) != 12 {
		t.Fatalf("parsed %d rules, want 12: %#v", len(rules), rules)
	}
	check := func(i int, want ufwAddedRule) {
		t.Helper()
		if rules[i] != want {
			t.Fatalf("rule %d = %#v, want %#v", i, rules[i], want)
		}
	}
	check(0, ufwAddedRule{Action: "allow", Port: 22, Proto: "tcp", Comment: sshFirewallComment, AnySource: true})
	check(5, ufwAddedRule{Action: "allow", Port: 51820, Proto: "udp", Comment: "Tako mesh", AnySource: true})
	check(6, ufwAddedRule{Action: "allow", Port: 8080, AnySource: true})
	check(7, ufwAddedRule{Action: "allow", Port: 2222, Proto: "tcp", AnySource: false})
	check(8, ufwAddedRule{Action: "allow", Port: 3000, Proto: "tcp", AnySource: false})
	check(9, ufwAddedRule{Action: "allow", Port: 9000, AnySource: false})
	check(10, ufwAddedRule{Action: "limit", Port: 2022, Proto: "tcp", AnySource: true})
	check(11, ufwAddedRule{Action: "allow", AnySource: true})
}

func TestParseUFWAddedRulesEmpty(t *testing.T) {
	rules := parseUFWAddedRules("Added user rules (see 'ufw status' for running firewall):\n(None)\n")
	if len(rules) != 0 {
		t.Fatalf("rules = %#v, want none", rules)
	}
	if missing := missingSSHPorts(rules, []int{22}); !slices.Equal(missing, []int{22}) {
		t.Fatalf("missing = %v, want [22]", missing)
	}
}

func TestMissingSSHPorts(t *testing.T) {
	rules := parseUFWAddedRules(ufwShowAddedSample)

	if missing := missingSSHPorts(rules, []int{22, 2200}); len(missing) != 0 {
		t.Fatalf("missing = %v, want none", missing)
	}
	// 2222 is only allowed from a private range, 2022 only rate-limited, and
	// 8080 with no protocol counts as tcp+udp.
	if missing := missingSSHPorts(rules, []int{22, 2022, 2222, 8080}); !slices.Equal(missing, []int{2022, 2222}) {
		t.Fatalf("missing = %v, want [2022 2222]", missing)
	}
	// A UDP-only rule does not admit SSH.
	if missing := missingSSHPorts(rules, []int{51820}); !slices.Equal(missing, []int{51820}) {
		t.Fatalf("missing = %v, want [51820]", missing)
	}
}

func TestStaleSSHPorts(t *testing.T) {
	rules := parseUFWAddedRules(ufwShowAddedSample)

	if stale := staleSSHPorts(rules, []int{22, 2222}); !slices.Equal(stale, []int{2200}) {
		t.Fatalf("stale = %v, want [2200]", stale)
	}
	if stale := staleSSHPorts(rules, []int{22, 2200}); len(stale) != 0 {
		t.Fatalf("stale = %v, want none", stale)
	}
}

func TestSkippedFirewallReportAndWarnings(t *testing.T) {
	report := skippedFirewallReport("Rocky Linux 9.4", parseNativeFirewall("active\n"), 51820, []int{22, 2222})

	if report.Backend != FirewallBackendNone || report.Status != FirewallStatusSkipped {
		t.Fatalf("report = %+v, want none/skipped", report)
	}
	if !strings.Contains(report.Reason, "UFW is not available on Rocky Linux 9.4") {
		t.Fatalf("reason = %q", report.Reason)
	}
	if len(report.AllowedPorts) != 0 {
		t.Fatalf("skipped report must not list applied ports: %v", report.AllowedPorts)
	}
	if report.NativeFirewall != NativeFirewallFirewalld {
		t.Fatalf("native firewall = %q, want firewalld", report.NativeFirewall)
	}

	warnings := report.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %#v, want 1", warnings)
	}
	for _, want := range []string{"did not configure a host firewall", "firewalld is active and was not modified by Tako", "22/tcp, 2222/tcp, 80/tcp, 443/tcp, 443/udp, 51820/udp"} {
		if !strings.Contains(warnings[0], want) {
			t.Fatalf("warning %q missing %q", warnings[0], want)
		}
	}

	inactive := skippedFirewallReport("Alpine 3.20", parseNativeFirewall("inactive"), 51820, []int{22})
	if inactive.NativeFirewall != "" || strings.Contains(inactive.Warnings()[0], "firewalld") {
		t.Fatalf("inactive firewalld must not be reported: %+v", inactive)
	}
}

func TestStaleSSHPortWarningsIncludeRemovalCommand(t *testing.T) {
	report := &FirewallReport{Backend: FirewallBackendUFW, Status: FirewallStatusConfigured, StaleSSHPorts: []int{2200}}
	warnings := report.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "2200/tcp") || !strings.Contains(warnings[0], "sudo ufw delete allow 2200/tcp") {
		t.Fatalf("warnings = %#v", warnings)
	}
}
