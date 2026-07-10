package unregistry

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type recordingRunner struct {
	calls []CommandSpec
	errAt int
}

func TestExecRunnerRedactsSensitiveBuildArgsFromErrors(t *testing.T) {
	var streamed strings.Builder
	output, err := (ExecRunner{Stdout: &streamed, Stderr: &streamed}).Run(context.Background(), CommandSpec{
		Name:                "sh",
		Args:                []string{"-c", `echo "$1" >&2; exit 1`, "sh", "TOKEN=super-secret"},
		SensitiveArgIndexes: []int{3},
	})
	if err == nil {
		t.Fatal("expected command failure")
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(output, "super-secret") || strings.Contains(streamed.String(), "super-secret") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error did not redact sensitive arg: %v", err)
	}
}

func (r *recordingRunner) Run(_ context.Context, spec CommandSpec) (string, error) {
	r.calls = append(r.calls, spec)
	if r.errAt > 0 && len(r.calls) == r.errAt {
		return "failed", errors.New("boom")
	}
	return "ok", nil
}

func TestCheckAvailableChecksDockerBuildxAndPussh(t *testing.T) {
	runner := &recordingRunner{}
	client := Client{Runner: runner}

	if err := client.CheckAvailable(context.Background()); err != nil {
		t.Fatalf("CheckAvailable returned error: %v", err)
	}

	want := [][]string{
		{"version"},
		{"buildx", "version"},
		{"pussh", "--help"},
	}
	if len(runner.calls) != len(want) {
		t.Fatalf("calls = %#v, want %d", runner.calls, len(want))
	}
	for i, args := range want {
		if runner.calls[i].Name != "docker" || !slices.Equal(runner.calls[i].Args, args) {
			t.Fatalf("call %d = %#v, want docker %v", i, runner.calls[i], args)
		}
	}
}

func TestBuildUsesBuildxLoadForSinglePlatform(t *testing.T) {
	runner := &recordingRunner{}
	client := Client{Runner: runner}

	err := client.Build(context.Background(), BuildRequest{
		Image:      "demo/web:abc123",
		ContextDir: "/work/app",
		Dockerfile: "packages/web/Dockerfile",
		Platform:   "linux/arm64",
		Args:       map[string]string{"SENTRY_IMAGE": "getsentry/sentry:26.6.0", "A": "first"},
		Target:     "runtime",
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	if len(runner.calls) != 1 {
		t.Fatalf("calls = %#v, want 1", runner.calls)
	}
	got := runner.calls[0]
	wantArgs := []string{"buildx", "build", "--platform", "linux/arm64", "--load", "-t", "demo/web:abc123", "-f", "packages/web/Dockerfile", "--build-arg", "A=first", "--build-arg", "SENTRY_IMAGE=getsentry/sentry:26.6.0", "--target", "runtime", "."}
	if got.Name != "docker" || got.Dir != "/work/app" || !slices.Equal(got.Args, wantArgs) {
		t.Fatalf("build command = %#v, want docker %v in /work/app", got, wantArgs)
	}
	if want := []int{10, 12}; !slices.Equal(got.SensitiveArgIndexes, want) {
		t.Fatalf("sensitive arg indexes = %v, want %v", got.SensitiveArgIndexes, want)
	}
}

func TestPushUsesDockerPusshTargetAndKey(t *testing.T) {
	runner := &recordingRunner{}
	client := Client{Runner: runner}

	err := client.Push(context.Background(), PushRequest{
		Image:  "demo/web:abc123",
		Target: "deploy@example.test:2222",
		SSHKey: "/keys/id_ed25519",
	})
	if err != nil {
		t.Fatalf("Push returned error: %v", err)
	}

	if len(runner.calls) != 1 {
		t.Fatalf("calls = %#v, want 1", runner.calls)
	}
	got := runner.calls[0]
	wantArgs := []string{"pussh", "demo/web:abc123", "deploy@example.test:2222", "-i", "/keys/id_ed25519"}
	if got.Name != "docker" || !slices.Equal(got.Args, wantArgs) {
		t.Fatalf("push command = %#v, want docker %v", got, wantArgs)
	}
}

func TestPushCanSpecifyPlatformAndNoHostKeyCheck(t *testing.T) {
	runner := &recordingRunner{}
	client := Client{Runner: runner}

	err := client.Push(context.Background(), PushRequest{
		Image:      "demo/web:abc123",
		Target:     "deploy@example.test",
		Platform:   "linux/amd64",
		NoHostKeys: true,
	})
	if err != nil {
		t.Fatalf("Push returned error: %v", err)
	}

	wantArgs := []string{"pussh", "--platform", "linux/amd64", "--no-host-key-check", "demo/web:abc123", "deploy@example.test"}
	if !slices.Equal(runner.calls[0].Args, wantArgs) {
		t.Fatalf("push args = %#v, want %#v", runner.calls[0].Args, wantArgs)
	}
}
