package deployplan

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

func testConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1.2.3"},
	}
}

func TestImageRefPrefersExplicitImage(t *testing.T) {
	got := ImageRef(testConfig(), "production", "web", config.ServiceConfig{
		Image: "registry.example.com/web:stable",
		Build: ".",
	}, "abcdef1234567890")
	if got != "registry.example.com/web:stable" {
		t.Fatalf("ImageRef() = %q, want explicit image", got)
	}
}

func TestImageRefUsesBuildTagForBuildService(t *testing.T) {
	got := ImageRef(testConfig(), "production", "web", config.ServiceConfig{Build: "."}, "abcdef1234567890")
	if got != "demo/web:abcdef1234567890" {
		t.Fatalf("ImageRef() = %q, want build-tagged image", got)
	}
}

func TestImageRefBuildWithoutTagFallsBackToEnvironmentTag(t *testing.T) {
	got := ImageRef(testConfig(), "production", "web", config.ServiceConfig{Build: "."}, "")
	if got != "demo/web:1.2.3-production" {
		t.Fatalf("ImageRef() = %q, want environment-tagged image", got)
	}
}

func TestDefaultImageRefs(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web": {Build: "."},
		"db":  {Image: "postgres:16-alpine"},
	}

	got := DefaultImageRefs(testConfig(), "production", services)
	if got["web"] != "demo/web:1.2.3-production" {
		t.Fatalf("web image ref = %q, want default environment image", got["web"])
	}
	if got["db"] != "postgres:16-alpine" {
		t.Fatalf("db image ref = %q, want explicit image", got["db"])
	}
}

func TestDefaultDeployImageRefsUsesBuildTagForBuildServices(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web": {Build: "."},
		"db":  {Image: "postgres:16-alpine"},
	}

	got := DefaultDeployImageRefs(testConfig(), "production", services, "abcdef1234567890")
	if got["web"] != "demo/web:abcdef1234567890" {
		t.Fatalf("web image ref = %q, want build-tagged image", got["web"])
	}
	if got["db"] != "postgres:16-alpine" {
		t.Fatalf("db image ref = %q, want prebuilt image unchanged", got["db"])
	}
}

func TestMergeRuntimeImageRefsPreservesDeployedAndActualImages(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web":    {Build: "."},
		"api":    {Build: "./api"},
		"cache":  {Image: "redis:7"},
		"worker": {Build: "./worker"},
	}
	deployedImageRefs := map[string]string{
		"web": "demo/web:built",
	}
	actualState := map[string]*reconcile.ActualService{
		"api":   {Name: "api", Image: "demo/api:old", Replicas: 1},
		"cache": {Name: "cache", Image: "redis:6", Replicas: 1},
	}

	got := MergeRuntimeImageRefs(testConfig(), "production", services, deployedImageRefs, actualState)
	want := map[string]string{
		"web":    "demo/web:built",
		"api":    "demo/api:old",
		"cache":  "redis:6",
		"worker": "demo/worker:1.2.3-production",
	}
	for serviceName, wantImage := range want {
		if got[serviceName] != wantImage {
			t.Fatalf("image ref for %s = %q, want %q", serviceName, got[serviceName], wantImage)
		}
	}
}
