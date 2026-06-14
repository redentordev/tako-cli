package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigRejectsUnknownJSONStorageField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tako.json")
	err := os.WriteFile(path, []byte(`{
  "project": {"name": "demo", "version": "1.0.0"},
  "storage": {"nfs": {"enabled": true}},
  "servers": {"node-a": {"host": "10.0.0.1", "user": "deploy"}},
  "environments": {
    "production": {
      "servers": ["node-a"],
      "services": {"web": {"image": "nginx:alpine"}}
    }
  }
}`), 0600)
	if err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err = LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig should reject unknown storage field")
	}
	if !strings.Contains(err.Error(), `unknown field "storage"`) {
		t.Fatalf("error = %q, want unknown storage field error", err)
	}
}

func TestLoadConfigRejectsUnknownNestedJSONField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tako.json")
	err := os.WriteFile(path, []byte(`{
  "project": {"name": "demo", "version": "1.0.0"},
  "servers": {"node-a": {"host": "10.0.0.1", "user": "deploy"}},
  "environments": {
    "production": {
      "servers": ["node-a"],
      "services": {"web": {"image": "nginx:alpine", "unknown": true}}
    }
  }
}`), 0600)
	if err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err = LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig should reject unknown nested field")
	}
	if !strings.Contains(err.Error(), `unknown field "unknown"`) {
		t.Fatalf("error = %q, want unknown nested field error", err)
	}
}

func TestValidateConfigRejectsNFSVolumeSpecs(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image:   "nginx:alpine",
		Volumes: []string{"nfs:shared_data:/data:rw"},
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject NFS volumes")
	}
	if !strings.Contains(err.Error(), "NFS volume") || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("error = %q, want unsupported NFS volume error", err)
	}
}

func TestIsNFSVolumeDetectsRemovedPrefix(t *testing.T) {
	if !IsNFSVolume("nfs:shared_data:/shared:ro") {
		t.Fatal("IsNFSVolume should detect removed nfs prefix")
	}
	if IsNFSVolume("data:/data") {
		t.Fatal("IsNFSVolume should ignore regular volume specs")
	}
}

func TestValidateConfigRejectsDisabledRuntimeAgent(t *testing.T) {
	cfg := validValidationConfig()
	disabled := false
	cfg.Runtime = &RuntimeConfig{
		Agent: &AgentConfig{Enabled: &disabled},
	}

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject disabled runtime agent")
	}
	if !strings.Contains(err.Error(), "runtime.agent.enabled=false") {
		t.Fatalf("error = %q, want runtime agent error", err)
	}
}

func TestValidateConfigRejectsDisabledMesh(t *testing.T) {
	cfg := validValidationConfig()
	disabled := false
	cfg.Mesh = &MeshConfig{Enabled: &disabled}

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject disabled mesh")
	}
	if !strings.Contains(err.Error(), "mesh.enabled=false") {
		t.Fatalf("error = %q, want mesh enabled error", err)
	}
}

func TestValidateConfigDefaultsRequiredRuntimeBooleans(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Runtime = &RuntimeConfig{Agent: &AgentConfig{}}
	cfg.Mesh = &MeshConfig{ListenPort: 42420}

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	if cfg.Runtime.Agent.Enabled == nil || !*cfg.Runtime.Agent.Enabled {
		t.Fatal("runtime agent should default to enabled")
	}
	if cfg.Mesh.Enabled == nil || !*cfg.Mesh.Enabled {
		t.Fatal("mesh should default to enabled")
	}
}

func TestValidateConfigRejectsHealthCheckPathWithoutSlash(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Port = 8080
	web.HealthCheck.Path = "health"
	production.Services["web"] = web
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject health check paths without a leading slash")
	}
	if !strings.Contains(err.Error(), "must start with /") {
		t.Fatalf("error = %q, want leading slash guidance", err)
	}
}

func TestValidateConfigRejectsHealthCheckPathWithControlCharacter(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Port = 8080
	web.HealthCheck.Path = "/health\nx"
	production.Services["web"] = web
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject health check paths with control characters")
	}
	if !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("error = %q, want control character guidance", err)
	}
}

func TestValidateConfigRejectsLoadBalancerHealthCheckPathWithoutSlash(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.LoadBalancer.HealthCheck.Enabled = true
	web.LoadBalancer.HealthCheck.Path = "health"
	production.Services["web"] = web
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject load balancer health check paths without a leading slash")
	}
	if !strings.Contains(err.Error(), "load balancer health check path") {
		t.Fatalf("error = %q, want load balancer health check path context", err)
	}
}

func TestExpandEnvWithTrimExpandsBracedVariables(t *testing.T) {
	t.Setenv("SERVER_HOST", "  203.0.113.10  ")

	expanded, err := expandEnvWithTrim("host: ${SERVER_HOST}\n", true)
	if err != nil {
		t.Fatalf("expandEnvWithTrim returned error: %v", err)
	}
	if expanded != "host: 203.0.113.10\n" {
		t.Fatalf("expanded = %q", expanded)
	}
}

func TestExpandEnvWithTrimPreservesSchemaKey(t *testing.T) {
	expanded, err := expandEnvWithTrim("$schema: https://example.test/schema.json\n", true)
	if err != nil {
		t.Fatalf("expandEnvWithTrim returned error: %v", err)
	}
	if expanded != "$schema: https://example.test/schema.json\n" {
		t.Fatalf("expanded = %q", expanded)
	}
}

func TestExpandEnvWithTrimIgnoresYAMLCommentPlaceholders(t *testing.T) {
	expanded, err := expandEnvWithTrim("# host: ${SERVER_HOST}\nhost: example.com # ${COMMENT_ONLY}\n", true)
	if err != nil {
		t.Fatalf("expandEnvWithTrim returned error: %v", err)
	}
	if expanded != "# host: ${SERVER_HOST}\nhost: example.com # ${COMMENT_ONLY}\n" {
		t.Fatalf("expanded = %q", expanded)
	}
}

func TestExpandEnvWithTrimExpandsQuotedYAMLHashContent(t *testing.T) {
	t.Setenv("FRAGMENT", "section")

	expanded, err := expandEnvWithTrim("url: \"https://example.com/#${FRAGMENT}\" # ${COMMENT_ONLY}\n", true)
	if err != nil {
		t.Fatalf("expandEnvWithTrim returned error: %v", err)
	}
	if expanded != "url: \"https://example.com/#section\" # ${COMMENT_ONLY}\n" {
		t.Fatalf("expanded = %q", expanded)
	}
}

func TestExpandEnvWithTrimReportsMissingVariables(t *testing.T) {
	_, err := expandEnvWithTrim("host: ${SERVER_HOST}\nemail: ${LETSENCRYPT_EMAIL}\n", true)
	if err == nil {
		t.Fatal("expandEnvWithTrim should report missing variables")
	}
	for _, want := range []string{"SERVER_HOST", "LETSENCRYPT_EMAIL"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want missing variable %s", err, want)
		}
	}
}

func TestExpandEnvWithTrimChecksJSONCommentsAsContent(t *testing.T) {
	_, err := expandEnvWithTrim(`{"note":"# ${SERVER_HOST}"}`, false)
	if err == nil {
		t.Fatal("expandEnvWithTrim should treat JSON strings as content")
	}
	if !strings.Contains(err.Error(), "SERVER_HOST") {
		t.Fatalf("error = %q, want SERVER_HOST", err)
	}
}

func validValidationConfig() *Config {
	return &Config{
		Project: ProjectConfig{Name: "demo", Version: "1.0.0"},
		Servers: map[string]ServerConfig{
			"node-a": {Host: "10.0.0.1", User: "deploy", Password: "${SSH_PASSWORD}"},
			"node-b": {Host: "10.0.0.2", User: "deploy", Password: "${SSH_PASSWORD}"},
		},
		Environments: map[string]EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b"},
				Services: map[string]ServiceConfig{
					"web": {Image: "nginx:alpine"},
				},
			},
		},
	}
}
