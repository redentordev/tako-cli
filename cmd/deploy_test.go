package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/git"
)

func TestIsNonInteractiveAcceptsTruthyEnvValues(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		value string
	}{
		{name: "tako one", env: "TAKO_NONINTERACTIVE", value: "1"},
		{name: "tako true", env: "TAKO_NONINTERACTIVE", value: "true"},
		{name: "ci true uppercase", env: "CI", value: "TRUE"},
		{name: "ci yes", env: "CI", value: "yes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TAKO_NONINTERACTIVE", "")
			t.Setenv("CI", "")
			t.Setenv(tt.env, tt.value)

			if !isNonInteractive() {
				t.Fatalf("isNonInteractive() = false with %s=%q", tt.env, tt.value)
			}
		})
	}
}

func TestIsNonInteractiveRejectsFalseyEnvValues(t *testing.T) {
	t.Setenv("TAKO_NONINTERACTIVE", "0")
	t.Setenv("CI", "false")

	if isNonInteractive() {
		t.Fatal("isNonInteractive() should reject falsey values")
	}
}

func TestRequireDeployPromptAllowedRejectsNonInteractiveWithoutYes(t *testing.T) {
	t.Setenv("TAKO_NONINTERACTIVE", "true")
	t.Setenv("CI", "")

	err := requireDeployPromptAllowed("deployment plan includes destructive changes")
	if err == nil {
		t.Fatal("requireDeployPromptAllowed() error = nil, want non-interactive approval error")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %q, want --yes guidance", err)
	}
}

func TestRequireDeployPromptAllowedRejectsNonTerminalWithoutYes(t *testing.T) {
	t.Setenv("TAKO_NONINTERACTIVE", "")
	t.Setenv("CI", "")

	err := requireDeployPromptAllowed("deployment plan includes destructive changes")
	if err == nil {
		t.Fatal("requireDeployPromptAllowed() error = nil, want terminal requirement error")
	}
	if !strings.Contains(err.Error(), "terminal or --yes") {
		t.Fatalf("error = %q, want terminal/--yes guidance", err)
	}
}

func TestIsAffirmative(t *testing.T) {
	tests := []struct {
		response string
		want     bool
	}{
		{response: "y", want: true},
		{response: "Y\n", want: true},
		{response: "yes", want: true},
		{response: "YES\n", want: true},
		{response: "", want: false},
		{response: "no", want: false},
	}

	for _, tt := range tests {
		if got := isAffirmative(tt.response); got != tt.want {
			t.Fatalf("isAffirmative(%q) = %v, want %v", tt.response, got, tt.want)
		}
	}
}

func TestDeployActualStateErrorRefusesUnknownRunningServices(t *testing.T) {
	err := deployActualStateError(errors.New("node-a: takod unavailable"))
	if err == nil {
		t.Fatal("deployActualStateError returned nil")
	}
	for _, want := range []string{"refusing to plan", "unknown running services", "takod unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestDeployRemoteHistoryErrorFailsSuccessfulRuntimeMutation(t *testing.T) {
	err := deployRemoteHistoryError(errors.New("disk full"))
	if err == nil {
		t.Fatal("deployRemoteHistoryError returned nil")
	}
	for _, want := range []string{"deployment succeeded", "failed to save remote deployment history", "disk full"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestResolveDeployCommitInfoRejectsDirtyWorktree(t *testing.T) {
	reader := fakeDeployGitReader{
		repository: true,
		dirty:      true,
		status:     " M main.go\n?? new.txt\n",
	}

	_, err := resolveDeployCommitInfo(reader)
	if err == nil {
		t.Fatal("resolveDeployCommitInfo should reject dirty worktrees")
	}
	for _, want := range []string{"cannot deploy with uncommitted changes", "commit, stash, or discard", "M main.go", "?? new.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestResolveDeployCommitInfoReturnsCleanCommitInfo(t *testing.T) {
	want := &git.CommitInfo{
		Hash:      "abcdef",
		ShortHash: "abc",
		Branch:    "main",
		Message:   "deploy me",
		Author:    "redentor",
	}
	reader := fakeDeployGitReader{
		repository: true,
		commitInfo: want,
	}

	got, err := resolveDeployCommitInfo(reader)
	if err != nil {
		t.Fatalf("resolveDeployCommitInfo returned error: %v", err)
	}
	if got != want {
		t.Fatalf("commitInfo = %#v, want %#v", got, want)
	}
}

func TestResolveDeployCommitInfoRequiresGitRepository(t *testing.T) {
	_, err := resolveDeployCommitInfo(fakeDeployGitReader{})
	if err == nil {
		t.Fatal("resolveDeployCommitInfo should reject non-git repositories")
	}
	if !strings.Contains(err.Error(), "not a Git repository") {
		t.Fatalf("error = %q, want git repository guidance", err)
	}
}

type fakeDeployGitReader struct {
	repository bool
	dirty      bool
	status     string
	commitInfo *git.CommitInfo
}

func (f fakeDeployGitReader) IsRepository() bool {
	return f.repository
}

func (f fakeDeployGitReader) HasUncommittedChanges() (bool, error) {
	return f.dirty, nil
}

func (f fakeDeployGitReader) GetStatus() (string, error) {
	return f.status, nil
}

func (f fakeDeployGitReader) GetCommitInfo(_ string) (*git.CommitInfo, error) {
	if f.commitInfo == nil {
		return nil, errors.New("missing commit")
	}
	return f.commitInfo, nil
}
