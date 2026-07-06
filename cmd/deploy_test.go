package cmd

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	localstate "github.com/redentordev/tako-cli/pkg/state"
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

func TestDeployCommandSilencesUsageOnRunErrors(t *testing.T) {
	if !deployCmd.SilenceUsage {
		t.Fatal("deploy command should silence usage on execution errors")
	}
}

func TestRunDeployFailsInvalidYAMLBeforeGit(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	root := t.TempDir()
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
	})
	if err := os.Chdir(root); err != nil {
		t.Fatalf("failed to switch cwd: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), []byte("project:\n  name: demo\n  version: [\n"), 0600); err != nil {
		t.Fatalf("failed to write invalid config: %v", err)
	}
	oldCfgFile := cfgFile
	cfgFile = ""
	t.Cleanup(func() {
		cfgFile = oldCfgFile
	})

	err = runDeploy(deployCmd, nil)
	if err == nil {
		t.Fatal("runDeploy should fail on invalid YAML")
	}
	for _, want := range []string{"YAML syntax error in tako.yaml", "line 3", "3 |   version: [", "Check indentation"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "Git repository") {
		t.Fatalf("deploy should fail before git checks, got %q", err)
	}
}

func TestRunDeployImageRequiresServiceBeforeGit(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	root := t.TempDir()
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
	})
	if err := os.Chdir(root); err != nil {
		t.Fatalf("failed to switch cwd: %v", err)
	}
	t.Setenv("SSH_PASSWORD", "test-password")
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), []byte(`
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: example.com
    user: deploy
    password: ${SSH_PASSWORD}
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
`), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	oldCfgFile := cfgFile
	oldDeployImage := deployImage
	oldDeployService := deployService
	oldDeploySource := deploySource
	cfgFile = ""
	deployImage = "registry.example.com/web:sha"
	deployService = ""
	deploySource = ""
	t.Cleanup(func() {
		cfgFile = oldCfgFile
		deployImage = oldDeployImage
		deployService = oldDeployService
		deploySource = oldDeploySource
	})

	err = runDeploy(deployCmd, nil)
	if err == nil {
		t.Fatal("runDeploy should fail when --image is used without --service")
	}
	if !strings.Contains(err.Error(), "--image requires --service") {
		t.Fatalf("error = %q, want --service guidance", err)
	}
	if strings.Contains(err.Error(), "Git repository") {
		t.Fatalf("deploy should fail before git checks, got %q", err)
	}
}

func TestRunDeployFailsInvalidConfigBeforeGit(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	root := t.TempDir()
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
	})
	if err := os.Chdir(root); err != nil {
		t.Fatalf("failed to switch cwd: %v", err)
	}
	t.Setenv("SSH_PASSWORD", "test-password")
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), []byte(`
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: example.com
    user: deploy
    password: ${SSH_PASSWORD}
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
        replicas: 2
        loadBalancer:
          strategy: ip_hash
`), 0600); err != nil {
		t.Fatalf("failed to write invalid config: %v", err)
	}
	oldCfgFile := cfgFile
	cfgFile = ""
	t.Cleanup(func() {
		cfgFile = oldCfgFile
	})

	err = runDeploy(deployCmd, nil)
	if err == nil {
		t.Fatal("runDeploy should fail on invalid config")
	}
	for _, want := range []string{"config validation failed in tako.yaml", "invalid load balancer strategy", "round_robin and sticky"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "Git repository") {
		t.Fatalf("deploy should fail before git checks, got %q", err)
	}
}

func TestFormatDeployConfigErrorReportsValidationFailures(t *testing.T) {
	err := formatDeployConfigError("tako.yaml", errors.New(`invalid config: service web: invalid load balancer strategy "ip_hash"; supported strategies are round_robin and sticky`))
	for _, want := range []string{"config validation failed in tako.yaml", "invalid load balancer strategy", "round_robin and sticky"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
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

func TestValidateDeployImageOptionsRequiresService(t *testing.T) {
	_, err := validateDeployImageOptions("", "registry.example.com/web:sha", "")
	if err == nil {
		t.Fatal("validateDeployImageOptions should require --service with --image")
	}
	if !strings.Contains(err.Error(), "--image requires --service") {
		t.Fatalf("error = %q, want --service guidance", err)
	}
}

func TestValidateDeployImageOptionsRejectsWhitespaceImage(t *testing.T) {
	_, err := validateDeployImageOptions("web", " \t\n", "")
	if err == nil {
		t.Fatal("validateDeployImageOptions should reject whitespace-only --image")
	}
	if !strings.Contains(err.Error(), "--image must not be empty") {
		t.Fatalf("error = %q, want empty image guidance", err)
	}
}

func TestValidateDeployImageOptionsRejectsSourceCombination(t *testing.T) {
	_, err := validateDeployImageOptions("web", "registry.example.com/web:sha", ".")
	if err == nil {
		t.Fatal("validateDeployImageOptions should reject --image with --source")
	}
	if !strings.Contains(err.Error(), "--image cannot be combined with --source") {
		t.Fatalf("error = %q, want source combination guidance", err)
	}
}

func TestValidateDeployArchiveOptionsRequiresService(t *testing.T) {
	archivePath := writeDeployTestFile(t, "app.tar.gz", "archive")
	_, err := validateDeployArchiveOptions("", archivePath, "", "")
	if err == nil {
		t.Fatal("validateDeployArchiveOptions should require --service with --archive")
	}
	if !strings.Contains(err.Error(), "--archive requires --service") {
		t.Fatalf("error = %q, want --service guidance", err)
	}
}

func TestValidateDeployArchiveOptionsRejectsWhitespaceArchive(t *testing.T) {
	_, err := validateDeployArchiveOptions("web", " \t\n", "", "")
	if err == nil {
		t.Fatal("validateDeployArchiveOptions should reject whitespace-only --archive")
	}
	if !strings.Contains(err.Error(), "--archive must not be empty") {
		t.Fatalf("error = %q, want empty archive guidance", err)
	}
}

func TestValidateDeployArchiveOptionsRejectsSourceAndImageCombinations(t *testing.T) {
	archivePath := writeDeployTestFile(t, "app.tar.gz", "archive")
	if _, err := validateDeployArchiveOptions("web", archivePath, ".", ""); err == nil || !strings.Contains(err.Error(), "--archive cannot be combined with --source") {
		t.Fatalf("source combination error = %v, want source guidance", err)
	}
	if _, err := validateDeployArchiveOptions("web", archivePath, "", "registry.example.com/web:sha"); err == nil || !strings.Contains(err.Error(), "--archive cannot be combined with --image") {
		t.Fatalf("image combination error = %v, want image guidance", err)
	}
}

func TestValidateDeployArchiveOptionsRequiresRegularSupportedFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := validateDeployArchiveOptions("web", dir, "", ""); err == nil || !strings.Contains(err.Error(), "unsupported archive format") {
		t.Fatalf("directory format error = %v, want unsupported format", err)
	}
	archiveDirName := filepath.Join(t.TempDir(), "app.zip")
	if err := os.Mkdir(archiveDirName, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := validateDeployArchiveOptions("web", archiveDirName, "", ""); err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Fatalf("directory regular-file error = %v, want regular file guidance", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.zip")
	if _, err := validateDeployArchiveOptions("web", missing, "", ""); err == nil || !strings.Contains(err.Error(), "is not accessible") {
		t.Fatalf("missing file error = %v, want accessible guidance", err)
	}
}

func TestDeployArchiveBuildTagDerivesDeterministicTagAndAllowsExplicitRevision(t *testing.T) {
	archivePath := writeDeployTestFile(t, "app.zip", "hello archive")
	got, err := deployArchiveBuildTag("", archivePath)
	if err != nil {
		t.Fatalf("deployArchiveBuildTag returned error: %v", err)
	}
	if got != "archive-f8976760708a" {
		t.Fatalf("tag = %q, want deterministic content tag", got)
	}
	explicit, err := deployArchiveBuildTag("ci-123", archivePath)
	if err != nil {
		t.Fatalf("deployArchiveBuildTag explicit returned error: %v", err)
	}
	if explicit != "ci-123" {
		t.Fatalf("explicit tag = %q, want ci-123", explicit)
	}
}

func TestApplyDeployImageOverrideSetsImageAndClearsBuild(t *testing.T) {
	original := config.ServiceConfig{Build: ".", Image: "demo/web:old"}
	got := applyDeployImageOverride(original, " registry.example.com/web:sha ")
	if got.Image != "registry.example.com/web:sha" {
		t.Fatalf("Image = %q, want override", got.Image)
	}
	if got.Build != "" {
		t.Fatalf("Build = %q, want cleared", got.Build)
	}
	if original.Image != "demo/web:old" || original.Build != "." {
		t.Fatalf("original service mutated: %#v", original)
	}
}

func TestApplyDeploySourceOverrideSetsBuildClearsImageAndTrims(t *testing.T) {
	original := config.ServiceConfig{Build: "./old", Image: "demo/web:old"}
	got := applyDeploySourceOverride(original, " ./services/web \t")
	if got.Build != "./services/web" {
		t.Fatalf("Build = %q, want trimmed source", got.Build)
	}
	if got.Image != "" {
		t.Fatalf("Image = %q, want cleared", got.Image)
	}
	if original.Image != "demo/web:old" || original.Build != "./old" {
		t.Fatalf("original service mutated: %#v", original)
	}
}

func TestApplyDeploySourceOverrideNoopsForBlankSource(t *testing.T) {
	original := config.ServiceConfig{Build: "./old", Image: "demo/web:old"}
	got := applyDeploySourceOverride(original, " \t\n")
	if got.Image != original.Image || got.Build != original.Build {
		t.Fatalf("blank source changed service: got %#v want %#v", got, original)
	}
}

func TestApplyDeploySourceOverrideMakesImageServiceDeployableOnEmptyPlan(t *testing.T) {
	service := applyDeploySourceOverride(config.ServiceConfig{Image: "demo/web:old"}, ".")
	services := map[string]config.ServiceConfig{"web": service}
	got := deployplan.ServicesToDeployForPlan(&reconcile.ReconciliationPlan{}, services, false, true)
	if _, ok := got["web"]; !ok {
		t.Fatalf("overridden build service missing from deploy set: %#v", got)
	}
}

func TestApplyDeployArchiveOverrideSetsBuildClearsImageAndDoesNotMutateOriginal(t *testing.T) {
	original := config.ServiceConfig{Build: "./old", Image: "demo/web:old"}
	got := applyDeployArchiveOverride(original, " /tmp/tako-archive-123 \t")
	if got.Build != "/tmp/tako-archive-123" {
		t.Fatalf("Build = %q, want temp build context", got.Build)
	}
	if got.Image != "" {
		t.Fatalf("Image = %q, want cleared", got.Image)
	}
	if original.Image != "demo/web:old" || original.Build != "./old" {
		t.Fatalf("original service mutated: %#v", original)
	}
}

func TestExtractDeployArchiveTarGzPreservesFilesAndDirectories(t *testing.T) {
	archivePath := writeDeployTestTarGz(t, []deployTestArchiveEntry{
		{name: "app/", dir: true},
		{name: "app/Dockerfile", body: "FROM scratch\n"},
		{name: "app/main.txt", body: "hello"},
	})
	dest := t.TempDir()
	if err := extractDeployArchive(archivePath, dest); err != nil {
		t.Fatalf("extractDeployArchive returned error: %v", err)
	}
	if got := readDeployTestFile(t, filepath.Join(dest, "app", "main.txt")); got != "hello" {
		t.Fatalf("extracted file = %q, want hello", got)
	}
}

func TestExtractDeployArchiveZipPreservesFilesAndDirectories(t *testing.T) {
	archivePath := writeDeployTestZip(t, []deployTestArchiveEntry{
		{name: "app/", dir: true},
		{name: "app/main.txt", body: "hello zip"},
	})
	dest := t.TempDir()
	if err := extractDeployArchive(archivePath, dest); err != nil {
		t.Fatalf("extractDeployArchive returned error: %v", err)
	}
	if got := readDeployTestFile(t, filepath.Join(dest, "app", "main.txt")); got != "hello zip" {
		t.Fatalf("extracted file = %q, want hello zip", got)
	}
}

func TestExtractDeployArchiveHandlesDotSlashDirectoryEntries(t *testing.T) {
	archivePath := writeDeployTestTarGz(t, []deployTestArchiveEntry{
		{name: "./", dir: true},
		{name: "./app/main.txt", body: "hello dot slash"},
	})
	dest := t.TempDir()
	if err := extractDeployArchive(archivePath, dest); err != nil {
		t.Fatalf("extractDeployArchive returned error: %v", err)
	}
	if got := readDeployTestFile(t, filepath.Join(dest, "app", "main.txt")); got != "hello dot slash" {
		t.Fatalf("extracted file = %q, want hello dot slash", got)
	}
}

func TestExtractDeployArchiveRejectsBackslashEntryNames(t *testing.T) {
	archivePath := writeDeployTestZip(t, []deployTestArchiveEntry{{name: `app\main.txt`, body: "ambiguous"}})
	err := extractDeployArchive(archivePath, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "contains backslashes") {
		t.Fatalf("extract error = %v, want backslash rejection", err)
	}
}

func TestExtractDeployArchiveRejectsColonEntryNames(t *testing.T) {
	archivePath := writeDeployTestZip(t, []deployTestArchiveEntry{{name: "C:/app/main.txt", body: "ambiguous"}})
	err := extractDeployArchive(archivePath, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "contains colons") {
		t.Fatalf("extract error = %v, want colon rejection", err)
	}
}

func TestExtractDeployArchiveIgnoresTarMetadataEntries(t *testing.T) {
	archivePath := writeDeployTestTarGzRaw(t, []deployTestArchiveEntry{
		{name: "pax_header", body: "20 comment=metadata\n", typeflag: tar.TypeXHeader},
		{name: "pax_global_header", body: "20 comment=metadata\n", typeflag: tar.TypeXGlobalHeader},
		{name: "././@LongLink", body: "app/main.txt\x00", typeflag: tar.TypeGNULongName},
		{name: "././@LongLink", body: "app/link-target\x00", typeflag: tar.TypeGNULongLink},
		{name: "app/main.txt", body: "hello metadata"},
	})
	dest := t.TempDir()
	if err := extractDeployArchive(archivePath, dest); err != nil {
		t.Fatalf("extractDeployArchive returned error: %v", err)
	}
	if got := readDeployTestFile(t, filepath.Join(dest, "app", "main.txt")); got != "hello metadata" {
		t.Fatalf("extracted file = %q, want hello metadata", got)
	}
}

func TestExtractDeployArchiveRejectsPathTraversal(t *testing.T) {
	for _, tt := range []struct {
		name        string
		archivePath string
	}{
		{name: "tar.gz", archivePath: writeDeployTestTarGz(t, []deployTestArchiveEntry{{name: "../escape.txt", body: "no"}})},
		{name: "zip", archivePath: writeDeployTestZip(t, []deployTestArchiveEntry{{name: "../escape.txt", body: "no"}})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := extractDeployArchive(tt.archivePath, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), "would escape destination") {
				t.Fatalf("extract error = %v, want traversal rejection", err)
			}
		})
	}
}

func TestExtractDeployArchiveRejectsSymlinks(t *testing.T) {
	for _, tt := range []struct {
		name        string
		archivePath string
	}{
		{name: "tar.gz", archivePath: writeDeployTestTarGz(t, []deployTestArchiveEntry{{name: "link", symlink: "target"}})},
		{name: "zip", archivePath: writeDeployTestZip(t, []deployTestArchiveEntry{{name: "link", symlink: "target"}})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := extractDeployArchive(tt.archivePath, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), "links are not supported") {
				t.Fatalf("extract error = %v, want symlink rejection", err)
			}
		})
	}
}

func TestDeploySourceLabelForImageOverrideActivatesSourceMode(t *testing.T) {
	if got := deploySourceLabelForImageOverride("", "registry.example.com/web:sha"); got != "image" {
		t.Fatalf("source label = %q, want image", got)
	}
	if got := deploySourceLabelForImageOverride(".", "registry.example.com/web:sha"); got != "." {
		t.Fatalf("explicit source label = %q, want preserved source", got)
	}
}

func TestResolveDeploySourceInfoImageModeBypassesGitAndDerivesImageTag(t *testing.T) {
	now := time.Date(2026, 7, 5, 4, 34, 56, 0, time.UTC)
	info, err := resolveDeploySourceInfo(fakeDeployGitReader{}, false, deploySourceLabelForImageOverride("", "registry.example.com/web:sha"), "", "registry.example.com/web:sha", now)
	if err != nil {
		t.Fatalf("resolveDeploySourceInfo returned error: %v", err)
	}
	if !info.SourceMode {
		t.Fatal("SourceMode = false, want true")
	}
	if info.StateSource != "image" {
		t.Fatalf("StateSource = %q, want image", info.StateSource)
	}
	if info.BuildImageTag != "image-8a5076f3bc4d" {
		t.Fatalf("BuildImageTag = %q, want derived image tag", info.BuildImageTag)
	}
	if info.CommitInfo != nil {
		t.Fatalf("CommitInfo = %#v, want nil", info.CommitInfo)
	}
}

func TestResolveDeploySourceInfoImageModeAllowsExplicitRevision(t *testing.T) {
	info, err := resolveDeploySourceInfo(fakeDeployGitReader{}, false, deploySourceLabelForImageOverride("", "registry.example.com/web:sha"), "ci-123", "registry.example.com/web:sha", time.Now())
	if err != nil {
		t.Fatalf("resolveDeploySourceInfo returned error: %v", err)
	}
	if !info.SourceMode {
		t.Fatal("SourceMode = false, want true")
	}
	if info.StateSource != "image" {
		t.Fatalf("StateSource = %q, want image", info.StateSource)
	}
	if info.BuildImageTag != "ci-123" {
		t.Fatalf("BuildImageTag = %q, want explicit revision", info.BuildImageTag)
	}
}

func TestResolveDeploySourceInfoDefaultModeRequiresGitRepository(t *testing.T) {
	_, err := resolveDeploySourceInfo(fakeDeployGitReader{}, false, "", "", "", time.Date(2026, 7, 5, 4, 34, 56, 0, time.UTC))
	if err == nil {
		t.Fatal("resolveDeploySourceInfo should reject non-git repositories in default mode")
	}
	if !strings.Contains(err.Error(), "not a Git repository") {
		t.Fatalf("error = %q, want git repository guidance", err)
	}
}

func TestResolveDeploySourceInfoWhitespaceOnlyFlagsUseDefaultGitMode(t *testing.T) {
	_, err := resolveDeploySourceInfo(fakeDeployGitReader{}, false, " \t", "\n ", "", time.Now())
	if err == nil {
		t.Fatal("resolveDeploySourceInfo should use default git mode for whitespace-only source flags")
	}
	if !strings.Contains(err.Error(), "not a Git repository") {
		t.Fatalf("error = %q, want git repository guidance", err)
	}
}

func TestResolveDeploySourceInfoTrimsSourceAndRevision(t *testing.T) {
	info, err := resolveDeploySourceInfo(fakeDeployGitReader{}, false, " ./app \t", " ci-123 \n", "", time.Now())
	if err != nil {
		t.Fatalf("resolveDeploySourceInfo returned error: %v", err)
	}
	if info.BuildImageTag != "ci-123" {
		t.Fatalf("BuildImageTag = %q, want trimmed explicit revision", info.BuildImageTag)
	}
	if info.StateSource != "./app" {
		t.Fatalf("StateSource = %q, want trimmed source", info.StateSource)
	}
}

func TestDeployStartNotificationMessageOmitsEmptyCommitMessageSuffix(t *testing.T) {
	got := deployStartNotificationMessage("demo", "1.0.0", "production", "Revision", "source-20260705T043456Z", "")
	want := "Starting deployment of `demo` v1.0.0 to `production`\nRevision: `source-20260705T043456Z`"
	if got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestDeployStartNotificationMessageIncludesCommitMessage(t *testing.T) {
	got := deployStartNotificationMessage("demo", "1.0.0", "production", "Commit", "abc123", "deploy me")
	want := "Starting deployment of `demo` v1.0.0 to `production`\nCommit: `abc123` - deploy me"
	if got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestResolveDeploySourceInfoSourceModeBypassesGitAndGeneratesTag(t *testing.T) {
	now := time.Date(2026, 7, 5, 4, 34, 56, 0, time.UTC)
	info, err := resolveDeploySourceInfo(fakeDeployGitReader{}, false, ".", "", "", now)
	if err != nil {
		t.Fatalf("resolveDeploySourceInfo returned error: %v", err)
	}
	if !info.SourceMode {
		t.Fatal("SourceMode = false, want true")
	}
	if info.CommitInfo != nil {
		t.Fatalf("CommitInfo = %#v, want nil", info.CommitInfo)
	}
	if info.BuildImageTag != "source-20260705T043456Z" {
		t.Fatalf("BuildImageTag = %q, want generated source tag", info.BuildImageTag)
	}
	if info.StateSource != "." {
		t.Fatalf("StateSource = %q, want source label", info.StateSource)
	}
	gitFields := deployGitStringsFromCommit(info.CommitInfo)
	if gitFields != (deployGitStrings{}) {
		t.Fatalf("git fields = %#v, want empty", gitFields)
	}
}

func TestResolveDeploySourceInfoRevisionModeUsesExplicitTag(t *testing.T) {
	info, err := resolveDeploySourceInfo(fakeDeployGitReader{}, false, "", "ci-123", "", time.Now())
	if err != nil {
		t.Fatalf("resolveDeploySourceInfo returned error: %v", err)
	}
	if info.BuildImageTag != "ci-123" {
		t.Fatalf("BuildImageTag = %q, want explicit revision", info.BuildImageTag)
	}
	if info.CommitInfo != nil {
		t.Fatalf("CommitInfo = %#v, want nil", info.CommitInfo)
	}
}

func TestResolveDeploySourceInfoRejectsInvalidExplicitRevision(t *testing.T) {
	_, err := resolveDeploySourceInfo(fakeDeployGitReader{}, false, ".", "bad/tag", "", time.Now())
	if err == nil {
		t.Fatal("resolveDeploySourceInfo should reject invalid revision")
	}
	if !strings.Contains(err.Error(), "build tag") {
		t.Fatalf("error = %q, want build tag validation", err)
	}
}

func TestResolveDeployCommitInfoRejectsDirtyWorktree(t *testing.T) {
	reader := fakeDeployGitReader{
		repository: true,
		dirty:      true,
		status:     " M main.go\n?? new.txt\n",
	}

	_, _, err := resolveDeployCommitInfo(reader, false)
	if err == nil {
		t.Fatal("resolveDeployCommitInfo should reject dirty worktrees")
	}
	for _, want := range []string{"cannot deploy with uncommitted changes", "commit, stash, or discard", "M main.go", "?? new.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestResolveDeployCommitInfoWrapsGitStatusCheckError(t *testing.T) {
	reader := fakeDeployGitReader{
		repository: true,
		dirtyErr:   errors.New("git unavailable"),
	}

	_, _, err := resolveDeployCommitInfo(reader, false)
	if err == nil {
		t.Fatal("resolveDeployCommitInfo should return git status check error")
	}
	for _, want := range []string{"failed to check git status", "git unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestResolveDeployCommitInfoWrapsDirtyStatusError(t *testing.T) {
	reader := fakeDeployGitReader{
		repository: true,
		dirty:      true,
		statusErr:  errors.New("status failed"),
	}

	_, _, err := resolveDeployCommitInfo(reader, true)
	if err == nil {
		t.Fatal("resolveDeployCommitInfo should return dirty status error")
	}
	for _, want := range []string{"failed to get git status", "status failed"} {
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

	got, dirtyStatus, err := resolveDeployCommitInfo(reader, false)
	if err != nil {
		t.Fatalf("resolveDeployCommitInfo returned error: %v", err)
	}
	if got != want {
		t.Fatalf("commitInfo = %#v, want %#v", got, want)
	}
	if dirtyStatus != "" {
		t.Fatalf("dirtyStatus = %q, want empty", dirtyStatus)
	}
}

func TestResolveDeployCommitInfoRequiresGitRepository(t *testing.T) {
	_, _, err := resolveDeployCommitInfo(fakeDeployGitReader{}, false)
	if err == nil {
		t.Fatal("resolveDeployCommitInfo should reject non-git repositories")
	}
	if !strings.Contains(err.Error(), "not a Git repository") {
		t.Fatalf("error = %q, want git repository guidance", err)
	}
}

func TestResolveDeployCommitInfoAllowsDirtyWorktreeWhenExplicit(t *testing.T) {
	want := &git.CommitInfo{
		Hash:      "abcdef",
		ShortHash: "abc",
		Branch:    "feature",
		Message:   "deploy test",
		Author:    "redentor",
	}
	reader := fakeDeployGitReader{
		repository: true,
		dirty:      true,
		status:     " M .dockerignore\n",
		commitInfo: want,
	}

	got, dirtyStatus, err := resolveDeployCommitInfo(reader, true)
	if err != nil {
		t.Fatalf("resolveDeployCommitInfo returned error: %v", err)
	}
	if got != want {
		t.Fatalf("commitInfo = %#v, want %#v", got, want)
	}
	if dirtyStatus != "M .dockerignore" {
		t.Fatalf("dirtyStatus = %q, want dirty file list", dirtyStatus)
	}
}

func TestResolveDeployCommitInfoUsesDirtyLabelForBlankStatus(t *testing.T) {
	want := &git.CommitInfo{Hash: "abcdef", ShortHash: "abc"}
	reader := fakeDeployGitReader{
		repository: true,
		dirty:      true,
		status:     " \n\t ",
		commitInfo: want,
	}

	got, dirtyStatus, err := resolveDeployCommitInfo(reader, true)
	if err != nil {
		t.Fatalf("resolveDeployCommitInfo returned error: %v", err)
	}
	if got != want {
		t.Fatalf("commitInfo = %#v, want %#v", got, want)
	}
	if dirtyStatus != "(dirty worktree)" {
		t.Fatalf("dirtyStatus = %q, want fallback dirty label", dirtyStatus)
	}
}

func TestRecordFailedDeploymentStatePersistsRemoteAndLocalFailure(t *testing.T) {
	start := time.Now().Add(-2 * time.Second)
	remoteSaver := &fakeRemoteDeploymentSaver{}
	localSaver := &fakeLocalDeploymentSaver{}
	deployment := &remotestate.DeploymentState{
		Timestamp: start,
		Services:  map[string]remotestate.ServiceState{},
	}
	cfg := &config.Config{
		Runtime: &config.RuntimeConfig{Mode: config.RuntimeModeTakod},
	}
	commit := &git.CommitInfo{Hash: "abcdef"}
	deployErr := errors.New("web failed")

	err := recordFailedDeploymentState(remoteSaver, localSaver, deployment, cfg, "production", []string{"node-a", "node-b"}, commit, start, deployErr)
	if err != nil {
		t.Fatalf("recordFailedDeploymentState returned error: %v", err)
	}
	if remoteSaver.saved == nil {
		t.Fatal("remote deployment was not saved")
	}
	if remoteSaver.saved.Status != remotestate.StatusFailed {
		t.Fatalf("remote status = %q, want failed", remoteSaver.saved.Status)
	}
	if remoteSaver.saved.Error != "web failed" {
		t.Fatalf("remote error = %q, want deployment error", remoteSaver.saved.Error)
	}
	if remoteSaver.saved.Duration <= 0 {
		t.Fatalf("remote duration = %s, want positive duration", remoteSaver.saved.Duration)
	}
	if localSaver.saved == nil {
		t.Fatal("local deployment was not saved")
	}
	if localSaver.saved.Status != "failed" {
		t.Fatalf("local status = %q, want failed", localSaver.saved.Status)
	}
	if localSaver.saved.GitCommit != "abcdef" {
		t.Fatalf("local git commit = %q, want abcdef", localSaver.saved.GitCommit)
	}
	if got := strings.Join(localSaver.saved.Servers, ","); got != "node-a,node-b" {
		t.Fatalf("local servers = %q, want node-a,node-b", got)
	}
}

func TestRecordFailedDeploymentStateReturnsRemoteSaveError(t *testing.T) {
	remoteSaver := &fakeRemoteDeploymentSaver{err: errors.New("disk full")}
	deployment := &remotestate.DeploymentState{
		Timestamp: time.Now(),
		Services:  map[string]remotestate.ServiceState{},
	}
	cfg := &config.Config{Runtime: &config.RuntimeConfig{Mode: config.RuntimeModeTakod}}

	err := recordFailedDeploymentState(remoteSaver, nil, deployment, cfg, "production", []string{"node-a"}, nil, time.Now(), errors.New("deploy failed"))
	if err == nil {
		t.Fatal("recordFailedDeploymentState should return remote save errors")
	}
	if !strings.Contains(err.Error(), "failed to save failed remote deployment state") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %q, want remote save context", err)
	}
}

func TestRetiredDeploymentServersDetectsRemovedNodes(t *testing.T) {
	got := retiredDeploymentServers(
		[]string{"node-b", "node-a", "node-b", "node-c", ""},
		[]string{"node-a"},
	)
	want := []string{"node-b", "node-c"}
	if !slices.Equal(got, want) {
		t.Fatalf("retiredDeploymentServers() = %#v, want %#v", got, want)
	}
}

func TestRetiredDeploymentServersIgnoresUnchangedNodes(t *testing.T) {
	got := retiredDeploymentServers(
		[]string{"node-a", "node-b"},
		[]string{"node-b", "node-a", "node-c"},
	)
	if len(got) != 0 {
		t.Fatalf("retiredDeploymentServers() = %#v, want none", got)
	}
}

func TestApplyDeployRemovalsCallsRemoveChangesOnly(t *testing.T) {
	remover := &fakeDeployServiceRemover{}
	plan := &reconcile.ReconciliationPlan{
		Changes: []reconcile.ServiceChange{
			{Type: reconcile.ChangeNone, ServiceName: "web"},
			{Type: reconcile.ChangeRemove, ServiceName: "old-api"},
			{Type: reconcile.ChangeUpdate, ServiceName: "worker"},
			{Type: reconcile.ChangeRemove, ServiceName: "old-worker"},
		},
	}

	if err := applyDeployRemovals(remover, plan); err != nil {
		t.Fatalf("applyDeployRemovals returned error: %v", err)
	}
	if got := strings.Join(remover.removed, ","); got != "old-api,old-worker" {
		t.Fatalf("removed services = %q, want old-api,old-worker", got)
	}
}

func TestApplyDeployRemovalsReturnsServiceContext(t *testing.T) {
	remover := &fakeDeployServiceRemover{err: errors.New("node failed")}
	plan := &reconcile.ReconciliationPlan{
		Changes: []reconcile.ServiceChange{
			{Type: reconcile.ChangeRemove, ServiceName: "old-api"},
		},
	}

	err := applyDeployRemovals(remover, plan)
	if err == nil {
		t.Fatal("applyDeployRemovals returned nil, want error")
	}
	if !strings.Contains(err.Error(), "old-api") || !strings.Contains(err.Error(), "node failed") {
		t.Fatalf("error = %q, want service and cause", err)
	}
}

func TestDeploymentSuccessStatusReturnsWarmedForManualPending(t *testing.T) {
	if status := deploymentSuccessStatus([]string{"web"}); status != remotestate.StatusWarmed {
		t.Fatalf("status = %q, want warmed", status)
	}
	if status := deploymentSuccessStatus(nil); status != remotestate.StatusSuccess {
		t.Fatalf("status = %q, want success", status)
	}
}

func TestPruneTakodServiceRevisionsAfterGraceSleepsBeforePrune(t *testing.T) {
	pruner := &fakeTakodRevisionPruner{}
	services := map[string]config.ServiceConfig{
		"web": {
			Deploy: config.DeployConfig{
				Strategy:    config.DeployStrategyBlueGreen,
				GracePeriod: "250ms",
			},
		},
	}
	keepRevisions := map[string]string{"web": "rev-web"}
	var slept time.Duration
	originalSleep := blueGreenGraceSleep
	blueGreenGraceSleep = func(duration time.Duration) {
		slept = duration
		if pruner.called {
			t.Fatal("prune was called before grace sleep")
		}
	}
	t.Cleanup(func() {
		blueGreenGraceSleep = originalSleep
	})

	if err := pruneTakodServiceRevisionsAfterGrace(pruner, services, keepRevisions); err != nil {
		t.Fatalf("pruneTakodServiceRevisionsAfterGrace returned error: %v", err)
	}
	if slept != 250*time.Millisecond {
		t.Fatalf("slept = %s, want 250ms", slept)
	}
	if !pruner.called {
		t.Fatal("expected prune to be called after grace sleep")
	}
	if pruner.keepRevisions["web"] != "rev-web" {
		t.Fatalf("keep revisions = %#v, want web rev-web", pruner.keepRevisions)
	}
}

type deployTestArchiveEntry struct {
	name     string
	body     string
	dir      bool
	symlink  string
	typeflag byte
}

func writeDeployTestFile(t *testing.T, name string, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readDeployTestFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func writeDeployTestTarGz(t *testing.T, entries []deployTestArchiveEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.tar.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: 0o644}
		switch {
		case entry.dir:
			header.Typeflag = tar.TypeDir
			header.Mode = 0o755
		case entry.symlink != "":
			header.Typeflag = tar.TypeSymlink
			header.Linkname = entry.symlink
		case entry.typeflag != 0:
			header.Typeflag = entry.typeflag
			header.Size = int64(len(entry.body))
		default:
			header.Typeflag = tar.TypeReg
			header.Size = int64(len(entry.body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg || entry.typeflag != 0 {
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeDeployTestTarGzRaw(t *testing.T, entries []deployTestArchiveEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "raw.tar.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		writeDeployTestRawTarEntry(t, gz, entry.name, []byte(entry.body), typeflag)
	}
	if _, err := gz.Write(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeDeployTestRawTarEntry(t *testing.T, writer *gzip.Writer, name string, body []byte, typeflag byte) {
	t.Helper()
	header := make([]byte, 512)
	copy(header[0:100], []byte(name))
	copy(header[100:108], []byte("0000644\x00"))
	copy(header[108:116], []byte("0000000\x00"))
	copy(header[116:124], []byte("0000000\x00"))
	copy(header[124:136], []byte(fmt.Sprintf("%011o\x00", len(body))))
	copy(header[136:148], []byte("00000000000\x00"))
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}
	header[156] = typeflag
	copy(header[257:263], []byte("ustar\x00"))
	copy(header[263:265], []byte("00"))
	checksum := 0
	for _, b := range header {
		checksum += int(b)
	}
	copy(header[148:156], []byte(fmt.Sprintf("%06o\x00 ", checksum)))
	if _, err := writer.Write(header); err != nil {
		t.Fatal(err)
	}
	if len(body) > 0 {
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
		padding := (512 - len(body)%512) % 512
		if padding > 0 {
			if _, err := writer.Write(make([]byte, padding)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func writeDeployTestZip(t *testing.T, entries []deployTestArchiveEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	for _, entry := range entries {
		if entry.dir {
			if _, err := zw.Create(entry.name); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if entry.symlink != "" {
			header := &zip.FileHeader{Name: entry.name}
			header.SetMode(os.ModeSymlink | 0o777)
			writer, err := zw.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte(entry.symlink)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		writer, err := zw.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

type fakeDeployGitReader struct {
	repository bool
	dirty      bool
	dirtyErr   error
	status     string
	statusErr  error
	commitInfo *git.CommitInfo
}

func (f fakeDeployGitReader) IsRepository() bool {
	return f.repository
}

func (f fakeDeployGitReader) HasUncommittedChanges() (bool, error) {
	if f.dirtyErr != nil {
		return false, f.dirtyErr
	}
	return f.dirty, nil
}

func (f fakeDeployGitReader) GetStatus() (string, error) {
	if f.statusErr != nil {
		return "", f.statusErr
	}
	return f.status, nil
}

func (f fakeDeployGitReader) GetCommitInfo(_ string) (*git.CommitInfo, error) {
	if f.commitInfo == nil {
		return nil, errors.New("missing commit")
	}
	return f.commitInfo, nil
}

type fakeRemoteDeploymentSaver struct {
	saved *remotestate.DeploymentState
	err   error
}

func (f *fakeRemoteDeploymentSaver) SaveDeployment(deployment *remotestate.DeploymentState) error {
	if f.err != nil {
		return f.err
	}
	f.saved = deployment
	return nil
}

type fakeLocalDeploymentSaver struct {
	saved *localstate.DeploymentState
	err   error
}

func (f *fakeLocalDeploymentSaver) SaveDeployment(deployment *localstate.DeploymentState) error {
	if f.err != nil {
		return f.err
	}
	f.saved = deployment
	return nil
}

type fakeDeployServiceRemover struct {
	removed []string
	err     error
}

func (f *fakeDeployServiceRemover) RemoveServiceTakod(serviceName string) error {
	if f.err != nil {
		return f.err
	}
	f.removed = append(f.removed, serviceName)
	return nil
}

type fakeTakodRevisionPruner struct {
	called        bool
	services      map[string]config.ServiceConfig
	keepRevisions map[string]string
	err           error
}

func (f *fakeTakodRevisionPruner) PruneTakodServiceRevisions(services map[string]config.ServiceConfig, keepRevisions map[string]string) error {
	f.called = true
	f.services = services
	f.keepRevisions = keepRevisions
	return f.err
}
