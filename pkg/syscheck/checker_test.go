package syscheck

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCheckRequirementTimesOut(t *testing.T) {
	restore := useFakeSyscheckCommand(t)
	defer restore()
	t.Setenv("TAKO_FAKE_SYSCHECK_SLEEP", "1")
	oldTimeout := requirementCheckTimeout
	requirementCheckTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		requirementCheckTimeout = oldTimeout
	})

	installed, version := NewSystemChecker(false).checkRequirement(Requirement{
		Name:    "Git",
		Command: "git",
		Args:    []string{"--version"},
	})

	if installed {
		t.Fatal("checkRequirement should report a hung command as not installed")
	}
	if version != "" {
		t.Fatalf("version = %q, want empty", version)
	}
}

func TestRunSyscheckCommandReportsTimeout(t *testing.T) {
	restore := useFakeSyscheckCommand(t)
	defer restore()
	t.Setenv("TAKO_FAKE_SYSCHECK_SLEEP", "1")

	err := runSyscheckCommand(10*time.Millisecond, "docker", "info")
	if err == nil {
		t.Fatal("runSyscheckCommand should return timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %q, want timeout context", err)
	}
}

func TestCommandExistsReturnsFalseOnTimeout(t *testing.T) {
	restore := useFakeSyscheckCommand(t)
	defer restore()
	t.Setenv("TAKO_FAKE_SYSCHECK_SLEEP", "1")
	oldTimeout := commandExistsTimeout
	commandExistsTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		commandExistsTimeout = oldTimeout
	})

	if commandExists("brew") {
		t.Fatal("commandExists should return false for a hung command")
	}
}

func TestPromptNixpacksInstallReturnsFalseWhenNonInteractive(t *testing.T) {
	oldStdin := os.Stdin
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	_ = writer.Close()
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = reader.Close()
	})

	if NewSystemChecker(false).PromptNixpacksInstall() {
		t.Fatal("PromptNixpacksInstall should not install in non-interactive mode")
	}
}

func useFakeSyscheckCommand(t *testing.T) func() {
	t.Helper()
	oldCommand := syscheckCommandContext
	syscheckCommandContext = fakeSyscheckCommandContext
	t.Setenv("GO_WANT_TAKO_SYSCHECK_HELPER", "1")
	return func() {
		syscheckCommandContext = oldCommand
	}
}

func fakeSyscheckCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	commandArgs := append([]string{"-test.run=TestSyscheckCommandHelper", "--", name}, args...)
	return exec.CommandContext(ctx, os.Args[0], commandArgs...)
}

func TestSyscheckCommandHelper(t *testing.T) {
	if os.Getenv("GO_WANT_TAKO_SYSCHECK_HELPER") != "1" {
		return
	}
	if os.Getenv("TAKO_FAKE_SYSCHECK_SLEEP") == "1" {
		time.Sleep(time.Second)
		os.Exit(0)
	}
	_, _ = os.Stdout.WriteString("tool version 1.2.3\n")
	os.Exit(0)
}
