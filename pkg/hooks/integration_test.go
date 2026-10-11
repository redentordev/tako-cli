package hooks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

// runResolvedHook resolves the project's hooks with the running platform's
// rules and runs one of them through BuildEnv, returning its output lines.
func runResolvedHook(t *testing.T, project string, name Name, passEnv []string) (Outcome, []string) {
	t.Helper()
	res := Resolve(project, nil, runtime.GOOS)
	if err := res.Err(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	invocation, ok := res.Hooks[name]
	if !ok {
		t.Fatalf("%s not resolved: %+v", name, res)
	}
	sink := &events.BufferSink{}
	env := BuildEnv(os.Environ(), passEnv, Context{Hook: name, Command: "deploy", Project: "demo"}, runtime.GOOS)
	outcome := (&Runner{Sink: sink}).Run(context.Background(), invocation, env)
	var lines []string
	for _, event := range sink.Events() {
		if event.Type == events.TypeHookOutput {
			lines = append(lines, event.Data["data"].(string))
		}
	}
	return outcome, lines
}

func TestRealShellHookOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook")
	}
	project := t.TempDir()
	writeHookFile(t, filepath.Join(project, ".tako-hooks"), "pre-deploy", `#!/bin/sh
echo "hook=$TAKO_HOOK project=$TAKO_PROJECT secret=${HOOKS_TEST_DOTENV_SECRET:-absent}"
if read line; then echo "stdin=data"; else echo "stdin=eof"; fi
pwd
`, 0o755)
	t.Setenv("HOOKS_TEST_DOTENV_SECRET", "s3cret-value")
	outcome, lines := runResolvedHook(t, project, PreDeploy, nil)
	if outcome.Status != StatusCompleted {
		t.Fatalf("outcome = %+v lines = %v", outcome, lines)
	}
	resolvedProject, _ := filepath.EvalSymlinks(project)
	if len(lines) != 3 || lines[0] != "hook=pre-deploy project=demo secret=absent" || lines[1] != "stdin=eof" {
		t.Fatalf("lines = %v", lines)
	}
	if got, _ := filepath.EvalSymlinks(lines[2]); got != resolvedProject {
		t.Fatalf("working directory = %q, want %q", lines[2], project)
	}
}

func TestRealCmdHookOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows .cmd hook")
	}
	project := t.TempDir()
	writeHookFile(t, filepath.Join(project, ".tako-hooks"), "post-deploy.cmd", "@echo off\r\necho hook=%TAKO_HOOK% secret=%HOOKS_TEST_DOTENV_SECRET%\r\nexit /b 0\r\n", 0o644)
	t.Setenv("HOOKS_TEST_DOTENV_SECRET", "s3cret-value")
	outcome, lines := runResolvedHook(t, project, PostDeploy, nil)
	if outcome.Status != StatusCompleted || len(lines) != 1 || lines[0] != "hook=post-deploy secret=" {
		t.Fatalf("outcome = %+v lines = %q", outcome, lines)
	}
}

func TestRealPowerShellHookOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows .ps1 hook")
	}
	if _, err := exec.LookPath("pwsh"); err != nil {
		if _, err := exec.LookPath("powershell"); err != nil {
			t.Skip("no PowerShell on PATH")
		}
	}
	project := t.TempDir()
	writeHookFile(t, filepath.Join(project, ".tako-hooks"), "pre-deploy.ps1", "Write-Output \"hook=$env:TAKO_HOOK token=$env:HOOKS_TEST_TOKEN\"\r\nexit 4\r\n", 0o644)
	t.Setenv("HOOKS_TEST_TOKEN", "passed")
	outcome, lines := runResolvedHook(t, project, PreDeploy, []string{"HOOKS_TEST_*"})
	if outcome.Status != StatusFailed || outcome.ExitCode == nil || *outcome.ExitCode != 4 {
		t.Fatalf("outcome = %+v lines = %q", outcome, lines)
	}
	if len(lines) != 1 || lines[0] != "hook=pre-deploy token=passed" {
		t.Fatalf("lines = %q", lines)
	}
}
