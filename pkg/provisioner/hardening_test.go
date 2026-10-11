package provisioner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSSHDHardeningScriptBacksUpBeforeEditsAndValidatesBeforeExit(t *testing.T) {
	script := sshdHardeningScript(sshdHardeningOptions{DisablePasswordAuth: true})

	backup := strings.Index(script, `cp -p "$cfg" "$backup"`)
	firstEdit := strings.Index(script, "sed -i")
	validate := strings.Index(script, `"$sshd_bin" -t`)
	restore := strings.Index(script, `cp -p "$backup" "$cfg"`)
	exitZero := strings.LastIndex(script, "exit 0")

	if backup < 0 || firstEdit < 0 || validate < 0 || restore < 0 || exitZero < 0 {
		t.Fatalf("script is missing backup/edit/validate/restore/exit steps:\n%s", script)
	}
	if backup > firstEdit {
		t.Fatalf("backup must precede the first edit:\n%s", script)
	}
	if validate > exitZero || restore > exitZero {
		t.Fatalf("sshd -t and restore must precede exit 0:\n%s", script)
	}
	if !strings.HasPrefix(script, "set -eu") {
		t.Fatalf("script must run with set -eu:\n%s", script)
	}
	if !strings.Contains(script, "command -v sshd || true") || !strings.Contains(script, "sshd_bin=/usr/sbin/sshd") {
		t.Fatalf("script must resolve sshd with a /usr/sbin fallback:\n%s", script)
	}
	if strings.Contains(script, "systemctl") {
		t.Fatalf("the validation script must not reload or restart sshd:\n%s", script)
	}
	for _, tuning := range []string{"MaxStartups 100:30:100", "MaxSessions 100", "ClientAliveInterval 60", "ClientAliveCountMax 10", "LoginGraceTime 120"} {
		if !strings.Contains(script, tuning) {
			t.Fatalf("script is missing %q:\n%s", tuning, script)
		}
	}
	if !strings.Contains(script, "PasswordAuthentication no") {
		t.Fatalf("password auth edit missing when enabled:\n%s", script)
	}
}

func TestSSHDHardeningScriptNeverRewritesExplicitPermitRootLogin(t *testing.T) {
	script := sshdHardeningScript(sshdHardeningOptions{})

	if strings.Contains(script, "s/^PermitRootLogin no/") {
		t.Fatalf("script must not rewrite PermitRootLogin no:\n%s", script)
	}
	guard := strings.Index(script, `if ! grep -Eq '^[[:space:]]*PermitRootLogin[[:space:]]' "$cfg"; then`)
	set := strings.Index(script, "PermitRootLogin prohibit-password")
	if guard < 0 || set < 0 || guard > set {
		t.Fatalf("prohibit-password must only apply when no active directive exists:\n%s", script)
	}
}

func TestSSHDHardeningScriptLeavesPasswordAuthWhenDisabled(t *testing.T) {
	script := sshdHardeningScript(sshdHardeningOptions{DisablePasswordAuth: false})
	if strings.Contains(script, "PasswordAuthentication") {
		t.Fatalf("password auth must be untouched when key access is unverified:\n%s", script)
	}
}

func TestSSHDHardeningErrorIncludesValidatorOutput(t *testing.T) {
	err := sshdHardeningError("sshd -t rejected the hardened sshd_config; restored the previous configuration:\n/etc/ssh/sshd_config line 140: Directive 'MaxStartups' is not allowed within a Match block\n", errors.New("command failed: Process exited with status 1"))
	for _, want := range []string{"sshd was not reloaded", "restored the previous configuration", "not allowed within a Match block"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestSSHDReloadCommandReloadsInsteadOfRestarting(t *testing.T) {
	if strings.Contains(sshdReloadCommand, "restart") || !strings.Contains(sshdReloadCommand, "systemctl reload ssh") || !strings.Contains(sshdReloadCommand, "systemctl reload sshd") {
		t.Fatalf("reload command = %q", sshdReloadCommand)
	}
}

func TestFail2BanJailConfigBansOnSSHPortSet(t *testing.T) {
	if config := fail2banSSHDJailConfig("", nil); !strings.Contains(config, "\nport = 22\n") {
		t.Fatalf("default jail should ban on port 22:\n%s", config)
	}
	config := fail2banSSHDJailConfig("", []int{2222, 22})
	if !strings.Contains(config, "\nport = 22,2222\n") {
		t.Fatalf("custom jail should ban on 22 and 2222:\n%s", config)
	}
	if strings.Contains(config, "port = ssh") {
		t.Fatalf("jail must not use the ssh service name:\n%s", config)
	}
}

func TestHardenSecurityRejectsInvalidSSHPorts(t *testing.T) {
	if err := NewProvisioner(nil, false).HardenSecurity(HardeningOptions{SSHPorts: []int{0}}); err == nil {
		t.Fatal("expected invalid SSH port to be rejected")
	}
}

// TestSSHDHardeningScriptRestoresOnValidationFailure runs the generated
// script against a temp sshd_config with a fake failing `sshd`.
func TestSSHDHardeningScriptRestoresOnValidationFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires a POSIX shell and GNU sed")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	for _, name := range []string{"sed", "grep", "cp"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not available", name)
		}
	}

	run := func(t *testing.T, sshdExit int, original string) (string, string, error) {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "sshd_config")
		if err := os.WriteFile(cfg, []byte(original), 0o644); err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(dir, "bin")
		if err := os.Mkdir(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		fake := "#!/bin/sh\necho 'fake sshd validation message'\nexit " + map[int]string{0: "0", 1: "1"}[sshdExit] + "\n"
		if err := os.WriteFile(filepath.Join(bin, "sshd"), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
		script := sshdHardeningScript(sshdHardeningOptions{DisablePasswordAuth: true})
		script = strings.ReplaceAll(script, sshdConfigBackupPath, filepath.Join(dir, "sshd_config.tako-bak"))
		script = strings.ReplaceAll(script, sshdConfigPath, cfg)
		script = strings.ReplaceAll(script, "[ -d /run/sshd ] || mkdir -p /run/sshd", ":")
		cmd := exec.Command(sh, "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, runErr := cmd.CombinedOutput()
		data, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return string(data), string(out), runErr
	}

	original := "PermitRootLogin no\n#PasswordAuthentication yes\n"

	got, out, err := run(t, 1, original)
	if err == nil {
		t.Fatal("script should exit non-zero when sshd -t fails")
	}
	if got != original {
		t.Fatalf("config not restored:\n%s", got)
	}
	if !strings.Contains(out, "fake sshd validation message") {
		t.Fatalf("validator output missing: %s", out)
	}

	got, _, err = run(t, 0, original)
	if err != nil {
		t.Fatalf("script failed with valid config: %v", err)
	}
	if !strings.Contains(got, "PermitRootLogin no\n") || strings.Contains(got, "prohibit-password") {
		t.Fatalf("PermitRootLogin no must be preserved:\n%s", got)
	}
	if !strings.Contains(got, "PasswordAuthentication no") || !strings.Contains(got, "MaxStartups 100:30:100") {
		t.Fatalf("edits not applied:\n%s", got)
	}
}
