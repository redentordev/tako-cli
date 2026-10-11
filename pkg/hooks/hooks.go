// Package hooks resolves and runs Tako's local lifecycle hooks: executables
// in the project's hook directory (default .tako-hooks/) or argv commands
// declared under hooks.commands. Hooks run on the operator's machine, never
// on nodes, with a curated secret-free environment, and every output line
// is reported as a redacted hook.output event. See docs/HOOKS.md.
package hooks

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
)

// Name is a lifecycle hook name.
type Name string

// Recognized lifecycle hooks.
const (
	PreDeploy    Name = "pre-deploy"
	PostDeploy   Name = "post-deploy"
	PreRollback  Name = "pre-rollback"
	PostRollback Name = "post-rollback"
	PrePromote   Name = "pre-promote"
	PostPromote  Name = "post-promote"
	PreSetup     Name = "pre-setup"
	PostSetup    Name = "post-setup"
	OnFailure    Name = "on-failure"
)

// Known returns every recognized hook name in lifecycle order.
func Known() []Name {
	names := make([]Name, 0, len(config.HookNames))
	for _, name := range config.HookNames {
		names = append(names, Name(name))
	}
	return names
}

// IsKnown reports whether name is a recognized hook.
func IsKnown(name string) bool {
	return config.IsHookName(name)
}

// Hook sources reported in hook.started data.source.
const (
	SourceFile   = "file"
	SourceConfig = "config"
)

// Invocation is one resolved, runnable hook.
type Invocation struct {
	Name Name
	// Source is SourceFile or SourceConfig.
	Source string
	// Path is the hook file (absolute) for file hooks.
	Path string
	// RelPath is Path relative to the project directory, slash-separated,
	// for events and messages.
	RelPath string
	// Interpreter is the program that runs a script hook (pwsh, cmd.exe,
	// sh, ...), empty when the hook file or command runs directly.
	Interpreter string
	// Argv is the full command line: Argv[0] is executed without a shell.
	Argv []string
	// Dir is the working directory: the project directory.
	Dir string
}

// Outcome statuses.
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusTimedOut  = "timed_out"
	StatusSkipped   = "skipped"
)

// Outcome is the result of running (or skipping) one hook.
type Outcome struct {
	Name   Name
	Status string
	// ExitCode is set when the hook process ran to an exit status.
	ExitCode   *int
	DurationMs int64
	TimedOut   bool
	// Err describes a failure; nil when Status is completed or skipped.
	// Cancellation wraps the context error so errors.Is(err,
	// context.Canceled) holds.
	Err error
}

// Failed reports whether the hook ran and did not complete successfully.
func (o Outcome) Failed() bool {
	return o.Status == StatusFailed || o.Status == StatusTimedOut
}

// HookError is implemented by every resolution error that belongs to one
// hook, so callers can attribute it (for example to hooks.<name>).
type HookError interface {
	error
	HookName() Name
}

// NotExecutableError reports a Unix hook file without an executable bit.
type NotExecutableError struct {
	Hook Name
	Path string
}

func (e *NotExecutableError) HookName() Name { return e.Hook }
func (e *NotExecutableError) Error() string {
	return fmt.Sprintf("hook %s: %s is not executable; run `chmod +x %s` (or `git update-index --chmod=+x %s` when the file was committed from Windows)", e.Hook, e.Path, e.Path, e.Path)
}

// NoRunnableVariantError reports hook files that exist only in variants
// this platform cannot run (for example only pre-deploy.ps1 on Linux).
type NoRunnableVariantError struct {
	Hook  Name
	GOOS  string
	Files []string
}

func (e *NoRunnableVariantError) HookName() Name { return e.Hook }
func (e *NoRunnableVariantError) Error() string {
	want := fmt.Sprintf("an executable file named %s without an extension", e.Hook)
	if e.GOOS == "windows" {
		want = fmt.Sprintf("%s.ps1, %s.cmd, %s.bat, %s.exe, or an extensionless script", e.Hook, e.Hook, e.Hook, e.Hook)
	}
	return fmt.Sprintf("hook %s: no runnable %s variant exists for %s (found %s); add %s", e.Hook, e.Hook, e.GOOS, strings.Join(e.Files, ", "), want)
}

// AmbiguousHookError reports a hook defined more than once: several native
// Windows variants, or a file plus a hooks.commands entry.
type AmbiguousHookError struct {
	Hook    Name
	Sources []string
}

func (e *AmbiguousHookError) HookName() Name { return e.Hook }
func (e *AmbiguousHookError) Error() string {
	return fmt.Sprintf("hook %s is defined more than once (%s); keep exactly one definition", e.Hook, strings.Join(e.Sources, ", "))
}

// CommandNotFoundError reports a hooks.commands argv[0] that cannot be
// found.
type CommandNotFoundError struct {
	Hook    Name
	Command string
	Err     error
}

func (e *CommandNotFoundError) HookName() Name { return e.Hook }
func (e *CommandNotFoundError) Unwrap() error  { return e.Err }
func (e *CommandNotFoundError) Error() string {
	return fmt.Sprintf("hook %s: command %q from hooks.commands.%s was not found: %v", e.Hook, e.Command, e.Hook, e.Err)
}

// InterpreterNotFoundError reports a script hook whose interpreter is not
// installed (for example a .ps1 hook without pwsh or powershell.exe, or a
// shell script on Windows without Git Bash).
type InterpreterNotFoundError struct {
	Hook        Name
	Path        string
	Interpreter string
}

func (e *InterpreterNotFoundError) HookName() Name { return e.Hook }
func (e *InterpreterNotFoundError) Error() string {
	return fmt.Sprintf("hook %s: %s needs %s, which was not found on PATH", e.Hook, e.Path, e.Interpreter)
}
