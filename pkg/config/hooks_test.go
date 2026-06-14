package config

import "testing"

func TestValidateConfigAcceptsServiceHooks(t *testing.T) {
	cfg := minimalHookConfig()
	web := cfg.Environments["production"].Services["web"]
	web.Hooks.PreDeploy = &HookConfig{
		Command:    "npm run migrate",
		Timeout:    "5m",
		User:       "1000:1000",
		WorkingDir: "/app",
		Env:        map[string]string{"HOOK_MODE": "pre"},
		Secrets:    []string{"DATABASE_URL"},
	}
	cfg.Environments["production"].Services["web"] = web

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func TestValidateConfigRejectsInvalidHooks(t *testing.T) {
	tests := map[string]HookConfig{
		"empty command":    {Timeout: "5m"},
		"invalid timeout":  {Command: "echo ok", Timeout: "forever"},
		"control user":     {Command: "echo ok", User: "bad\nuser"},
		"control work dir": {Command: "echo ok", WorkingDir: "/app\nbad"},
	}
	for name, hook := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := minimalHookConfig()
			web := cfg.Environments["production"].Services["web"]
			web.Hooks.PreDeploy = &hook
			cfg.Environments["production"].Services["web"] = web

			if err := ValidateConfig(cfg); err == nil {
				t.Fatal("ValidateConfig should reject invalid hook")
			}
		})
	}
}

func minimalHookConfig() *Config {
	return &Config{
		Project: ProjectConfig{Name: "demo", Version: "1.0.0"},
		Servers: map[string]ServerConfig{
			"prod": {Host: "example.test", User: "deploy", Password: "${SSH_PASSWORD}"},
		},
		Environments: map[string]EnvironmentConfig{
			"production": {
				Servers: []string{"prod"},
				Services: map[string]ServiceConfig{
					"web": {Image: "nginx:1.27"},
				},
			},
		},
	}
}
