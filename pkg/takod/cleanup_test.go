package takod

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestValidateCleanupRequest(t *testing.T) {
	valid := CleanupRequest{
		Project:     "demo-app",
		Environment: "production_1",
		ProxyFiles:  []string{"demo-production.yml"},
	}
	if err := validateCleanupRequest(valid); err != nil {
		t.Fatalf("valid cleanup request returned error: %v", err)
	}

	invalid := valid
	invalid.Project = "../demo"
	if err := validateCleanupRequest(invalid); err == nil {
		t.Fatalf("expected unsafe project to be rejected")
	}

	invalid = valid
	invalid.Environment = "prod;rm"
	if err := validateCleanupRequest(invalid); err == nil {
		t.Fatalf("expected unsafe environment to be rejected")
	}

	invalid = valid
	invalid.ProxyFiles = []string{"../demo.yml"}
	if err := validateCleanupRequest(invalid); err == nil {
		t.Fatalf("expected unsafe proxy file to be rejected")
	}
}

func TestImageRepositoryMatchesProject(t *testing.T) {
	for _, repo := range []string{
		"demo/web",
		"demo",
		"registry.example.com/demo/web",
		"localhost:5000/demo/web",
	} {
		if !imageRepositoryMatchesProject(repo, "demo", nil) {
			t.Fatalf("expected repository %q to match project", repo)
		}
	}

	for _, repo := range []string{
		"demo-app/web",
		"company/demo-web",
		"registry.example.com/notdemo/web",
	} {
		if imageRepositoryMatchesProject(repo, "demo", nil) {
			t.Fatalf("expected repository %q not to match project", repo)
		}
	}
}

func TestImageRepositoryMatchesExactAllowlist(t *testing.T) {
	allowed := []string{"registry.example.com/demo/web", "registry.example.com/demo/api"}
	if !imageRepositoryMatchesProject("registry.example.com/demo/web", "demo", allowed) {
		t.Fatal("expected exact allowlisted repository to match")
	}
	for _, repo := range []string{
		"demo/web",
		"registry.example.com/demo/unrelated",
		"registry.example.com/demo/web-extra",
	} {
		if imageRepositoryMatchesProject(repo, "demo", allowed) {
			t.Fatalf("expected repository %q not to match exact allowlist", repo)
		}
	}
}

func TestValidateCleanupRequestRejectsUnsafeImageRepository(t *testing.T) {
	if err := validateCleanupRequest(CleanupRequest{
		Project:           "demo",
		ImageRepositories: []string{"localhost:5000/demo/web"},
	}); err != nil {
		t.Fatalf("expected registry repository with port to be accepted: %v", err)
	}

	err := validateCleanupRequest(CleanupRequest{
		Project:           "demo",
		ImageRepositories: []string{"demo/web:latest"},
	})
	if err == nil {
		t.Fatal("expected tagged image repository to be rejected")
	}
}

func TestCleanupRejectsNegativeKeepImages(t *testing.T) {
	_, err := CleanupProject(context.Background(), CleanupRequest{
		Project:        "demo",
		KeepImages:     -1,
		CleanOldImages: true,
	})
	if err == nil {
		t.Fatal("expected negative keepImages to be rejected")
	}
}

func TestCleanupRequestIncludesMaintenanceCleanup(t *testing.T) {
	if (CleanupRequest{Project: "demo"}).includesMaintenanceCleanup() {
		t.Fatal("expected empty cleanup request not to include maintenance cleanup")
	}
	if !(CleanupRequest{Project: "demo", CleanStoppedContainers: true}).includesMaintenanceCleanup() {
		t.Fatal("expected stopped-container cleanup to count as maintenance cleanup")
	}
	if !(CleanupRequest{Project: "demo", PruneDocker: true}).includesMaintenanceCleanup() {
		t.Fatal("expected Docker prune to count as maintenance cleanup")
	}
}

func TestUniqueFields(t *testing.T) {
	got := uniqueFields("a\nb\na\tc\n")
	want := []string{"a", "b", "c"}
	if !slices.Equal(got, want) {
		t.Fatalf("uniqueFields() = %#v, want %#v", got, want)
	}
}

func TestCleanupUnusedProjectVolumesScopesToProjectEnvironment(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	target := runtimeid.VolumeName("demo", "production", "data")
	otherStage := runtimeid.VolumeName("demo", "preview", "data")
	otherProject := runtimeid.VolumeName("other", "production", "data")
	t.Setenv("TAKO_FAKE_VOLUME_LS_OUTPUT", strings.Join([]string{target, otherStage, otherProject}, "\n")+"\n")

	removed, err := cleanupUnusedProjectVolumes(context.Background(), "demo", "production")
	if err != nil {
		t.Fatalf("cleanupUnusedProjectVolumes returned error: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}

	entries := readCommandLog(t, logPath)
	want := "docker volume rm " + target
	if !slices.Contains(entries, want) {
		t.Fatalf("docker log missing %q in %#v", want, entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, "volume rm "+otherStage) || strings.Contains(entry, "volume rm "+otherProject) {
			t.Fatalf("cleanup removed unrelated volume via %q; all entries %#v", entry, entries)
		}
	}
}

func TestCleanupUnusedProjectVolumesSkipsInUseVolumes(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	target := runtimeid.VolumeName("demo", "production", "data")
	t.Setenv("TAKO_FAKE_VOLUME_LS_OUTPUT", target+"\n")
	t.Setenv("TAKO_FAKE_FAIL_VOLUME_RM", target)

	removed, err := cleanupUnusedProjectVolumes(context.Background(), "demo", "production")
	if err != nil {
		t.Fatalf("cleanupUnusedProjectVolumes should skip in-use volumes, got error: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
}

func TestScopedProjectPruneDoesNotRunGlobalDockerSystemPrune(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	response, err := CleanupProject(context.Background(), CleanupRequest{
		Project:     "demo",
		Environment: "production",
		PruneDocker: true,
	})
	if err != nil {
		t.Fatalf("CleanupProject returned error: %v", err)
	}
	if len(response.Warnings) > 0 {
		t.Fatalf("CleanupProject warnings = %#v", response.Warnings)
	}

	entries := readCommandLog(t, logPath)
	for _, entry := range entries {
		if strings.Contains(entry, "docker system prune") || strings.Contains(entry, "docker volume prune") {
			t.Fatalf("scoped prune should not run global prune command %q; all entries %#v", entry, entries)
		}
		if strings.Contains(entry, "docker images --format") {
			t.Fatalf("environment-scoped prune should not remove project images across stages via %q; all entries %#v", entry, entries)
		}
	}
}

func TestSecureProxyLogPermissionsSetsModes(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "archive")
	if err := os.Mkdir(subdir, 0777); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}
	logPath := filepath.Join(subdir, "access.log")
	if err := os.WriteFile(logPath, []byte("ok\n"), 0666); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	if err := secureProxyLogPermissions(root); err != nil {
		t.Fatalf("secureProxyLogPermissions returned error: %v", err)
	}

	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatalf("failed to stat root: %v", err)
	}
	if got := rootInfo.Mode().Perm(); got != 0750 {
		t.Fatalf("root mode = %v, want 0750", got)
	}

	logInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("failed to stat log: %v", err)
	}
	if got := logInfo.Mode().Perm(); got != 0640 {
		t.Fatalf("log mode = %v, want 0640", got)
	}
}
