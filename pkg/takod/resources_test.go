package takod

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestListImagesFiltersProjectImages(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_IMAGES_OUTPUT", "img1\tdemo/web\t1.0.0\t12MB\t2 hours ago\nimg2\tother/web\t1.0.0\t9MB\t1 hour ago\n")

	response, err := ListImages(context.Background(), ImageListRequest{Project: "demo", Environment: "production"})
	if err != nil {
		t.Fatalf("ListImages returned error: %v", err)
	}
	if len(response.Images) != 1 {
		t.Fatalf("images = %#v, want one demo image", response.Images)
	}
	if response.Images[0].Reference != "demo/web:1.0.0" || response.Images[0].ID != "img1" {
		t.Fatalf("unexpected image summary: %#v", response.Images[0])
	}
}

func TestRemoveImagesRejectsOtherProject(t *testing.T) {
	_, err := RemoveImages(context.Background(), ImageRemoveRequest{
		Project:    "demo",
		References: []string{"other/web:1.0.0"},
		Force:      true,
	})
	if err == nil {
		t.Fatal("RemoveImages should reject images outside the project namespace")
	}
}

func TestPruneImagesRemovesOnlyUnusedProjectImages(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_IMAGES_OUTPUT", "imglive\tdemo/web\tlive\t12MB\t1 hour ago\nimgold\tdemo/web\told\t12MB\t2 hours ago\nimgother\tother/web\told\t9MB\t2 hours ago\n")
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo-web-1\n")
	t.Setenv("TAKO_FAKE_INSPECT_OUTPUT", "demo/web:live\tsha256:imglive\n")

	response, err := PruneImages(context.Background(), ImagePruneRequest{
		Project:     "demo",
		Environment: "production",
		Force:       true,
	})
	if err != nil {
		t.Fatalf("PruneImages returned error: %v", err)
	}
	if !slices.Equal(response.Removed, []string{"demo/web:old"}) {
		t.Fatalf("removed = %#v, want old image", response.Removed)
	}
	if !slices.Equal(response.Skipped, []string{"demo/web:live"}) {
		t.Fatalf("skipped = %#v, want live image", response.Skipped)
	}

	entries := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(entries, "docker rmi demo/web:old") {
		t.Fatalf("expected old image removal, got log:\n%s", entries)
	}
	if strings.Contains(entries, "docker rmi demo/web:live") || strings.Contains(entries, "docker rmi other/web:old") {
		t.Fatalf("unexpected image removal in log:\n%s", entries)
	}
}

func TestPruneImagesRequiresForce(t *testing.T) {
	_, err := PruneImages(context.Background(), ImagePruneRequest{
		Project:     "demo",
		Environment: "production",
	})
	if err == nil {
		t.Fatal("PruneImages should require force confirmation")
	}
}

func TestListVolumesReturnsProjectVolumes(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_VOLUME_LS_OUTPUT", "tako_demo_production_data\tlocal\tlocal\nshared-cache\tlocal\tlocal\n")

	response, err := ListVolumes(context.Background(), VolumeListRequest{Project: "demo", Environment: "production"})
	if err != nil {
		t.Fatalf("ListVolumes returned error: %v", err)
	}
	if len(response.Volumes) != 2 {
		t.Fatalf("volumes = %#v, want two volumes", response.Volumes)
	}
	if response.Volumes[0].Name != "shared-cache" || response.Volumes[1].Name != "tako_demo_production_data" {
		t.Fatalf("volumes not sorted as expected: %#v", response.Volumes)
	}
}

func TestRemoveVolumesAllowsLabelOwnedCustomName(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_VOLUME_INSPECT_LABELS", "demo\tproduction\n")

	response, err := RemoveVolumes(context.Background(), VolumeRemoveRequest{
		Project:     "demo",
		Environment: "production",
		Names:       []string{"shared-cache"},
		Force:       true,
	})
	if err != nil {
		t.Fatalf("RemoveVolumes returned error: %v", err)
	}
	if !slices.Equal(response.Removed, []string{"shared-cache"}) {
		t.Fatalf("removed = %#v, want shared-cache", response.Removed)
	}
}
