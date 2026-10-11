package providercli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const fakeCredential = "fake-token-0123456789abcdef"

func fakeCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	commandArgs := append([]string{"-test.run=TestProviderCLIHelper", "--", name}, args...)
	return exec.CommandContext(ctx, os.Args[0], commandArgs...)
}

func TestProviderCLIHelper(t *testing.T) {
	if os.Getenv("GO_WANT_PROVIDERCLI_HELPER") != "1" {
		return
	}
	fmt.Fprint(os.Stdout, os.Getenv("PROVIDERCLI_STDOUT"))
	fmt.Fprint(os.Stderr, os.Getenv("PROVIDERCLI_STDERR"))
	if os.Getenv("PROVIDERCLI_FAIL") == "1" {
		os.Exit(3)
	}
	os.Exit(0)
}

func useHelper(t *testing.T, stdout, stderr string, fail bool) {
	t.Helper()
	t.Setenv("GO_WANT_PROVIDERCLI_HELPER", "1")
	t.Setenv("PROVIDERCLI_STDOUT", stdout)
	t.Setenv("PROVIDERCLI_STDERR", stderr)
	if fail {
		t.Setenv("PROVIDERCLI_FAIL", "1")
	} else {
		t.Setenv("PROVIDERCLI_FAIL", "0")
	}
}

func TestRunReturnsStdout(t *testing.T) {
	useHelper(t, "value", "", false)
	got, err := Run(context.Background(), Options{Command: fakeCommand}, "tool")
	if err != nil || got != "value" {
		t.Fatalf("Run = %q, %v", got, err)
	}
}

func TestRunDefaultFallsBackToStdout(t *testing.T) {
	useHelper(t, "listing-output", "", true)
	_, err := Run(context.Background(), Options{Command: fakeCommand}, "tool")
	if err == nil || !strings.Contains(err.Error(), "listing-output") {
		t.Fatalf("default error = %v, want stdout fallback", err)
	}
}

func TestRunExcludeStdoutNeverLeaksCredential(t *testing.T) {
	useHelper(t, fakeCredential, "", true)
	_, err := Run(context.Background(), Options{Command: fakeCommand, ExcludeStdoutFromErrors: true}, "gh", "auth", "token")
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(err.Error(), fakeCredential) {
		t.Fatalf("error leaked stdout: %v", err)
	}
	if !strings.Contains(err.Error(), "gh failed") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("error = %v, want command name and exit status", err)
	}
}

func TestRunExcludeStdoutRedactsAndTruncatesStderr(t *testing.T) {
	useHelper(t, fakeCredential, "denied "+fakeCredential+" secret-word "+strings.Repeat("x", 4000), true)
	_, err := Run(context.Background(), Options{
		Command:                 fakeCommand,
		ExcludeStdoutFromErrors: true,
		RedactStderr:            func(s string) string { return strings.ReplaceAll(s, "secret-word", "[MASKED]") },
	}, "aws")
	if err == nil {
		t.Fatal("expected failure")
	}
	message := err.Error()
	if strings.Contains(message, fakeCredential) || strings.Contains(message, "secret-word") {
		t.Fatalf("error not redacted: %.200s", message)
	}
	if !strings.Contains(message, "denied [REDACTED] [MASKED]") || !strings.Contains(message, "(truncated)") {
		t.Fatalf("error = %.300s", message)
	}
	if len(message) > DefaultMaxStderrBytes+200 {
		t.Fatalf("error length %d exceeds truncation bound", len(message))
	}
}

func TestSetForTestingRestores(t *testing.T) {
	restore := SetForTesting(nil, func(string) (string, error) { return "", exec.ErrNotFound })
	if _, err := LookPath("anything"); err == nil {
		t.Fatal("fake lookPath not installed")
	}
	restore()
	if fmt.Sprintf("%p", lookPath) != fmt.Sprintf("%p", exec.LookPath) {
		t.Fatal("lookPath not restored")
	}
}

func TestMissingCLIErrorMessage(t *testing.T) {
	err := MissingCLIError("ecr", "aws", "https://example.invalid/install")
	if err.Error() != `ecr provider requires the "aws" CLI on PATH; install it from https://example.invalid/install` {
		t.Fatalf("message = %q", err)
	}
}
