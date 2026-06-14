package git

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestGitOutputTrimsCommandOutput(t *testing.T) {
	restore := useFakeGitCommand(t)
	defer restore()
	t.Setenv("TAKO_FAKE_GIT_OUTPUT", "abcdef\n")

	client := NewClient(".")
	commit, err := client.GetCurrentCommit()
	if err != nil {
		t.Fatalf("GetCurrentCommit returned error: %v", err)
	}
	if commit != "abcdef" {
		t.Fatalf("commit = %q, want abcdef", commit)
	}
}

func TestGitOutputTimesOut(t *testing.T) {
	restore := useFakeGitCommand(t)
	defer restore()
	t.Setenv("TAKO_FAKE_GIT_SLEEP", "1")
	oldTimeout := gitCommandTimeout
	gitCommandTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		gitCommandTimeout = oldTimeout
	})

	client := NewClient(".")
	_, err := client.GetStatus()
	if err == nil {
		t.Fatal("GetStatus should return a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %q, want timeout context", err)
	}
}

func useFakeGitCommand(t *testing.T) func() {
	t.Helper()
	oldCommand := gitCommandContext
	gitCommandContext = fakeGitCommandContext
	t.Setenv("GO_WANT_TAKO_GIT_HELPER", "1")
	return func() {
		gitCommandContext = oldCommand
	}
}

func fakeGitCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	commandArgs := append([]string{"-test.run=TestGitCommandHelper", "--", name}, args...)
	return exec.CommandContext(ctx, os.Args[0], commandArgs...)
}

func TestGitCommandHelper(t *testing.T) {
	if os.Getenv("GO_WANT_TAKO_GIT_HELPER") != "1" {
		return
	}
	if os.Getenv("TAKO_FAKE_GIT_SLEEP") == "1" {
		time.Sleep(time.Second)
		os.Exit(0)
	}
	if output := os.Getenv("TAKO_FAKE_GIT_OUTPUT"); output != "" {
		_, _ = os.Stdout.WriteString(output)
	}
	os.Exit(0)
}
