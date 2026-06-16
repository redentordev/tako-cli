package deployer

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestResolveServiceEnvLocalLink(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1.0.0"},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Services: map[string]config.ServiceConfig{
					"admin": {
						Image: "nginx:alpine",
						Port:  3000,
					},
					"renderer": {
						Image: "nginx:alpine",
						Port:  3000,
						Env: map[string]config.EnvValue{
							"CMS_ADMIN_API_BASE_URL": {Link: &config.ServiceLinkRef{Service: "admin"}},
						},
					},
				},
			},
		},
	}
	deploy := &Deployer{config: cfg, environment: "production"}
	renderer := cfg.Environments["production"].Services["renderer"]

	env, err := deploy.resolveServiceEnv("renderer", &renderer)
	if err != nil {
		t.Fatalf("resolveServiceEnv returned error: %v", err)
	}
	if got := env["CMS_ADMIN_API_BASE_URL"].PlainString(); got != "http://admin:3000" {
		t.Fatalf("CMS_ADMIN_API_BASE_URL = %q", got)
	}
}
