package takod

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestValidateCleanupRequest(t *testing.T) {
	valid := CleanupRequest{
		Project:     "demo-app",
		Environment: "production_1",
		ProxyFiles:  []string{"demo-production.json"},
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
	invalid.ProxyFiles = []string{"../demo.json"}
	if err := validateCleanupRequest(invalid); err == nil {
		t.Fatalf("expected unsafe proxy file to be rejected")
	}
}

func TestCleanupFenceCannotDeleteAnotherProjectsProxyManifest(t *testing.T) {
	useTempProxyPaths(t)
	if err := os.MkdirAll(proxyRoutesDir, 0700); err != nil {
		t.Fatal(err)
	}
	name := runtimeid.ProxyConfigFileName("beta", "production")
	content := `{"version":1,"project":"beta","environment":"production","routes":[{"service":"web","domains":["beta.example.com"],"upstreams":["http://web:3000"]}]}`
	path := filepath.Join(proxyRoutesDir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := withOperationFence(context.Background(), nodeidentity.OperationFence{Project: "alpha", Environment: "production"})
	if _, err := CleanupProject(ctx, CleanupRequest{Project: "alpha", Environment: "production", ProxyFiles: []string{name}}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("cross-project cleanup error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cross-project proxy manifest was removed: %v", err)
	}
}

func TestCleanupWithdrawsProxyRoutesBeforeStoppingContainers(t *testing.T) {
	useTempProxyPaths(t)
	name := runtimeid.ProxyConfigFileName("demo", "production")
	content := `{"version":1,"project":"demo","environment":"production","routes":[{"service":"web","domains":["example.com"],"upstreams":["http://web:3000"]}]}`
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: name, Content: content}); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PROJECT_PS_OUTPUT", "worker123\n")

	response, err := CleanupProject(context.Background(), CleanupRequest{
		Project: "demo", Environment: "production", ProxyFiles: []string{name}, RemoveContainers: true,
	})
	if err != nil {
		t.Fatalf("CleanupProject returned error: %v", err)
	}
	if response.ProxyFilesRemoved != 1 || response.ContainersRemoved != 1 {
		t.Fatalf("cleanup response = %#v", response)
	}
	entries := readCommandLog(t, logPath)
	proxyProbe := slices.Index(entries, "docker ps --filter name=^tako-proxy$ --format {{.Names}}")
	containerList := slices.Index(entries, "docker ps -aq --filter label=tako.project=demo --filter label=tako.environment=production")
	stop := slices.Index(entries, "docker stop worker123")
	if proxyProbe < 0 || containerList <= proxyProbe || stop <= containerList {
		t.Fatalf("route withdrawal must precede container stop: %#v", entries)
	}
}

func TestCleanupStopsBeforeDeletingStateWhenContainerDrainFails(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PROJECT_PS_OUTPUT", "worker123\n")
	t.Setenv("TAKO_FAKE_DOCKER_STOP_ERROR", "worker refused stop")

	response, err := CleanupProject(context.Background(), CleanupRequest{
		Project: "demo", Environment: "production", RemoveContainers: true, RemoveNetworks: true, RemoveTakodState: true,
	})
	if err == nil || !strings.Contains(err.Error(), "gracefully stop") {
		t.Fatalf("CleanupProject error = %v, want graceful stop failure", err)
	}
	if response == nil {
		t.Fatal("CleanupProject should return partial response with the error")
	}
	entries := readCommandLog(t, logPath)
	for _, entry := range entries {
		if strings.HasPrefix(entry, "docker network ls") {
			t.Fatalf("cleanup continued after failed container drain: %#v", entries)
		}
	}
}

func TestCleanupDoesNotStopContainersWhenRouteWithdrawalFails(t *testing.T) {
	useTempProxyPaths(t)
	name := runtimeid.ProxyConfigFileName("demo", "production")
	content := `{"version":1,"project":"demo","environment":"production","routes":[{"service":"web","domains":["example.com"],"upstreams":["http://web:3000"]}]}`
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: name, Content: content}); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "tako-proxy\n")
	t.Setenv("TAKO_FAKE_INSPECT_ARGS", currentProxyArgsJSON(defaultProxyEmail))
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_ERROR", "invalid caddy config")
	t.Setenv("TAKO_FAKE_PROJECT_PS_OUTPUT", "worker123\n")

	if _, err := CleanupProject(context.Background(), CleanupRequest{
		Project: "demo", Environment: "production", ProxyFiles: []string{name}, RemoveContainers: true,
	}); err == nil || !strings.Contains(err.Error(), "withdraw proxy") {
		t.Fatalf("CleanupProject error = %v, want route withdrawal failure", err)
	}
	for _, entry := range readCommandLog(t, logPath) {
		if strings.HasPrefix(entry, "docker stop worker123") {
			t.Fatalf("container stopped despite failed route withdrawal")
		}
	}
}

func TestHandleCleanupValidatesBeforeDeletingSchedules(t *testing.T) {
	dataDir := t.TempDir()
	server := NewServer(filepath.Join(t.TempDir(), "takod.sock"), dataDir, "test")
	schedule := testBackupScheduleRequest()
	if _, err := server.backupScheduler.Upsert(context.Background(), schedule); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/cleanup", bytes.NewBufferString(`{"project":"demo","environment":"production","removeContainers":true,"proxyFiles":["../unsafe.json"]}`))
	recorder := httptest.NewRecorder()
	server.handleCleanup(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("cleanup status = %d, want 400", recorder.Code)
	}
	if _, err := os.Stat(backupSchedulePath(dataDir, schedule)); err != nil {
		t.Fatalf("invalid cleanup deleted backup schedule: %v", err)
	}
}

func TestHandleCleanupKeepsSchedulesWhenRouteWithdrawalFails(t *testing.T) {
	useTempProxyPaths(t)
	dataDir := t.TempDir()
	server := NewServer(filepath.Join(t.TempDir(), "takod.sock"), dataDir, "test")
	schedule := testBackupScheduleRequest()
	if _, err := server.backupScheduler.Upsert(context.Background(), schedule); err != nil {
		t.Fatal(err)
	}
	name := runtimeid.ProxyConfigFileName("demo", "production")
	content := `{"version":1,"project":"demo","environment":"production","routes":[{"service":"web","domains":["example.com"],"upstreams":["http://web:3000"]}]}`
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: name, Content: content}); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "tako-proxy\n")
	t.Setenv("TAKO_FAKE_INSPECT_ARGS", currentProxyArgsJSON(defaultProxyEmail))
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_ERROR", "reload failed")
	req := httptest.NewRequest(http.MethodPost, "/v1/cleanup", bytes.NewBufferString(`{"project":"demo","environment":"production","removeContainers":true,"proxyFiles":["`+name+`"]}`))
	recorder := httptest.NewRecorder()
	server.handleCleanup(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("cleanup status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(backupSchedulePath(dataDir, schedule)); err != nil {
		t.Fatalf("failed cleanup deleted backup schedule: %v", err)
	}
	if _, ok := server.backupScheduler.entries[backupScheduleKey(schedule)]; !ok {
		t.Fatal("failed cleanup unscheduled backup")
	}
}

func TestImageRepositoryMatchesProject(t *testing.T) {
	for _, repo := range []string{
		"demo/web",
		"demo",
		"demo-app/web",
		"company/demo-web",
		"registry.example.com/notdemo/web",
		"registry.example.com/demo/web",
		"localhost:5000/demo/web",
		"getsentry/sentry",
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

func TestValidateCleanupRequestRejectsUnsafeBuildCacheKeepStorage(t *testing.T) {
	err := validateCleanupRequest(CleanupRequest{
		Project:               "demo",
		CleanBuildCache:       true,
		BuildCacheKeepStorage: "--all",
	})
	if err == nil {
		t.Fatal("expected unsafe build cache keep storage to be rejected")
	}

	err = validateCleanupRequest(CleanupRequest{
		Project:               "demo",
		CleanBuildCache:       true,
		BuildCacheKeepStorage: "10 GB",
	})
	if err == nil {
		t.Fatal("expected whitespace in build cache keep storage to be rejected")
	}
}

func TestUniqueFields(t *testing.T) {
	got := uniqueFields("a\nb\na\tc\n")
	want := []string{"a", "b", "c"}
	if !slices.Equal(got, want) {
		t.Fatalf("uniqueFields() = %#v, want %#v", got, want)
	}
}

func TestCleanupDanglingImagesUsesDockerImagePrune(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	t.Setenv("TAKO_FAKE_DANGLING_IMAGE_IDS", "sha256:a\nsha256:b\nsha256:a\n")

	removed, err := cleanupDanglingImages(context.Background())
	if err != nil {
		t.Fatalf("cleanupDanglingImages returned error: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}

	entries := readCommandLog(t, logPath)
	listWant := "docker images -f dangling=true --filter label!=" + composeProjectImageLabel + " -q"
	if !slices.Contains(entries, listWant) {
		t.Fatalf("docker log missing %q in %#v", listWant, entries)
	}
	want := "docker image prune -f --filter label!=" + composeProjectImageLabel
	if !slices.Contains(entries, want) {
		t.Fatalf("docker log missing %q in %#v", want, entries)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry, "docker rmi ") {
			t.Fatalf("cleanup should not remove dangling image IDs directly via %q; all entries %#v", entry, entries)
		}
	}
}

func TestCleanupImagesExcludesComposeOwnedImages(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	// The fake output represents Docker's already-filtered candidate list.
	t.Setenv("TAKO_FAKE_IMAGE_LIST_OUTPUT", "tako-id\tdemo/web\ttako\n")

	removed, err := cleanupImages(context.Background(), "demo", []string{"demo/web"})
	if err != nil {
		t.Fatalf("cleanupImages returned error: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	entries := readCommandLog(t, logPath)
	listWant := "docker images --filter label!=" + composeProjectImageLabel + " --format {{.ID}}\t{{.Repository}}\t{{.Tag}}"
	if !slices.Contains(entries, listWant) {
		t.Fatalf("docker log missing Compose exclusion %q in %#v", listWant, entries)
	}
	if !slices.Contains(entries, "docker rmi -f demo/web:tako") {
		t.Fatalf("docker log missing Tako image removal in %#v", entries)
	}
}

func TestCleanupImagesRemovesOnlyAllowlistedTagWhenImageIDIsShared(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	t.Setenv("TAKO_FAKE_IMAGE_LIST_OUTPUT", strings.Join([]string{
		"shared-id\tdemo/web\ttako",
		"shared-id\tunrelated/cache\tlatest",
	}, "\n")+"\n")
	removed, err := cleanupImages(context.Background(), "demo", []string{"demo/web"})
	if err != nil {
		t.Fatalf("cleanupImages returned error: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	entries := readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker rmi -f demo/web:tako") {
		t.Fatalf("docker log missing allowlisted tag removal in %#v", entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, "unrelated/cache") || strings.Contains(entry, "rmi -f shared-id") {
			t.Fatalf("cleanup exceeded allowlist via %q", entry)
		}
	}
}

func TestCleanupOldProjectImagesExcludesComposeOwnedImages(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	t.Setenv("TAKO_FAKE_IMAGE_LIST_OUTPUT", strings.Join([]string{
		"tako-new\tdemo/web\tnew",
		"tako-old\tdemo/web\told",
	}, "\n")+"\n")

	removed, err := cleanupOldProjectImages(context.Background(), "demo", 1, []string{"demo/web"})
	if err != nil {
		t.Fatalf("cleanupOldProjectImages returned error: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	entries := readCommandLog(t, logPath)
	listWant := "docker images --filter label!=" + composeProjectImageLabel + " --format {{.ID}}\t{{.Repository}}\t{{.Tag}}"
	if !slices.Contains(entries, listWant) {
		t.Fatalf("docker log missing Compose exclusion %q in %#v", listWant, entries)
	}
	if !slices.Contains(entries, "docker rmi -f demo/web:old") {
		t.Fatalf("docker log missing Tako image removal in %#v", entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, "rmi") && strings.Contains(entry, "compose-id") {
			t.Fatalf("cleanup removed Compose-owned image via %q", entry)
		}
	}
}

func TestCleanupImagesFailsClosedWhenComposeImageInventoryFails(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	t.Setenv("TAKO_FAKE_IMAGE_LIST_ERROR", "inventory failed")
	if _, err := cleanupImages(context.Background(), "demo", []string{"demo/web"}); err == nil || !strings.Contains(err.Error(), "list docker images") {
		t.Fatalf("expected image inventory error, got %v", err)
	}
	for _, entry := range readCommandLog(t, logPath) {
		if strings.HasPrefix(entry, "docker rmi ") {
			t.Fatalf("cleanup should fail before removal, got %q", entry)
		}
	}
}

func TestCleanupImagesWithoutExplicitAllowlistRemovesNothing(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	t.Setenv("TAKO_FAKE_IMAGE_LIST_OUTPUT", strings.Join([]string{
		"upstream\tpostgres\tlatest",
		"namespace\tgetsentry/sentry\tlatest",
		"local\tdemo/web\tlatest",
	}, "\n")+"\n")
	removed, err := cleanupImages(context.Background(), "postgres", nil)
	if err != nil {
		t.Fatalf("cleanupImages returned error: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if _, err := os.Stat(logPath); err == nil {
		for _, entry := range readCommandLog(t, logPath) {
			if strings.HasPrefix(entry, "docker rmi ") {
				t.Fatalf("cleanup without allowlist removed an image via %q", entry)
			}
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("failed to stat command log: %v", err)
	}
}

func TestCountDockerImagePruneEntriesIgnoresNoopOutput(t *testing.T) {
	output := "Total reclaimed space: 0B\n"
	if got := countDockerImagePruneEntries(output); got != 0 {
		t.Fatalf("countDockerImagePruneEntries() = %d, want 0", got)
	}
}

func TestCleanupBuildCacheUsesDefaultKeepStorage(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	if _, err := cleanupBuildCache(context.Background(), ""); err != nil {
		t.Fatalf("cleanupBuildCache returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	want := "docker builder prune -f --keep-storage " + DefaultBuildCacheKeepStorage
	if !slices.Contains(entries, want) {
		t.Fatalf("docker log missing %q in %#v", want, entries)
	}
}

func TestCleanupBuildCacheUsesRequestedKeepStorage(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	if _, err := cleanupBuildCache(context.Background(), "8GB"); err != nil {
		t.Fatalf("cleanupBuildCache returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker builder prune -f --keep-storage 8GB") {
		t.Fatalf("docker log missing requested keep-storage in %#v", entries)
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

	removed, err := cleanupUnusedProjectVolumes(context.Background(), "demo", "production", nil)
	if err == nil || !strings.Contains(err.Error(), "automatic named-volume deletion is disabled") || removed != 0 {
		t.Fatalf("cleanup removed=%d, err=%v", removed, err)
	}

	entries := readCommandLog(t, logPath)
	for _, entry := range entries {
		if strings.Contains(entry, "volume rm ") {
			t.Fatalf("disabled cleanup removed volume via %q; all entries %#v", entry, entries)
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

	removed, err := cleanupUnusedProjectVolumes(context.Background(), "demo", "production", nil)
	if err == nil || removed != 0 {
		t.Fatalf("cleanup removed=%d, err=%v", removed, err)
	}
}

func TestCleanupUnusedProjectVolumesSkipsProtectedExternalVolumes(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	protected := runtimeid.VolumeName("demo", "production", "imported")
	owned := runtimeid.VolumeName("demo", "production", "cache")
	t.Setenv("TAKO_FAKE_VOLUME_LS_OUTPUT", strings.Join([]string{protected, owned}, "\n")+"\n")

	removed, err := cleanupUnusedProjectVolumes(context.Background(), "demo", "production", []string{protected})
	if err == nil || removed != 0 {
		t.Fatalf("cleanup removed=%d, err=%v", removed, err)
	}

	entries := readCommandLog(t, logPath)
	for _, entry := range entries {
		if strings.Contains(entry, "volume rm ") {
			t.Fatalf("disabled cleanup removed volume via %q; all entries %#v", entry, entries)
		}
	}
}

func TestCleanupUnusedProjectVolumesRequiresTakodLocalCreationEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		evidence string
	}{
		{name: "unlabelled", evidence: "local|<no value>|<no value>|<no value>|<no value>|null"},
		{name: "custom driver", evidence: "nfs|demo|production|takod|<no value>|null"},
		{name: "driver options", evidence: `local|demo|production|takod|<no value>|{"type":"none"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "commands.log")
			restore := useFakeCommands(t, logPath)
			defer restore()
			volume := runtimeid.VolumeName("demo", "production", "data")
			t.Setenv("TAKO_FAKE_VOLUME_LS_OUTPUT", volume+"\n")
			t.Setenv("TAKO_FAKE_VOLUME_CLEANUP_EVIDENCE", test.evidence)
			removed, err := cleanupUnusedProjectVolumes(context.Background(), "demo", "production", nil)
			if err == nil || removed != 0 {
				t.Fatalf("cleanup removed=%d, err=%v", removed, err)
			}
			if commands := strings.Join(readCommandLog(t, logPath), "\n"); strings.Contains(commands, "volume rm "+volume) {
				t.Fatalf("unproven volume was removed:\n%s", commands)
			}
		})
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
	if len(response.Warnings) != 1 || !strings.Contains(response.Warnings[0], "automatic named-volume deletion is disabled") {
		t.Fatalf("CleanupProject warnings = %#v", response.Warnings)
	}
	if response.UnusedVolumesCleaned {
		t.Fatal("disabled named-volume cleanup was reported as completed")
	}
	if response.BuildCacheCleaned {
		t.Fatal("scoped prune should not report shared Docker build cache cleanup")
	}

	entries := readCommandLog(t, logPath)
	for _, entry := range entries {
		if strings.Contains(entry, "docker system prune") || strings.Contains(entry, "docker volume prune") {
			t.Fatalf("scoped prune should not run global prune command %q; all entries %#v", entry, entries)
		}
		if strings.Contains(entry, "docker builder prune") || strings.Contains(entry, "docker images -f dangling=true") {
			t.Fatalf("scoped prune should not clean shared Docker cache via %q; all entries %#v", entry, entries)
		}
		if strings.Contains(entry, "docker images --format") {
			t.Fatalf("environment-scoped prune should not remove project images across stages via %q; all entries %#v", entry, entries)
		}
	}
}

func TestCleanupNetworksDisconnectsSharedProxyBeforeRemovingStageNetwork(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	target := runtimeid.NetworkName("demo", "production")
	exportNetwork := runtimeid.ExportNetworkName("demo", "production", "api")
	otherStage := runtimeid.NetworkName("demo", "preview")
	otherProject := runtimeid.NetworkName("other", "production")
	t.Setenv("TAKO_FAKE_NETWORK_LS_OUTPUT", strings.Join([]string{target, exportNetwork, otherStage, otherProject}, "\n")+"\n")

	removed, err := cleanupNetworks(context.Background(), "demo", "production")
	if err != nil {
		t.Fatalf("cleanupNetworks returned error: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}

	entries := readCommandLog(t, logPath)
	wantDisconnect := "docker network disconnect -f " + target + " tako-proxy"
	wantRemove := "docker network rm " + target
	if !slices.Contains(entries, wantDisconnect) {
		t.Fatalf("docker log missing proxy disconnect %q in %#v", wantDisconnect, entries)
	}
	if !slices.Contains(entries, wantRemove) {
		t.Fatalf("docker log missing network remove %q in %#v", wantRemove, entries)
	}
	wantExportRemove := "docker network rm " + exportNetwork
	if !slices.Contains(entries, wantExportRemove) {
		t.Fatalf("docker log missing export network remove %q in %#v", wantExportRemove, entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, otherStage) || strings.Contains(entry, otherProject) {
			t.Fatalf("cleanup touched unrelated network via %q; all entries %#v", entry, entries)
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
