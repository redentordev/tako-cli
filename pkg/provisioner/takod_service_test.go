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
		"id":      buildUserIDCommand(username),
		"create":  buildUserCreateCommand(username),
		"access":  buildTakodAccessCommand(username),
		"cleanup": buildLegacySudoersRemoveCommand(username),
	}

	want := map[string]string{
		"id":      "id -u 'deploy-user'",
		"create":  "sudo useradd -m -s /bin/bash 'deploy-user'",
		"access":  "sudo usermod -aG 'tako' 'deploy-user'",
		"cleanup": "sudo rm -f -- '/etc/sudoers.d/tako-deploy-user'",
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
