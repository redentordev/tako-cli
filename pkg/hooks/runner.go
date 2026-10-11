package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

// MaxLineBytes caps one hook.output line; longer lines are split.
const MaxLineBytes = 1024 * 1024

// DefaultWaitDelay is how long the runner waits after SIGTERM (or after the
// hook exits) before force-killing the process tree and closing pipes.
const DefaultWaitDelay = 10 * time.Second

// Runner runs resolved hooks and reports them as hook.* events.
type Runner struct {
	// Sink receives hook events; pass the engine's redacting event stream.
	Sink events.Sink
	// Timeout bounds each run; zero uses config.DefaultHookTimeout.
	Timeout time.Duration
	// OutputLevel is the level of hook.output events: info (verbose) or
	// debug (quiet).
	OutputLevel events.Level
	// WaitDelay overrides DefaultWaitDelay (tests).
	WaitDelay time.Duration
}

// Run executes one hook with env as its complete environment and no stdin,
// streaming each stdout/stderr line as a hook.output event. It never
// returns raw output to the caller and never writes to process stdout.
func (r *Runner) Run(ctx context.Context, invocation Invocation, env []string) Outcome {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = config.DefaultHookTimeout
	}
	outputLevel := r.OutputLevel
	if outputLevel == "" {
		outputLevel = events.LevelInfo
	}
	waitDelay := r.WaitDelay
	if waitDelay <= 0 {
		waitDelay = DefaultWaitDelay
	}
	name := invocation.Name

	started := map[string]any{
		"hook":           string(name),
		"source":         invocation.Source,
		"timeoutSeconds": int64(timeout / time.Second),
	}
	label := string(name) + " hook"
	if invocation.Source == SourceFile {
		started["path"] = invocation.RelPath
		label += " (" + invocation.RelPath + ")"
	} else {
		started["argv"] = append([]string(nil), invocation.Argv...)
	}
	if invocation.Interpreter != "" {
		started["interpreter"] = invocation.Interpreter
	}
	r.emit(events.Event{Type: events.TypeHookStarted, Level: events.LevelInfo, Message: fmt.Sprintf("→ Running %s\n", label), Data: started})

	start := time.Now()
	outcome := Outcome{Name: name}
	if len(invocation.Argv) == 0 {
		outcome.Status = StatusFailed
		outcome.Err = fmt.Errorf("hook %s has no command", name)
		r.finish(outcome, false)
		return outcome
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, invocation.Argv[0], invocation.Argv[1:]...)
	cmd.Dir = invocation.Dir
	cmd.Env = env
	cmd.Stdin = nil // the null device: hooks get immediate EOF
	stdout := &lineWriter{runner: r, hook: name, stream: "stdout", level: outputLevel}
	stderr := &lineWriter{runner: r, hook: name, stream: "stderr", level: outputLevel}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	tree := newProcessTree(cmd, waitDelay)
	cmd.Cancel = tree.terminate
	cmd.WaitDelay = waitDelay

	err := cmd.Start()
	if err == nil {
		tree.started()
		err = cmd.Wait()
	}
	tree.cleanup()
	stdout.flush()
	stderr.flush()

	outcome.DurationMs = time.Since(start).Milliseconds()
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		if code >= 0 {
			outcome.ExitCode = &code
		}
	}
	switch {
	case ctx.Err() != nil:
		outcome.Status = StatusFailed
		outcome.Err = fmt.Errorf("hook %s interrupted: %w", name, ctx.Err())
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		outcome.Status = StatusTimedOut
		outcome.TimedOut = true
		outcome.Err = fmt.Errorf("hook %s timed out after %s", name, timeout)
	case err != nil:
		outcome.Status = StatusFailed
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && outcome.ExitCode != nil {
			outcome.Err = fmt.Errorf("hook %s exited with status %d", name, *outcome.ExitCode)
		} else if errors.As(err, &exitErr) {
			outcome.Err = fmt.Errorf("hook %s was terminated: %v", name, err)
		} else {
			outcome.Err = fmt.Errorf("hook %s could not be started: %w", name, err)
		}
	default:
		outcome.Status = StatusCompleted
	}
	r.finish(outcome, stderr.mentionsUnsetVariable())
	return outcome
}

func (r *Runner) finish(outcome Outcome, unsetVariableHint bool) {
	exitCode := -1
	if outcome.ExitCode != nil {
		exitCode = *outcome.ExitCode
	}
	data := map[string]any{
		"hook":       string(outcome.Name),
		"exitCode":   exitCode,
		"durationMs": outcome.DurationMs,
	}
	elapsed := time.Duration(outcome.DurationMs) * time.Millisecond
	if outcome.Status == StatusCompleted {
		r.emit(events.Event{Type: events.TypeHookCompleted, Level: events.LevelInfo, Message: fmt.Sprintf("  ✓ %s hook completed (%s)\n", outcome.Name, elapsed.Round(10*time.Millisecond)), Data: data})
		return
	}
	data["error"] = outcome.Err.Error()
	data["timedOut"] = outcome.TimedOut
	message := fmt.Sprintf("  ✗ %v\n", outcome.Err)
	if unsetVariableHint {
		hint := "the hook mentioned an unset variable; hooks receive only the baseline environment, TAKO_* context, and variables listed in hooks.passEnv"
		data["hint"] = hint
		message += "    hint: " + hint + "\n"
	}
	r.emit(events.Event{Type: events.TypeHookFailed, Level: events.LevelError, Message: message, Data: data})
}

func (r *Runner) emit(event events.Event) {
	if r.Sink == nil {
		return
	}
	event.Phase = events.PhaseHooks
	r.Sink.Emit(event)
}

// lineWriter splits one output stream into hook.output events.
type lineWriter struct {
	runner *Runner
	hook   Name
	stream string
	level  events.Level

	mu        sync.Mutex
	buf       []byte
	unsetHint bool
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		index := bytes.IndexByte(w.buf, '\n')
		if index < 0 {
			break
		}
		w.emitLine(string(w.buf[:index]))
		w.buf = w.buf[index+1:]
	}
	for len(w.buf) >= MaxLineBytes {
		w.emitLine(string(w.buf[:MaxLineBytes]))
		w.buf = w.buf[MaxLineBytes:]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emitLine(string(w.buf))
		w.buf = nil
	}
}

func (w *lineWriter) emitLine(line string) {
	line = strings.TrimSuffix(line, "\r")
	if w.stream == "stderr" {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "unbound variable") || strings.Contains(lower, "parameter not set") || strings.Contains(lower, "parameter null or not set") {
			w.unsetHint = true
		}
	}
	w.runner.emit(events.Event{
		Type:    events.TypeHookOutput,
		Level:   w.level,
		Message: line + "\n",
		Data:    map[string]any{"hook": string(w.hook), "stream": w.stream, "data": line},
	})
}

func (w *lineWriter) mentionsUnsetVariable() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.unsetHint
}
