package registryauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/internal/providercli"
	"github.com/redentordev/tako-cli/pkg/config"
)

const (
	fakeToken  = "fake-token-0123456789abcdef"
	fakeToken2 = "fake-token-fedcba9876543210"
	ecrHost    = "123456789012.dkr.ecr.eu-central-1.amazonaws.com"
)

// TestRegistryAuthCommandHelper is the re-exec fake for provider CLIs. Each
// invocation is appended to REGISTRYAUTH_FAKE_LOG; output and failure are
// chosen per "<command>_<first arg>" key through FAKE_OUT_/FAKE_ERR_/FAKE_FAIL_
// environment variables.
func TestRegistryAuthCommandHelper(t *testing.T) {
	if os.Getenv("GO_WANT_REGISTRYAUTH_HELPER") != "1" {
		return
	}
	args := os.Args
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+2 >= len(args) {
		os.Exit(2)
	}
	command := args[separator+1]
	commandArgs := args[separator+2:]
	if logPath := os.Getenv("REGISTRYAUTH_FAKE_LOG"); logPath != "" {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(2)
		}
		_, _ = file.WriteString(strings.Join(append([]string{command}, commandArgs...), " ") + "\n")
		_ = file.Close()
	}
	key := command + "_" + commandArgs[0]
	if delay := os.Getenv("FAKE_DELAY_" + key); delay != "" {
		if d, err := time.ParseDuration(delay); err == nil {
			time.Sleep(d)
		}
	}
	fmt.Fprint(os.Stdout, os.Getenv("FAKE_OUT_"+key))
	fmt.Fprint(os.Stderr, os.Getenv("FAKE_ERR_"+key))
	if os.Getenv("FAKE_FAIL_"+key) == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}

type fakeCLI struct {
	t       *testing.T
	logPath string
}

func useFakeCLI(t *testing.T, missing ...string) *fakeCLI {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	t.Setenv("GO_WANT_REGISTRYAUTH_HELPER", "1")
	t.Setenv("REGISTRYAUTH_FAKE_LOG", logPath)
	missingSet := map[string]bool{}
	for _, name := range missing {
		missingSet[name] = true
	}
	restore := providercli.SetForTesting(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		commandArgs := append([]string{"-test.run=TestRegistryAuthCommandHelper", "--", name}, args...)
		return exec.CommandContext(ctx, os.Args[0], commandArgs...)
	}, func(name string) (string, error) {
		if missingSet[name] {
			return "", exec.ErrNotFound
		}
		return name, nil
	})
	t.Cleanup(restore)
	return &fakeCLI{t: t, logPath: logPath}
}

func (f *fakeCLI) commands() []string {
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		f.t.Fatalf("read log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func noEnv(string) string { return "" }

func envMap(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestECRMintArgvAndUsername(t *testing.T) {
	cli := useFakeCLI(t)
	t.Setenv("FAKE_OUT_aws_ecr", fakeToken+"\n")
	var registered []string
	minter := NewMinter(Options{Getenv: noEnv, Register: func(_ string, v string) { registered = append(registered, v) }})
	credential, err := minter.Auth(context.Background(), ecrHost, config.RegistryConfig{Provider: "ecr", Profile: "prod"})
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if credential.Username != "AWS" || credential.Password != fakeToken {
		t.Fatalf("credential = %+v", credential)
	}
	if got := cli.commands(); len(got) != 1 || got[0] != "aws ecr get-login-password --region eu-central-1 --profile prod" {
		t.Fatalf("commands = %q", got)
	}
	if len(registered) != 1 || registered[0] != fakeToken {
		t.Fatalf("registered = %q, want token registered before return", registered)
	}
}

func TestECRExplicitRegionArgv(t *testing.T) {
	args, err := ECRCommand("123456789012.dkr.ecr.us-west-1.amazonaws.com", config.RegistryConfig{Provider: "ecr", Region: "us-west-1"})
	if err != nil || strings.Join(args, " ") != "ecr get-login-password --region us-west-1" {
		t.Fatalf("args = %q, %v", args, err)
	}
}

func TestGCRMintArgvAndUsername(t *testing.T) {
	cli := useFakeCLI(t)
	t.Setenv("FAKE_OUT_gcloud_auth", fakeToken)
	minter := NewMinter(Options{Getenv: noEnv})
	credential, err := minter.Auth(context.Background(), "us-docker.pkg.dev", config.RegistryConfig{Provider: "artifact-registry", Account: "deploy@example.com"})
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if credential.Username != "oauth2accesstoken" || credential.Password != fakeToken {
		t.Fatalf("credential = %+v", credential)
	}
	if got := cli.commands(); len(got) != 1 || got[0] != "gcloud auth print-access-token --account deploy@example.com" {
		t.Fatalf("commands = %q", got)
	}
}

func TestGHCRTokenAndUsernameOrder(t *testing.T) {
	entry := config.RegistryConfig{Provider: "ghcr"}

	// GITHUB_TOKEN and GITHUB_ACTOR set, GH_TOKEN unset: no gh command.
	cli := useFakeCLI(t)
	minter := NewMinter(Options{Getenv: envMap(map[string]string{"GITHUB_TOKEN": fakeToken, "GITHUB_ACTOR": "ci-bot"})})
	credential, err := minter.Auth(context.Background(), "ghcr.io", entry)
	if err != nil || credential.Password != fakeToken || credential.Username != "ci-bot" || credential.FromGHCLI {
		t.Fatalf("CI credential = %+v, %v", credential, err)
	}
	if got := cli.commands(); len(got) != 0 {
		t.Fatalf("gh ran in CI mode: %q", got)
	}

	// GH_TOKEN wins over GITHUB_TOKEN; explicit username wins over actor.
	minter = NewMinter(Options{Getenv: envMap(map[string]string{"GH_TOKEN": fakeToken2, "GITHUB_TOKEN": fakeToken, "GITHUB_ACTOR": "ci-bot"})})
	credential, err = minter.Auth(context.Background(), "ghcr.io", config.RegistryConfig{Provider: "ghcr", Username: "octocat"})
	if err != nil || credential.Password != fakeToken2 || credential.Username != "octocat" {
		t.Fatalf("override credential = %+v, %v", credential, err)
	}

	// Nothing in the environment: gh auth token, then gh api user.
	t.Setenv("FAKE_OUT_gh_auth", fakeToken+"\n")
	t.Setenv("FAKE_OUT_gh_api", "octocat\n")
	minter = NewMinter(Options{Getenv: noEnv})
	credential, err = minter.Auth(context.Background(), "ghcr.io", entry)
	if err != nil || credential.Password != fakeToken || credential.Username != "octocat" || !credential.FromGHCLI {
		t.Fatalf("gh credential = %+v, %v", credential, err)
	}
	got := cli.commands()
	if len(got) != 2 || got[0] != "gh auth token" || got[1] != "gh api user --jq .login" {
		t.Fatalf("commands = %q", got)
	}
}

func TestRejectedOutputsNeverEchoed(t *testing.T) {
	cases := map[string]string{
		"empty":    "   \n",
		"short":    "fake-short",
		"control":  "fake-token-01234\x01567890",
		"oversize": strings.Repeat("f", MaxTokenBytes+1),
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			useFakeCLI(t)
			t.Setenv("FAKE_OUT_aws_ecr", output)
			minter := NewMinter(Options{Getenv: noEnv, Register: func(string, string) { t.Fatal("rejected token registered") }})
			_, err := minter.Auth(context.Background(), ecrHost, config.RegistryConfig{Provider: "ecr"})
			if err == nil {
				t.Fatal("expected rejection")
			}
			var mintErr *MintError
			if !errors.As(err, &mintErr) || mintErr.Host != ecrHost {
				t.Fatalf("error = %v, want MintError naming host", err)
			}
			trimmed := strings.TrimSpace(output)
			if trimmed != "" && strings.Contains(err.Error(), trimmed) {
				t.Fatalf("error echoed output: %.120s", err)
			}
			if !strings.Contains(err.Error(), "ecr") || !strings.Contains(err.Error(), ecrHost) {
				t.Fatalf("error = %v, want provider and host", err)
			}
		})
	}
}

func TestMissingCLIMessage(t *testing.T) {
	useFakeCLI(t, "gcloud")
	minter := NewMinter(Options{Getenv: noEnv})
	_, err := minter.Auth(context.Background(), "gcr.io", config.RegistryConfig{Provider: "gcr"})
	if err == nil || !strings.Contains(err.Error(), `"gcloud" CLI on PATH`) || !strings.Contains(err.Error(), "https://cloud.google.com/sdk/docs/install") {
		t.Fatalf("error = %v", err)
	}
}

func TestFailingCLIWithTokenOnStdoutDoesNotLeak(t *testing.T) {
	useFakeCLI(t)
	t.Setenv("FAKE_OUT_gh_auth", fakeToken)
	t.Setenv("FAKE_FAIL_gh_auth", "1")
	minter := NewMinter(Options{Getenv: noEnv})
	_, err := minter.Auth(context.Background(), "ghcr.io", config.RegistryConfig{Provider: "ghcr"})
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("error leaked stdout token: %v", err)
	}
	if !strings.Contains(err.Error(), "gh failed: exit status 1") || !strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("error = %v", err)
	}
}

func TestFailingCLIStderrIsRedacted(t *testing.T) {
	useFakeCLI(t)
	t.Setenv("FAKE_ERR_aws_ecr", "An error occurred (ExpiredToken) for known-secret-value")
	t.Setenv("FAKE_FAIL_aws_ecr", "1")
	minter := NewMinter(Options{Getenv: noEnv, Redact: func(s string) string { return strings.ReplaceAll(s, "known-secret-value", "[REDACTED]") }})
	_, err := minter.Auth(context.Background(), ecrHost, config.RegistryConfig{Provider: "ecr"})
	if err == nil || !strings.Contains(err.Error(), "ExpiredToken") || strings.Contains(err.Error(), "known-secret-value") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "aws sso login") {
		t.Fatalf("error = %v, want login hint", err)
	}
}

func TestRemintAfterRefreshAge(t *testing.T) {
	cli := useFakeCLI(t)
	t.Setenv("FAKE_OUT_gcloud_auth", fakeToken)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	minter := NewMinter(Options{Getenv: noEnv, Now: func() time.Time { return now }})
	entry := config.RegistryConfig{Provider: "gcr"}
	for i := 0; i < 3; i++ {
		if _, err := minter.Auth(context.Background(), "gcr.io", entry); err != nil {
			t.Fatalf("Auth: %v", err)
		}
	}
	if got := cli.commands(); len(got) != 1 {
		t.Fatalf("commands = %q, want one cached mint", got)
	}
	now = now.Add(50 * time.Minute)
	if _, err := minter.Auth(context.Background(), "gcr.io", entry); err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if got := cli.commands(); len(got) != 2 {
		t.Fatalf("commands = %q, want re-mint after 45m", got)
	}
	now = now.Add(5 * time.Hour)
	ecrMinter := NewMinter(Options{Getenv: noEnv, Now: func() time.Time { return now }})
	t.Setenv("FAKE_OUT_aws_ecr", fakeToken)
	if _, err := ecrMinter.Auth(context.Background(), ecrHost, config.RegistryConfig{Provider: "ecr"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Hour)
	if _, err := ecrMinter.Auth(context.Background(), ecrHost, config.RegistryConfig{Provider: "ecr"}); err != nil {
		t.Fatal(err)
	}
	if got := cli.commands(); len(got) != 3 {
		t.Fatalf("commands = %q, ECR must stay cached before 6h", got)
	}
	now = now.Add(2 * time.Hour)
	if _, err := ecrMinter.Auth(context.Background(), ecrHost, config.RegistryConfig{Provider: "ecr"}); err != nil {
		t.Fatal(err)
	}
	if got := cli.commands(); len(got) != 4 {
		t.Fatalf("commands = %q, want ECR re-mint after 6h", got)
	}
}

func TestConcurrentCallersShareOneMint(t *testing.T) {
	cli := useFakeCLI(t)
	t.Setenv("FAKE_OUT_aws_ecr", fakeToken)
	t.Setenv("FAKE_DELAY_aws_ecr", "300ms")
	minter := NewMinter(Options{Getenv: noEnv})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			credential, err := minter.Auth(context.Background(), ecrHost, config.RegistryConfig{Provider: "ecr"})
			if err == nil && credential.Password != fakeToken {
				err = fmt.Errorf("credential = %+v", credential)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := cli.commands(); len(got) != 1 {
		t.Fatalf("commands = %q, want one shared mint", got)
	}
}

func TestValidateTokenBounds(t *testing.T) {
	if err := ValidateToken(strings.Repeat("a", MinTokenLength)); err != nil {
		t.Fatalf("16-char token rejected: %v", err)
	}
	if err := ValidateToken(strings.Repeat("a", MaxTokenBytes)); err != nil {
		t.Fatalf("4096-byte token rejected: %v", err)
	}
	if err := ValidateToken(strings.Repeat("a", MinTokenLength-1)); err == nil {
		t.Fatal("15-char token accepted")
	}
}
