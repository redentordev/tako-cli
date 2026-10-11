package engine

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

// Regression: `CMS_RUNTIME_MODE: site` used to register "site" as a secret,
// so deploy output showed `[REDACTED]-renderer` and image tags (commit SHAs)
// were masked too.
func TestServiceEnvRegistrationKeepsNamesAndImageTagsReadable(t *testing.T) {
	cfg := &config.Config{
		Project:      config.ProjectConfig{Name: "jardin-cms"},
		Environments: map[string]config.EnvironmentConfig{"production": {}},
		Servers:      map[string]config.ServerConfig{"app": {}},
	}
	image := "132769310461.dkr.ecr.ap-southeast-1.amazonaws.com/cms-production/site-renderer:1b2615e0c3d4a5b6c7d8e9f00112233445566778"
	services := map[string]config.ServiceConfig{
		"site-renderer": {
			Image: image,
			Env: map[string]string{
				"CMS_RUNTIME_MODE":      "site",
				"PORT":                  "3000",
				"CMS_RUNTIME_API_TOKEN": "runtime-token-value",
			},
		},
	}

	eng := New(Options{})
	eng.RegisterPublicConfigNames(cfg, services)
	eng.RegisterServiceEnvValues(services)

	got := eng.RedactSensitive("→ Deploying service: site-renderer " + image + " token=runtime-token-value")
	for _, want := range []string{"site-renderer", image} {
		if !strings.Contains(got, want) {
			t.Fatalf("output lost %q: %q", want, got)
		}
	}
	if strings.Contains(got, "runtime-token-value") {
		t.Fatalf("credential env value leaked: %q", got)
	}
}
