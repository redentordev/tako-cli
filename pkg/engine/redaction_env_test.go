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

// Regression: v0.16.0 printed the deploy summary as
// `deployment 1791773952 · [REDACTED:pattern.base64]` (the full commit SHA
// matched the base64 pattern) and the admin domain line as
// `[REDACTED:env.BETTER_AUTH_URL]` (a public URL under a credential-like
// name).
func TestDeployOutputKeepsRevisionAndPublicURLsReadable(t *testing.T) {
	const sha = "2982b20ab726da547722e1ac44bb6a7f4c7d2dc6"
	services := map[string]config.ServiceConfig{
		"admin": {
			Image: "registry.example.com/admin:" + sha,
			Env: map[string]string{
				"BETTER_AUTH_URL":             "https://admin.example.com",
				"BETTER_AUTH_TRUSTED_ORIGINS": "https://admin.example.com,https://site.example.com",
				"BETTER_AUTH_SECRET":          "https://admin.example.com/not-really-a-url-secret",
				"JARDIN_N8N_HOST":             "n8n1.vixilabs-example.com",
			},
		},
	}

	eng := New(Options{})
	eng.RegisterPublicConfigNames(&config.Config{Project: config.ProjectConfig{Name: "jardin-cms"}}, services)
	eng.RegisterServiceEnvValues(services)
	eng.RegisterPublicRevisions(sha, sha[:7])

	for _, line := range []string{
		"✓ Deployed jardin-cms → production in 33s · deployment 1791773952 · " + sha,
		"   admin          https://admin.example.com       ready (DNS is routed and TLS is active)",
		"   n8n            https://n8n1.vixilabs-example.com  ready",
	} {
		if got := eng.RedactSensitive(line); got != line {
			t.Errorf("RedactSensitive(%q) = %q", line, got)
		}
	}
	if got := eng.RedactSensitive("secret https://admin.example.com/not-really-a-url-secret"); strings.Contains(got, "not-really") {
		t.Fatalf("credential-named value leaked: %q", got)
	}
	// An unregistered 40-hex value outside an image tag is still masked.
	if got := eng.RedactSensitive("token 0123456789abcdef0123456789abcdef01234567"); strings.Contains(got, "0123456789abcdef") {
		t.Fatalf("unregistered hex value leaked: %q", got)
	}
}
