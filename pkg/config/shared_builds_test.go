package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestValidateConfigAcceptsSharedBuildConsumers(t *testing.T) {
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := minimalValidConfigWithService(ServiceConfig{ImageFrom: "application"})
	cfg.Builds = map[string]SharedBuildConfig{
		"application": {Context: contextDir, Args: map[string]string{"BASE": "alpine"}, Target: "runtime"},
	}
	env := cfg.Environments["production"]
	env.Services["worker"] = ServiceConfig{Kind: ServiceKindJob, ImageFrom: "application", Schedule: "@hourly", Command: ListValue("work")}
	env.Services["migrate"] = ServiceConfig{Kind: ServiceKindRun, ImageFrom: "application", Command: ListValue("migrate")}
	cfg.Environments["production"] = env
	if err := ValidateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"web", "worker", "migrate"} {
		service := cfg.Environments["production"].Services[name]
		if service.SharedBuildHash == "" || service.Build != "" || service.Image != "" {
			t.Fatalf("shared build consumer %s = %#v", name, service)
		}
	}
	if slices.Contains(cfg.Environments["production"].Services["migrate"].DependsOn, "application") {
		t.Fatal("shared build name was added as a service dependency")
	}
}

func TestValidateConfigRejectsInvalidSharedBuildReferences(t *testing.T) {
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		service ServiceConfig
		builds  map[string]SharedBuildConfig
		want    string
	}{
		{name: "unknown standard", service: ServiceConfig{ImageFrom: "missing"}, want: "top-level build"},
		{name: "mixed sources", service: ServiceConfig{Image: "nginx", ImageFrom: "shared"}, builds: map[string]SharedBuildConfig{"shared": {Context: contextDir}}, want: "cannot be combined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := minimalValidConfigWithService(tt.service)
			cfg.Builds = tt.builds
			if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestSharedBuildFingerprintUsesDeclaredRelativeContext(t *testing.T) {
	first := &Config{Builds: map[string]SharedBuildConfig{"application": {Context: "./app", Args: map[string]string{"BASE": "alpine"}}}}
	second := &Config{Builds: map[string]SharedBuildConfig{"application": {Context: "./app", Args: map[string]string{"BASE": "alpine"}}}}
	normalizeConfigRelativePaths(first, filepath.Join(t.TempDir(), "checkout-a"))
	normalizeConfigRelativePaths(second, filepath.Join(t.TempDir(), "checkout-b"))
	if first.Builds["application"].Context == second.Builds["application"].Context {
		t.Fatal("test contexts did not resolve to different hosts paths")
	}
	if first.Builds["application"].Fingerprint() != second.Builds["application"].Fingerprint() {
		t.Fatalf("fingerprints differ: %q %q", first.Builds["application"].Fingerprint(), second.Builds["application"].Fingerprint())
	}
}
