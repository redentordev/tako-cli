package deployplan

import (
	"strings"
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

func TestImageRefAcceptsSourceBuildTagForBuildService(t *testing.T) {
	got := ImageRef(testConfig(), "production", "web", config.ServiceConfig{Build: "."}, "source-20260705T043456Z")
	if got != "demo/web:source-20260705T043456Z" {
		t.Fatalf("ImageRef() = %q, want source-tagged image", got)
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

func TestSharedBuildConsumersResolveOneBuildImageRef(t *testing.T) {
	cfg := testConfig()
	cfg.Builds = map[string]config.SharedBuildConfig{"application": {Context: "."}}
	services := map[string]config.ServiceConfig{
		"web":     {ImageFrom: "application", SharedBuildHash: "hash"},
		"worker":  {ImageFrom: "application", SharedBuildHash: "hash"},
		"migrate": {Kind: config.ServiceKindRun, ImageFrom: "application", SharedBuildHash: "hash", Command: config.ListValue("migrate")},
	}
	got := DefaultDeployImageRefs(cfg, "production", services, "abcdef")
	want := SharedBuildImageRef(cfg, "production", "application", "abcdef")
	for _, name := range []string{"web", "worker", "migrate"} {
		if got[name] != want {
			t.Fatalf("%s image = %q", name, got[name])
		}
	}
	if got[SharedBuildImageRefKey("application")] != want {
		t.Fatalf("build ref = %q", got[SharedBuildImageRefKey("application")])
	}
}

func TestSharedBuildImageRefSeparatesNamespaceAndDefinitionFingerprint(t *testing.T) {
	cfg := testConfig()
	cfg.Builds = map[string]config.SharedBuildConfig{"web": {Context: ".", Args: map[string]string{"BASE": "first"}}}
	first := SharedBuildImageRef(cfg, "production", "web", "revision")
	cfg.Builds["web"] = config.SharedBuildConfig{Context: ".", Args: map[string]string{"BASE": "second"}}
	second := SharedBuildImageRef(cfg, "production", "web", "revision")
	if first == second || !strings.HasPrefix(first, "demo/shared/web:revision-sb-") || !strings.HasPrefix(second, "demo/shared/web:revision-sb-") {
		t.Fatalf("refs = %q %q", first, second)
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
