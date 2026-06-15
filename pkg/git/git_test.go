package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

func TestStatusIgnoresTakoLocalState(t *testing.T) {
	dir := initRealGitRepo(t)
	takoHistoryDir := filepath.Join(dir, ".tako", "deployments", "dev", "history")
	if err := os.MkdirAll(takoHistoryDir, 0755); err != nil {
		t.Fatalf("create tako state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(takoHistoryDir, "deploy.json"), []byte("{}\n"), 0644); err != nil {
		t.Fatalf("write tako state: %v", err)
	}

	client := NewClient(dir)
	dirty, err := client.HasUncommittedChanges()
	if err != nil {
		t.Fatalf("HasUncommittedChanges returned error: %v", err)
	}
	if dirty {
		t.Fatal("HasUncommittedChanges returned true for generated .tako state")
	}

	status, err := client.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if strings.Contains(status, ".tako") {
		t.Fatalf("GetStatus included generated .tako state: %q", status)
	}
}

func TestStatusReportsSourceChangesWhenTakoLocalStateExists(t *testing.T) {
	dir := initRealGitRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, ".tako"), 0755); err != nil {
		t.Fatalf("create tako state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tako", ".lock"), []byte("{}\n"), 0600); err != nil {
		t.Fatalf("write tako lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatalf("modify source file: %v", err)
	}

	client := NewClient(dir)
	dirty, err := client.HasUncommittedChanges()
	if err != nil {
		t.Fatalf("HasUncommittedChanges returned error: %v", err)
	}
	if !dirty {
		t.Fatal("HasUncommittedChanges returned false for modified source")
	}

	status, err := client.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if !strings.Contains(status, "app.txt") {
		t.Fatalf("GetStatus = %q, want source file change", status)
	}
	if strings.Contains(status, ".tako") {
		t.Fatalf("GetStatus included generated .tako state: %q", status)
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

func initRealGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runRealGit(t, dir, "init", "-q")
	runRealGit(t, dir, "config", "user.email", "test@example.com")
	runRealGit(t, dir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("initial\n"), 0644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	runRealGit(t, dir, "add", "app.txt")
	runRealGit(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func runRealGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
	return string(output)
}
