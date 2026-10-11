package takod

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeImageCleanupJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func imageCleanupServices(images map[string]string) map[string]any {
	services := make(map[string]any, len(images))
	for name, image := range images {
		services[name] = map[string]any{"name": name, "image": image}
	}
	return services
}

func writeImageCleanupDeployment(t *testing.T, dataDir, project, environment, id string, images map[string]string) {
	t.Helper()
	writeImageCleanupJSON(t, filepath.Join(dataDir, "history", project, environment, "deployments", id+".json"), map[string]any{
		"id": id, "projectName": project, "environment": environment, "services": imageCleanupServices(images),
	})
}

func writeImageCleanupDesired(t *testing.T, dataDir, project, environment string, images map[string]string) {
	t.Helper()
	writeImageCleanupJSON(t, filepath.Join(dataDir, "desired", project, environment, "revision.json"), map[string]any{
		"project": project, "environment": environment, "revisionId": "rev", "services": imageCleanupServices(images),
	})
}

func rmiEntries(entries []string) []string {
	var out []string
	for _, entry := range entries {
		if strings.HasPrefix(entry, "docker rmi") {
			out = append(out, entry)
		}
	}
	return out
}

// sharedServerImageList lists images newest first. Tags carry no environment
// (commit-hash build tags), so ownership comes from deployment state.
const sharedServerImageList = "" +
	"stg-5\tdemo/web\tsha-stg5\n" +
	"stg-4\tdemo/web\tsha-stg4\n" +
	"stg-3\tdemo/web\tsha-stg3\n" +
	"prd-3\tdemo/web\tsha-prd3\n" +
	"prd-2\tdemo/web\tsha-prd2\n" +
	"prd-1\tdemo/web\tsha-prd1\n" +
	"stg-2\tdemo/web\tsha-stg2\n" +
	"stg-1\tdemo/web\tsha-stg1\n" +
	"stgapi-3\tdemo/api\tsha-stg3\n" +
	"stgapi-2\tdemo/api\tsha-stg2\n" +
	"stgapi-1\tdemo/api\tsha-stg1\n"

func writeSharedServerState(t *testing.T, dataDir string) {
	t.Helper()
	for i, tag := range []string{"sha-stg1", "sha-stg2", "sha-stg3", "sha-stg4", "sha-stg5"} {
		images := map[string]string{"web": "demo/web:" + tag}
		if i < 3 {
			images["api"] = "demo/api:" + tag
		}
		writeImageCleanupDeployment(t, dataDir, "demo", "staging", "s"+tag, images)
	}
	writeImageCleanupDesired(t, dataDir, "demo", "staging", map[string]string{"web": "demo/web:sha-stg5", "api": "demo/api:sha-stg3"})
	for _, tag := range []string{"sha-prd1", "sha-prd2", "sha-prd3"} {
		writeImageCleanupDeployment(t, dataDir, "demo", "production", "p"+tag, map[string]string{"web": "demo/web:" + tag})
	}
	writeImageCleanupDesired(t, dataDir, "demo", "production", map[string]string{"web": "demo/web:sha-prd3"})
}

func TestCleanupOldProjectImagesStagingLeavesProductionImagesOnSharedServer(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	dataDir := t.TempDir()
	writeSharedServerState(t, dataDir)
	t.Setenv("TAKO_FAKE_IMAGE_LIST_OUTPUT", sharedServerImageList)

	removed, err := cleanupOldProjectImages(context.Background(), dataDir, "demo", "staging", 1, []string{"demo/api", "demo/web"})
	if err != nil {
		t.Fatalf("cleanupOldProjectImages returned error: %v", err)
	}
	got := rmiEntries(readCommandLog(t, logPath))
	want := []string{"docker rmi stg-4", "docker rmi stg-3", "docker rmi stg-2", "docker rmi stg-1", "docker rmi stgapi-2", "docker rmi stgapi-1"}
	if !slices.Equal(got, want) || removed != len(want) {
		t.Fatalf("rmi = %#v (removed %d), want %#v", got, removed, want)
	}
	for _, entry := range got {
		if strings.Contains(entry, "prd") || strings.Contains(entry, "-f") {
			t.Fatalf("staging cleanup touched production or forced removal: %q", entry)
		}
	}
}

func TestCleanupOldProjectImagesKeepsNewestNPerService(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	dataDir := t.TempDir()
	writeSharedServerState(t, dataDir)
	t.Setenv("TAKO_FAKE_IMAGE_LIST_OUTPUT", sharedServerImageList)

	if _, err := cleanupOldProjectImages(context.Background(), dataDir, "demo", "staging", 3, []string{"demo/api", "demo/web"}); err != nil {
		t.Fatalf("cleanupOldProjectImages returned error: %v", err)
	}
	// web keeps stg-5..stg-3; api has only three images, all kept.
	got := rmiEntries(readCommandLog(t, logPath))
	want := []string{"docker rmi stg-2", "docker rmi stg-1"}
	if !slices.Equal(got, want) {
		t.Fatalf("rmi = %#v, want %#v", got, want)
	}

	// Production cleanup sees only its own three images and keeps them all.
	if err := os.Remove(logPath); err != nil {
		t.Fatalf("remove log: %v", err)
	}
	if _, err := cleanupOldProjectImages(context.Background(), dataDir, "demo", "production", 3, []string{"demo/web"}); err != nil {
		t.Fatalf("production cleanup returned error: %v", err)
	}
	if got := rmiEntries(readCommandLog(t, logPath)); len(got) != 0 {
		t.Fatalf("production cleanup removed %#v, want nothing", got)
	}
}

func TestCleanupOldProjectImagesRetainsImagesUsedByContainers(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	dataDir := t.TempDir()
	writeSharedServerState(t, dataDir)
	t.Setenv("TAKO_FAKE_IMAGE_LIST_OUTPUT", sharedServerImageList)
	// A stopped container still uses stg-2's image.
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "container-1\n")
	t.Setenv("TAKO_FAKE_INSPECT_IMAGE", "sha256:stg-2aaaaaaaaaaaaaaaa\tdemo/web:sha-stg2\n")

	if _, err := cleanupOldProjectImages(context.Background(), dataDir, "demo", "staging", 3, []string{"demo/web"}); err != nil {
		t.Fatalf("cleanupOldProjectImages returned error: %v", err)
	}
	got := rmiEntries(readCommandLog(t, logPath))
	want := []string{"docker rmi stg-1"}
	if !slices.Equal(got, want) {
		t.Fatalf("rmi = %#v, want %#v", got, want)
	}
}

func TestCleanupOldProjectImagesRemovesNothingWhenStateUnreadable(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, dataDir string) string{
		"corrupt deployment record": func(t *testing.T, dataDir string) string {
			writeSharedServerState(t, dataDir)
			path := filepath.Join(dataDir, "history", "demo", "production", "deployments", "broken.json")
			if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
				t.Fatal(err)
			}
			return dataDir
		},
		"corrupt desired state of another project": func(t *testing.T, dataDir string) string {
			writeSharedServerState(t, dataDir)
			path := filepath.Join(dataDir, "desired", "other", "production", "revision.json")
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			return dataDir
		},
		"missing data directory": func(t *testing.T, dataDir string) string {
			return filepath.Join(dataDir, "missing")
		},
		"unknown data directory": func(t *testing.T, dataDir string) string {
			return ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "commands.log")
			restore := useFakeCommands(t, logPath)
			defer restore()
			dataDir := setup(t, t.TempDir())
			t.Setenv("TAKO_FAKE_IMAGE_LIST_OUTPUT", sharedServerImageList)

			removed, err := cleanupOldProjectImages(context.Background(), dataDir, "demo", "staging", 0, []string{"demo/api", "demo/web"})
			if err == nil {
				t.Fatalf("expected error for unreadable state")
			}
			if removed != 0 {
				t.Fatalf("removed = %d, want 0", removed)
			}
			if got := rmiEntries(readCommandLog(t, logPath)); len(got) != 0 {
				t.Fatalf("rmi = %#v, want nothing removed", got)
			}
		})
	}
}

func TestCleanupOldProjectImagesRemovesNothingWhenContainerUsageUnreadable(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	dataDir := t.TempDir()
	writeSharedServerState(t, dataDir)
	t.Setenv("TAKO_FAKE_IMAGE_LIST_OUTPUT", sharedServerImageList)
	t.Setenv("TAKO_FAKE_DOCKER_PS_ERROR", "daemon unavailable")

	if _, err := cleanupOldProjectImages(context.Background(), dataDir, "demo", "staging", 0, []string{"demo/web"}); err == nil {
		t.Fatalf("expected container usage error")
	}
	if got := rmiEntries(readCommandLog(t, logPath)); len(got) != 0 {
		t.Fatalf("rmi = %#v, want nothing removed", got)
	}
}

func TestPlanOldImageRemovalsProtectsSharedAndCurrentImages(t *testing.T) {
	images := parseDockerImageList("" +
		"new\tdemo/web\tsha-new\n" +
		"both\tdemo/web\tsha-both\n" +
		"other-current\tdemo/web\tsha-other\n" +
		"legacy-prod\tdemo/web\t1.0-production\n" +
		"legacy-stg\tdemo/web\t1.0-staging\n" +
		"multi\tdemo/web\tsha-multi\n" +
		"multi\tunrelated/cache\tlatest\n" +
		"unknown\tdemo/web\tsha-unknown\n")
	evidence := imageCleanupEvidence{
		owners: map[string]map[string]bool{
			"demo/web:sha-new":   {"staging": true},
			"demo/web:sha-both":  {"staging": true, "production": true},
			"demo/web:sha-other": {"staging": true},
			"demo/web:sha-multi": {"staging": true},
		},
		environments:  map[string]bool{"staging": true, "production": true},
		protectedRefs: map[string]bool{"demo/web:sha-other": true},
	}
	got := planOldImageRemovals(images, evidence, "staging", 1, []string{"demo/web"})
	if len(got) != 1 || got[0].ID != "legacy-stg" {
		t.Fatalf("removals = %#v, want only legacy staging image", got)
	}
}
