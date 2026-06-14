package takod

import (
	"context"
	"path/filepath"
	"testing"
)

func TestInspectVolumesReturnsRequestedVolumePresence(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_VOLUME_LS_OUTPUT", "tako_demo_production_data\nother-volume\n")

	response, err := InspectVolumes(context.Background(), VolumeInspectRequest{
		Project:     "demo",
		Environment: "production",
		Volumes:     []string{"tako_demo_production_data", "missing"},
	})
	if err != nil {
		t.Fatalf("InspectVolumes returned error: %v", err)
	}

	if !response.Volumes["tako_demo_production_data"] {
		t.Fatal("expected existing volume to be true")
	}
	if response.Volumes["missing"] {
		t.Fatal("expected missing volume to be false")
	}
	if response.Project != "demo" || response.Environment != "production" {
		t.Fatalf("unexpected response metadata: %#v", response)
	}
}

func TestInspectVolumesRejectsUnsafeRequest(t *testing.T) {
	tests := map[string]VolumeInspectRequest{
		"badProject":     {Project: "../demo", Environment: "production", Volumes: []string{"data"}},
		"badEnvironment": {Project: "demo", Environment: "prod\n", Volumes: []string{"data"}},
		"badVolume":      {Project: "demo", Environment: "production", Volumes: []string{"bad/name"}},
		"duplicate":      {Project: "demo", Environment: "production", Volumes: []string{"data", "data"}},
	}

	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := InspectVolumes(context.Background(), req); err == nil {
				t.Fatal("expected request to be rejected")
			}
		})
	}
}
