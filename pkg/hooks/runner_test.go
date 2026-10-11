package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

const helperEnv = "HOOKS_TEST_HELPER"

// TestHelperProcess is re-executed as the hook process by the runner tests.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "echo":
		fmt.Println("hello")
		fmt.Fprintln(os.Stderr, "warn line")
		input, _ := io.ReadAll(os.Stdin)
		fmt.Printf("stdin=%d\n", len(input))
		fmt.Print("partial")
		os.Exit(0)
	case "exit3":
		fmt.Fprintln(os.Stderr, "VAR: unbound variable")
		os.Exit(3)
	case "spawn":
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		child.Env = append(os.Environ(), helperEnv+"=sleep")
		if err := child.Start(); err != nil {
			os.Exit(9)
		}
		_ = os.WriteFile(os.Getenv("HOOKS_TEST_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		time.Sleep(time.Minute)
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(2)
}

func helperInvocation(t *testing.T, name Name) Invocation {
	t.Helper()
	return Invocation{Name: name, Source: SourceConfig, Argv: []string{os.Args[0], "-test.run=^TestHelperProcess$"}, Dir: t.TempDir()}
}

func helperEnvFor(mode string, extra ...string) []string {
	return append(append(os.Environ(), helperEnv+"="+mode), extra...)
}

func TestRunnerEmitsLifecycleAndOutputEvents(t *testing.T) {
	sink := &events.BufferSink{}
	runner := &Runner{Sink: sink, Timeout: 30 * time.Second}
	outcome := runner.Run(context.Background(), helperInvocation(t, PreDeploy), helperEnvFor("echo"))
	if outcome.Status != StatusCompleted || outcome.Err != nil || outcome.ExitCode == nil || *outcome.ExitCode != 0 || outcome.DurationMs < 0 {
		t.Fatalf("outcome = %+v", outcome)
	}
	got := sink.Events()
	if got[0].Type != events.TypeHookStarted || got[len(got)-1].Type != events.TypeHookCompleted {
		t.Fatalf("events = %+v", got)
	}
	started := got[0].Data
	if started["hook"] != "pre-deploy" || started["source"] != SourceConfig || started["timeoutSeconds"] != int64(30) {
		t.Fatalf("started data = %v", started)
	}
	lines := map[string]string{}
	for _, event := range got {
		if event.Phase != events.PhaseHooks {
			t.Fatalf("event phase = %q", event.Phase)
		}
		if event.Type != events.TypeHookOutput {
			continue
		}
		if event.Level != events.LevelInfo || event.Data["hook"] != "pre-deploy" || event.Message != event.Data["data"].(string)+"\n" {
			t.Fatalf("output event = %+v", event)
		}
		lines[event.Data["data"].(string)] = event.Data["stream"].(string)
	}
	for line, stream := range map[string]string{"hello": "stdout", "warn line": "stderr", "stdin=0": "stdout", "partial": "stdout"} {
		if lines[line] != stream {
			t.Fatalf("line %q stream = %q, want %q (lines %v)", line, lines[line], stream, lines)
		}
	}
	completed := got[len(got)-1].Data
	if completed["exitCode"] != 0 || completed["durationMs"].(int64) < 0 {
		t.Fatalf("completed data = %v", completed)
	}
}

func TestRunnerQuietOutputIsDebug(t *testing.T) {
	sink := &events.BufferSink{}
	runner := &Runner{Sink: sink, OutputLevel: events.LevelDebug}
	runner.Run(context.Background(), helperInvocation(t, PostDeploy), helperEnvFor("echo"))
	for _, event := range sink.Events() {
		if event.Type == events.TypeHookOutput && event.Level != events.LevelDebug {
			t.Fatalf("quiet output at %s", event.Level)
		}
	}
}

func TestRunnerReportsNonZeroExit(t *testing.T) {
	sink := &events.BufferSink{}
	outcome := (&Runner{Sink: sink}).Run(context.Background(), helperInvocation(t, PreDeploy), helperEnvFor("exit3"))
	if outcome.Status != StatusFailed || outcome.ExitCode == nil || *outcome.ExitCode != 3 || !strings.Contains(outcome.Err.Error(), "hook pre-deploy exited with status 3") {
		t.Fatalf("outcome = %+v", outcome)
	}
	got := sink.Events()
	failed := got[len(got)-1]
	if failed.Type != events.TypeHookFailed || failed.Level != events.LevelError || failed.Data["exitCode"] != 3 || failed.Data["timedOut"] != false || failed.Data["error"] == "" {
		t.Fatalf("failed event = %+v", failed)
	}
	if hint, _ := failed.Data["hint"].(string); !strings.Contains(hint, "hooks.passEnv") {
		t.Fatalf("missing passEnv hint: %v", failed.Data)
	}
}

func TestRunnerStartFailure(t *testing.T) {
	sink := &events.BufferSink{}
	invocation := Invocation{Name: PreSetup, Source: SourceConfig, Argv: []string{filepath.Join(t.TempDir(), "does-not-exist")}}
	outcome := (&Runner{Sink: sink}).Run(context.Background(), invocation, nil)
	if outcome.Status != StatusFailed || outcome.ExitCode != nil || !strings.Contains(outcome.Err.Error(), "could not be started") {
		t.Fatalf("outcome = %+v", outcome)
	}
	got := sink.Events()
	if got[len(got)-1].Type != events.TypeHookFailed || got[len(got)-1].Data["exitCode"] != -1 {
		t.Fatalf("events = %+v", got)
	}
}

func TestRunnerTimeoutKillsProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	sink := &events.BufferSink{}
	runner := &Runner{Sink: sink, Timeout: 2 * time.Second, WaitDelay: time.Second}
	start := time.Now()
	outcome := runner.Run(context.Background(), helperInvocation(t, PreDeploy), helperEnvFor("spawn", "HOOKS_TEST_PIDFILE="+pidFile))
	if outcome.Status != StatusTimedOut || !outcome.TimedOut || !outcome.Failed() {
		t.Fatalf("outcome = %+v", outcome)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	got := sink.Events()
	failed := got[len(got)-1]
	if failed.Type != events.TypeHookFailed || failed.Data["timedOut"] != true {
		t.Fatalf("failed event = %+v", failed)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid not recorded: %v", err)
	}
	pid, _ := strconv.Atoi(string(raw))
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("hook child %d still running after timeout", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestRunnerCancellationIsContextCanceled(t *testing.T) {
	sink := &events.BufferSink{}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	outcome := (&Runner{Sink: sink, WaitDelay: time.Second}).Run(ctx, helperInvocation(t, PreDeploy), helperEnvFor("sleep"))
	if outcome.Status != StatusFailed || !errors.Is(outcome.Err, context.Canceled) {
		t.Fatalf("outcome = %+v", outcome)
	}
}
