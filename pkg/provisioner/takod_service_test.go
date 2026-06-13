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
