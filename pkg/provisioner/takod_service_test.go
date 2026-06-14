package provisioner

import (
	"strings"
	"testing"
)

func TestSystemdPathArg(t *testing.T) {
	path, err := systemdPathArg("", "/run/tako/takod.sock")
	if err != nil {
		t.Fatalf("systemdPathArg returned error: %v", err)
	}
	if path != "/run/tako/takod.sock" {
		t.Fatalf("unexpected fallback path %q", path)
	}

	for _, value := range []string{"relative/path", "/run/tako/bad path.sock", "/run/tako/bad\npath.sock"} {
		if _, err := systemdPathArg(value, ""); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestSystemdIdentifierArg(t *testing.T) {
	for _, value := range []string{"node-a", "node_a", "Node1"} {
		got, err := systemdIdentifierArg(value)
		if err != nil {
			t.Fatalf("systemdIdentifierArg(%q) returned error: %v", value, err)
		}
		if got != value {
			t.Fatalf("systemdIdentifierArg(%q) = %q, want %q", value, got, value)
		}
	}

	for _, value := range []string{"", "../node", "bad node", "bad\nnode", "bad/node"} {
		if _, err := systemdIdentifierArg(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestNormalizeLinuxArch(t *testing.T) {
	tests := map[string]string{
		"x86_64":  "amd64",
		"amd64":   "amd64",
		"aarch64": "arm64",
		"arm64":   "arm64",
	}

	for input, want := range tests {
		got, err := normalizeLinuxArch(input)
		if err != nil {
			t.Fatalf("normalizeLinuxArch(%q) returned error: %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeLinuxArch(%q) = %q, want %q", input, got, want)
		}
	}

	if _, err := normalizeLinuxArch("i386"); err == nil {
		t.Fatalf("expected unsupported architecture to be rejected")
	}
}

func TestReleaseVersionArg(t *testing.T) {
	for _, input := range []string{"v1.2.3", "1.2.3", "v1.2.3-beta.1"} {
		got, err := releaseVersionArg(input)
		if err != nil {
			t.Fatalf("releaseVersionArg(%q) returned error: %v", input, err)
		}
		if got != input {
			t.Fatalf("releaseVersionArg(%q) = %q, want %q", input, got, input)
		}
	}

	for _, input := range []string{"", "dev", "unknown", "v1.2.3;rm -rf /", "v1.2.3/foo", "v1.2.3\n"} {
		if _, err := releaseVersionArg(input); err == nil {
			t.Fatalf("expected %q to be rejected", input)
		}
	}
}

func TestInstallTakodBinaryFromFileRejectsNonRegularPath(t *testing.T) {
	provisioner := NewProvisioner(nil, false)

	err := provisioner.InstallTakodBinaryFromFile(t.TempDir())
	if err == nil {
		t.Fatal("InstallTakodBinaryFromFile should reject directories")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error = %q, want regular file context", err)
	}
}

func TestTakodBinaryPathCommandDoesNotEchoFallbackAfterCommandVSuccess(t *testing.T) {
	command := takodBinaryPathCommand()
	if !strings.Contains(command, "{ test -x /usr/local/bin/tako && echo /usr/local/bin/tako; }") {
		t.Fatalf("binary path command should group fallback branch:\n%s", command)
	}
	if strings.Contains(command, "command -v tako 2>/dev/null || test -x /usr/local/bin/tako && echo") {
		t.Fatalf("binary path command has ambiguous shell precedence:\n%s", command)
	}
}

func TestTakodSystemdUnitGrantsTakoGroupSocketAccess(t *testing.T) {
	unit := buildTakodSystemdUnit("/usr/local/bin/tako", "/run/tako/takod.sock", "/var/lib/tako", "node-a", "30s")
	for _, required := range []string{
		"User=root",
		"Group=tako",
		"RuntimeDirectory=tako",
		"RuntimeDirectoryMode=0770",
		"UMask=0007",
		"Requires=docker.service",
		"ExecStart=/usr/local/bin/tako takod run --socket /run/tako/takod.sock --data-dir /var/lib/tako --node node-a --actual-refresh-interval 30s",
	} {
		if !strings.Contains(unit, required) {
			t.Fatalf("systemd unit is missing %q:\n%s", required, unit)
		}
	}
}

func TestDeployUserCommandsQuoteArguments(t *testing.T) {
	username := "deploy-user"
	tests := map[string]string{
		"id":     buildUserIDCommand(username),
		"create": buildUserCreateCommand(username),
		"access": buildTakodAccessCommand(username),
	}

	want := map[string]string{
		"id":     "id -u 'deploy-user'",
		"create": "sudo useradd -m -s /bin/bash 'deploy-user'",
		"access": "sudo usermod -aG 'tako' 'deploy-user'",
	}

	for name, got := range tests {
		if got != want[name] {
			t.Fatalf("%s command = %q, want %q", name, got, want[name])
		}
	}
}

func TestBootstrapScriptsAvoidDownloadedShellInstallers(t *testing.T) {
	for name, script := range map[string]string{
		"base packages": basePackageInstallScript(),
		"docker":        dockerInstallScript(),
	} {
		for _, disallowed := range []string{"get.docker.com", "curl |", "curl -sSL", "wget |"} {
			if strings.Contains(script, disallowed) {
				t.Fatalf("%s script contains disallowed installer pattern %q:\n%s", name, disallowed, script)
			}
		}
	}
}

func TestSecurityHardeningScriptsAreNonInteractive(t *testing.T) {
	install := securityPackagesInstallScript()
	if !strings.Contains(install, "DEBIAN_FRONTEND=noninteractive") {
		t.Fatalf("security package install script should force noninteractive apt:\n%s", install)
	}
	if strings.Contains(install, "dpkg-reconfigure") {
		t.Fatalf("security package install script must not run interactive dpkg-reconfigure:\n%s", install)
	}

	unattended := unattendedUpgradesConfigScript()
	for _, required := range []string{
		"/etc/apt/apt.conf.d/20auto-upgrades",
		`APT::Periodic::Update-Package-Lists "1";`,
		`APT::Periodic::Unattended-Upgrade "1";`,
	} {
		if !strings.Contains(unattended, required) {
			t.Fatalf("unattended upgrades script is missing %q:\n%s", required, unattended)
		}
	}
	if strings.Contains(unattended, "dpkg-reconfigure") {
		t.Fatalf("unattended upgrades script must not run interactive dpkg-reconfigure:\n%s", unattended)
	}
}

func TestFail2BanJailConfigIgnoresActiveSSHClientIP(t *testing.T) {
	config := fail2banSSHDJailConfig("203.0.113.42")
	for _, required := range []string{
		"[sshd]",
		"maxretry = 5",
		"bantime = 3600",
		"ignoreip = 127.0.0.1/8 ::1 203.0.113.42",
	} {
		if !strings.Contains(config, required) {
			t.Fatalf("fail2ban config is missing %q:\n%s", required, config)
		}
	}
}

func TestFail2BanJailConfigRejectsInvalidIgnoreIP(t *testing.T) {
	config := fail2banSSHDJailConfig("203.0.113.42; bad")
	if strings.Contains(config, "ignoreip") || strings.Contains(config, "bad") {
		t.Fatalf("fail2ban config should not include unsafe client IP:\n%s", config)
	}
}

func TestTakodBinaryInstallScriptVerifiesReleaseChecksum(t *testing.T) {
	script := takodBinaryInstallScript("v1.2.3", "tako-linux-amd64")

	for _, required := range []string{
		"https://github.com/redentordev/tako-cli/releases/download/v1.2.3/tako-linux-amd64",
		"https://github.com/redentordev/tako-cli/releases/download/v1.2.3/checksums.txt",
		"calc_sha256()",
		"sha256sum \"$1\"",
		"shasum -a 256 \"$1\"",
		"awk -v name='tako-linux-amd64'",
		"checksum mismatch for tako-linux-amd64",
		"install -m 0755 \"$tmp\" /usr/local/bin/tako",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("install script is missing %q:\n%s", required, script)
		}
	}
}

func TestTakodBinaryInstallScriptFailsClosedOnChecksumProblems(t *testing.T) {
	script := takodBinaryInstallScript("v1.2.3", "tako-linux-amd64")

	for _, required := range []string{
		"sha256sum or shasum is required to verify takod binary",
		"checksum for tako-linux-amd64 not found in checksums.txt",
		"exit 1",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("install script is missing fail-closed behavior %q:\n%s", required, script)
		}
	}

	for _, disallowed := range []string{
		"skipping checksum",
		"skip checksum",
		"checksum not found in checksums file, skipping",
	} {
		if strings.Contains(script, disallowed) {
			t.Fatalf("install script contains disallowed best-effort verification text %q:\n%s", disallowed, script)
		}
	}
}
