package engine

import (
	"slices"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployplan"
)

func TestBuildSharedImagesExecutesEachBuildOnceWithAllConsumers(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1"},
		Builds: map[string]config.SharedBuildConfig{
			"application": {Context: "./app"},
			"tools":       {Context: "./tools"},
		},
	}
	services := map[string]config.ServiceConfig{
		"web":     {ImageFrom: "application", SharedBuildHash: "app-hash"},
		"worker":  {ImageFrom: "application", SharedBuildHash: "app-hash"},
		"migrate": {Kind: config.ServiceKindRun, ImageFrom: "tools", SharedBuildHash: "tools-hash"},
		"db":      {Image: "postgres:16"},
	}
	calls := map[string]int{}
	consumerCounts := map[string]int{}
	refs := map[string]string{}
	err := buildSharedImagesWith(cfg, "production", "revision", services, func(name string, build config.SharedBuildConfig, ref string, consumers map[string]config.ServiceConfig) error {
		calls[name]++
		consumerCounts[name] = len(consumers)
		refs[name] = ref
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls["application"] != 1 || calls["tools"] != 1 || consumerCounts["application"] != 2 || consumerCounts["tools"] != 1 {
		t.Fatalf("calls=%v consumers=%v", calls, consumerCounts)
	}
	if refs["application"] != deployplan.SharedBuildImageRef(cfg, "production", "application", "revision") || refs["tools"] != deployplan.SharedBuildImageRef(cfg, "production", "tools", "revision") {
		t.Fatalf("refs = %v", refs)
	}
}

func TestCleanupImageRepositoriesUsesSharedBuildRepositoryOnce(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo", Version: "1"}, Builds: map[string]config.SharedBuildConfig{"application": {Context: "."}}}
	services := map[string]config.ServiceConfig{
		"web":    {ImageFrom: "application", SharedBuildHash: "hash"},
		"worker": {ImageFrom: "application", SharedBuildHash: "hash"},
		"api":    {Build: "./api"},
	}
	got := CleanupImageRepositories(cfg, "production", services)
	if !slices.Equal(got, []string{"demo/api", "demo/shared/application"}) {
		t.Fatalf("repositories = %#v", got)
	}
}

func TestPlanSharedBuildArtifactIdentityChangesPlanHash(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo", Version: "1"}, Builds: map[string]config.SharedBuildConfig{"application": {Context: ".", Args: map[string]string{"BASE": "first"}}}}
	services := map[string]config.ServiceConfig{"web": {ImageFrom: "application", SharedBuildHash: cfg.Builds["application"].Fingerprint()}}
	first := DeployPlan{APIVersion: "v1", Kind: KindDeployPlan, Project: "demo", Environment: "production", Revision: "revision", SharedBuilds: planSharedBuilds(cfg, "production", "revision", services)}
	cfg.Builds["application"] = config.SharedBuildConfig{Context: ".", Args: map[string]string{"BASE": "second"}}
	services["web"] = config.ServiceConfig{ImageFrom: "application", SharedBuildHash: cfg.Builds["application"].Fingerprint()}
	second := first
	second.SharedBuilds = planSharedBuilds(cfg, "production", "revision", services)
	if first.Hash() == second.Hash() || first.SharedBuilds[0].Image == second.SharedBuilds[0].Image {
		t.Fatalf("plans did not capture shared artifact drift: %#v %#v", first.SharedBuilds, second.SharedBuilds)
	}
}

func TestBuildSharedImagesHonorsSkipBuild(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo"}, Builds: map[string]config.SharedBuildConfig{"application": {Context: "."}}}
	calls := 0
	err := ensureSharedImagesWith(cfg, "production", "revision", map[string]config.ServiceConfig{"web": {ImageFrom: "application", SharedBuildHash: "hash"}}, func(name string, ref string, consumers map[string]config.ServiceConfig) error {
		calls++
		if name != "application" || ref == "" || len(consumers) != 1 {
			t.Fatalf("ensure call = %q %q %#v", name, ref, consumers)
		}
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
