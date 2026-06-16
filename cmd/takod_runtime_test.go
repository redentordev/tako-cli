package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestRequireTakodRuntimeAllowsDefaultTakod(t *testing.T) {
	if err := requireTakodRuntime(&config.Config{}); err != nil {
		t.Fatalf("requireTakodRuntime returned error for default runtime: %v", err)
	}
}

func TestRequireTakodRuntimeRejectsNonTakod(t *testing.T) {
	cfg := &config.Config{
		Runtime: &config.RuntimeConfig{Mode: "legacy"},
	}

	err := requireTakodRuntime(cfg)
	if err == nil {
		t.Fatal("requireTakodRuntime should reject non-takod runtime")
	}
	if !strings.Contains(err.Error(), "runtime.mode=legacy") {
		t.Fatalf("error = %q, want runtime mode included", err.Error())
	}
}

func TestExternalVolumeNamesForEnvironmentReturnsConfiguredDockerNames(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Volumes: map[string]config.VolumeConfig{
			"cache": {
				Name: "custom-cache",
			},
			"data": {
				External: true,
			},
			"n8n": {
				External: true,
				Name:     "captain--n8n-data",
			},
		},
	}

	got := externalVolumeNamesForEnvironment(cfg, "production")
	want := []string{"captain--n8n-data", "data"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("external volumes = %#v, want %#v", got, want)
	}
}
