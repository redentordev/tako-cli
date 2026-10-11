// Package providercli runs external provider CLIs (aws, gcloud, gh, vault,
// ...) on the deploying machine. It is shared by secretSources adapters in
// pkg/secrets and registry credential minting in pkg/registryauth so both use
// one timeout, one environment policy and one set of test seams.
package providercli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout bounds every provider command.
const DefaultTimeout = 2 * time.Minute

// DefaultMaxStderrBytes caps stderr placed in an error when stdout is kept
// out of errors.
const DefaultMaxStderrBytes = 2048

var (
	commandContext = exec.CommandContext
	lookPath       = exec.LookPath
)

// Options tunes one command run.
type Options struct {
	// Command overrides the command constructor. Nil uses the package seam.
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
	// Timeout overrides DefaultTimeout when positive.
	Timeout time.Duration
	// ExcludeStdoutFromErrors keeps standard output out of every error. It is
	// required when stdout is a credential: a failing command whose stderr is
	// empty is then reported by its exit status only, and stderr is truncated
	// to MaxStderrBytes after RedactStderr.
	ExcludeStdoutFromErrors bool
	// RedactStderr is applied to stderr before it is placed in an error when
	// ExcludeStdoutFromErrors is set.
	RedactStderr func(string) string
	// MaxStderrBytes overrides DefaultMaxStderrBytes when positive.
	MaxStderrBytes int
}

// LookPath reports where name is found on PATH through the package seam.
func LookPath(name string) (string, error) {
	return lookPath(name)
}

// MissingCLIError is the shared message for a provider CLI that is not on
// PATH.
func MissingCLIError(provider, command, install string) error {
	return fmt.Errorf("%s provider requires the %q CLI on PATH; install it from %s", provider, command, install)
}

// Run executes name with args, an empty stdin, the process environment and a
// timeout, returning stdout.
func Run(ctx context.Context, opts Options, name string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	command := opts.Command
	if command == nil {
		command = commandContext
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := command(commandCtx, name, args...)
	cmd.Env = os.Environ()
	output, err := cmd.Output()
	if commandCtx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s command timed out after %s", name, timeout)
	}
	if err == nil {
		return string(output), nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return "", fmt.Errorf("%s failed: %w", name, err)
	}
	if opts.ExcludeStdoutFromErrors {
		return "", fmt.Errorf("%s", safeFailureMessage(name, exitErr, string(output), opts))
	}
	message := strings.TrimSpace(string(exitErr.Stderr))
	if message == "" {
		message = strings.TrimSpace(string(output))
	}
	if message != "" {
		return "", fmt.Errorf("%s failed: %s", name, message)
	}
	return "", fmt.Errorf("%s failed: %w", name, err)
}

// safeFailureMessage renders a failure without stdout. Any stdout value that
// also appears in stderr is masked before redaction and truncation.
func safeFailureMessage(name string, exitErr *exec.ExitError, stdout string, opts Options) string {
	status := exitErr.ProcessState.String()
	stderr := strings.TrimSpace(string(exitErr.Stderr))
	if stderr == "" {
		return fmt.Sprintf("%s failed: %s", name, status)
	}
	if trimmed := strings.TrimSpace(stdout); trimmed != "" {
		stderr = strings.ReplaceAll(stderr, trimmed, "[REDACTED]")
	}
	if opts.RedactStderr != nil {
		stderr = opts.RedactStderr(stderr)
	}
	limit := opts.MaxStderrBytes
	if limit <= 0 {
		limit = DefaultMaxStderrBytes
	}
	if len(stderr) > limit {
		stderr = truncateUTF8(stderr, limit) + "... (truncated)"
	}
	return fmt.Sprintf("%s failed: %s: %s", name, status, stderr)
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && (value[cut]&0xC0) == 0x80 {
		cut--
	}
	return value[:cut]
}

// SetForTesting replaces the command constructor and PATH lookup used by
// callers that do not pass Options.Command. It returns a restore function.
// Production code must never call it.
func SetForTesting(command func(ctx context.Context, name string, args ...string) *exec.Cmd, lookPathFunc func(name string) (string, error)) func() {
	oldCommand := commandContext
	oldLookPath := lookPath
	if command != nil {
		commandContext = command
	}
	if lookPathFunc != nil {
		lookPath = lookPathFunc
	}
	return func() {
		commandContext = oldCommand
		lookPath = oldLookPath
	}
}
