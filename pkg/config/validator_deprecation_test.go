package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDeprecatedConfigWarnings(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *Config
		wantFields []string
		wantEnvs   []string
		wantText   map[string][]string
	}{
		{
			name:       "no deployment config",
			cfg:        &Config{},
			wantFields: nil,
		},
		{
			name: "build strategy only is not deprecated",
			cfg: &Config{Deployment: &DeploymentConfig{
				Build: &BuildConfig{Strategy: BuildStrategyAuto},
			}},
			wantFields: nil,
		},
		{
			name: "strategy parallel and cache",
			cfg: &Config{Deployment: &DeploymentConfig{
				Strategy: "parallel",
				Parallel: &ParallelConfig{MaxConcurrentBuilds: 4},
				Cache:    &CacheConfig{Enabled: true},
				Build:    &BuildConfig{Strategy: BuildStrategyRemote},
			}},
			wantFields: []string{"deployment.strategy", "deployment.parallel", "deployment.cache"},
			wantEnvs:   []string{"", "", ""},
			wantText: map[string][]string{
				"deployment.strategy": {"has no effect", "remove this key", "dependsOn", "services.<name>.deploy.strategy"},
				"deployment.parallel": {"has no effect", "remove this key", "dependsOn", "services.<name>.deploy.strategy"},
				"deployment.cache":    {"has no effect", "remove this key", "tako cleanup --docker-cache", "--build-cache-keep-storage"},
			},
		},
		{
			name: "empty parallel block still counts as present",
			cfg: &Config{Deployment: &DeploymentConfig{
				Parallel: &ParallelConfig{},
			}},
			wantFields: []string{"deployment.parallel"},
			wantEnvs:   []string{""},
		},
		{
			name: "environment labels sorted by environment",
			cfg: &Config{Environments: map[string]EnvironmentConfig{
				"staging":    {Labels: map[string]string{"tier": "staging"}},
				"production": {Labels: map[string]string{"tier": "prod"}},
				"preview":    {Labels: map[string]string{}},
			}},
			wantFields: []string{"environments.production.labels", "environments.staging.labels"},
			wantEnvs:   []string{"production", "staging"},
			wantText: map[string][]string{
				"environments.production.labels": {"has no effect", "servers.<name>.labels", "serverSelector", "placement constraints"},
			},
		},
		{
			name: "deployment keys before environment labels",
			cfg: &Config{
				Deployment:   &DeploymentConfig{Cache: &CacheConfig{}},
				Environments: map[string]EnvironmentConfig{"production": {Labels: map[string]string{"a": "b"}}},
			},
			wantFields: []string{"deployment.cache", "environments.production.labels"},
			wantEnvs:   []string{"", "production"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings := ValidationWarnings(tt.cfg)
			var fields, envs []string
			for _, warning := range warnings {
				fields = append(fields, warning.Field)
				envs = append(envs, warning.Environment)
				if warning.Service != "" {
					t.Fatalf("deprecation warning has service %q", warning.Service)
				}
				if !strings.HasPrefix(warning.Message, warning.Field+" is deprecated and has no effect; ") {
					t.Fatalf("message %q does not start with the key path", warning.Message)
				}
				if !strings.HasSuffix(warning.Message, "It will be rejected starting in "+DeprecatedKeysRemovalRelease+".") {
					t.Fatalf("message %q does not name the removal release", warning.Message)
				}
				for _, want := range tt.wantText[warning.Field] {
					if !strings.Contains(warning.Message, want) {
						t.Fatalf("message %q missing %q", warning.Message, want)
					}
				}
			}
			if !reflect.DeepEqual(fields, tt.wantFields) {
				t.Fatalf("fields = %#v, want %#v", fields, tt.wantFields)
			}
			if len(tt.wantEnvs) > 0 && !reflect.DeepEqual(envs, tt.wantEnvs) {
				t.Fatalf("environments = %#v, want %#v", envs, tt.wantEnvs)
			}
		})
	}
}

func TestDeprecatedKeysRemovalRelease(t *testing.T) {
	if DeprecatedKeysRemovalRelease != "v0.15.0" {
		t.Fatalf("DeprecatedKeysRemovalRelease = %q, want v0.15.0", DeprecatedKeysRemovalRelease)
	}
}

func TestDeprecatedKeysStillLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tako.yaml")
	content := `project:
  name: legacy
  version: 1.0.0
deployment:
  strategy: parallel
  parallel:
    maxConcurrentBuilds: 4
    maxConcurrentDeploys: 2
    strategy: dependency-aware
  cache:
    enabled: true
    type: local
    retention: 7d
servers:
  production:
    host: 203.0.113.10
    user: root
    password: example
environments:
  production:
    servers: [production]
    labels:
      tier: prod
    services:
      web:
        image: nginx:alpine
        port: 80
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	var fields []string
	for _, warning := range ValidationWarnings(cfg) {
		fields = append(fields, warning.Field)
	}
	want := []string{"deployment.strategy", "deployment.parallel", "deployment.cache", "environments.production.labels"}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields = %#v, want %#v", fields, want)
	}
}
