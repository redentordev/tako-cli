package takod

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
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
		if !imageRepositoryMatchesProject(repo, "demo") {
			t.Fatalf("expected repository %q to match project", repo)
		}
	}

	for _, repo := range []string{
		"demo-app/web",
		"company/demo-web",
		"registry.example.com/notdemo/web",
	} {
		if imageRepositoryMatchesProject(repo, "demo") {
			t.Fatalf("expected repository %q not to match project", repo)
		}
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
