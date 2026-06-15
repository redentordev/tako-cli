package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestResolveServiceVolumeUsesTopLevelCustomVolumeName(t *testing.T) {
	cfg := volumeBackupTestConfig([]string{"data:/data"})
	cfg.Volumes = map[string]config.VolumeConfig{
		"data": {Name: "shared-data"},
	}

	target, err := resolveServiceVolume(cfg, "production", "web", "data")
	if err != nil {
		t.Fatalf("resolveServiceVolume returned error: %v", err)
	}
	if target.DockerName != "shared-data" {
		t.Fatalf("docker volume = %q, want custom top-level name", target.DockerName)
	}
	if target.BackupKey != "data" {
		t.Fatalf("backup key = %q, want data", target.BackupKey)
	}
}

func TestResolveServiceVolumeSupportsPathVolumeSelector(t *testing.T) {
	cfg := volumeBackupTestConfig([]string{"/var/lib/app"})

	target, err := resolveServiceVolume(cfg, "production", "web", "/var/lib/app")
	if err != nil {
		t.Fatalf("resolveServiceVolume returned error: %v", err)
	}
	wantDockerName := runtimeid.VolumeName("demo", "production", "/var/lib/app")
	if target.DockerName != wantDockerName {
		t.Fatalf("docker volume = %q, want %q", target.DockerName, wantDockerName)
	}
	if target.BackupKey != "var_lib_app" {
		t.Fatalf("backup key = %q, want var_lib_app", target.BackupKey)
	}
}

func TestResolveServiceVolumeRejectsBindMount(t *testing.T) {
	cfg := volumeBackupTestConfig([]string{"/host/data:/data"})

	_, err := resolveServiceVolume(cfg, "production", "web", "/data")
	if err == nil {
		t.Fatal("expected bind mount to be rejected")
	}
	if !strings.Contains(err.Error(), "bind mount") {
		t.Fatalf("error = %v, want bind mount context", err)
	}
}

func TestResolveServiceVolumeRejectsAmbiguousSelector(t *testing.T) {
	cfg := volumeBackupTestConfig([]string{"data:/data", "other:/data"})

	_, err := resolveServiceVolume(cfg, "production", "web", "/data")
	if err == nil {
		t.Fatal("expected ambiguous selector to be rejected")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v, want ambiguous context", err)
	}
}

func TestListServiceBackupVolumesSkipsBindMounts(t *testing.T) {
	cfg := volumeBackupTestConfig([]string{"/host/data:/data", "cache:/cache"})

	volumes, err := listServiceBackupVolumes(cfg, "production", "web")
	if err != nil {
		t.Fatalf("listServiceBackupVolumes returned error: %v", err)
	}
	if len(volumes) != 1 || volumes[0].BackupKey != "cache" {
		t.Fatalf("volumes = %#v, want only cache", volumes)
	}
}

func TestSanitizeBackupKeyProducesSafeFallback(t *testing.T) {
	got := sanitizeBackupKey("/var/lib/App Data")
	if got != "var_lib_app_data" {
		t.Fatalf("sanitizeBackupKey = %q, want var_lib_app_data", got)
	}
}

func volumeBackupTestConfig(volumes []string) *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Services: map[string]config.ServiceConfig{
					"web": {Volumes: volumes},
				},
			},
		},
	}
}
