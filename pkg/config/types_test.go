package config

import (
	"strings"
	"testing"
)

func TestGetNFSServerNameUsesEnvironmentDefault(t *testing.T) {
	cfg := nfsServerNameTestConfig("auto")

	serverName, err := cfg.GetNFSServerName("production")
	if err != nil {
		t.Fatalf("GetNFSServerName returned error: %v", err)
	}
	if serverName != "node-a" {
		t.Fatalf("server name = %q, want node-a", serverName)
	}
}

func TestGetNFSServerNameAcceptsEnvironmentNode(t *testing.T) {
	cfg := nfsServerNameTestConfig("node-b")

	serverName, err := cfg.GetNFSServerName("production")
	if err != nil {
		t.Fatalf("GetNFSServerName returned error: %v", err)
	}
	if serverName != "node-b" {
		t.Fatalf("server name = %q, want node-b", serverName)
	}
}

func TestGetNFSServerNameRejectsServerOutsideEnvironment(t *testing.T) {
	cfg := nfsServerNameTestConfig("storage")

	if _, err := cfg.GetNFSServerName("production"); err == nil {
		t.Fatal("GetNFSServerName should reject an NFS server outside the environment")
	}
}

func TestValidateConfigRejectsNFSServerOutsideEnvironment(t *testing.T) {
	cfg := validNFSValidationConfig("storage")
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image:   "nginx:alpine",
		Volumes: []string{"nfs:shared_data:/data:rw"},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("ValidateConfig should reject an NFS server outside the environment")
	}
}

func TestValidateConfigAllowsNFSServerOutsideEnvironmentWithoutNFSVolumes(t *testing.T) {
	cfg := validNFSValidationConfig("storage")

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func TestValidateConfigAllowsNFSServerOutsideUnusedEnvironment(t *testing.T) {
	cfg := validNFSValidationConfig("node-b")
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image:   "nginx:alpine",
		Volumes: []string{"nfs:shared_data:/data:rw"},
	}
	cfg.Environments["production"] = production
	cfg.Environments["staging"] = EnvironmentConfig{
		Servers: []string{"storage"},
		Services: map[string]ServiceConfig{
			"web": {Image: "nginx:alpine"},
		},
	}

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func TestValidateConfigAcceptsEnvironmentNFSServer(t *testing.T) {
	cfg := validNFSValidationConfig("node-b")
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image:   "nginx:alpine",
		Volumes: []string{"nfs:shared_data:/data:rw"},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func TestValidateConfigRejectsNFSVolumeWithoutNFSConfig(t *testing.T) {
	cfg := validNFSValidationConfig("node-b")
	cfg.Storage = nil
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image:   "nginx:alpine",
		Volumes: []string{"nfs:shared_data:/data:rw"},
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject NFS volumes when NFS is disabled")
	}
	if !strings.Contains(err.Error(), "requires storage.nfs.enabled") {
		t.Fatalf("error = %q, want storage.nfs.enabled error", err)
	}
}

func TestValidateConfigRejectsNFSVolumeMissingExport(t *testing.T) {
	cfg := validNFSValidationConfig("node-b")
	cfg.Storage.NFS.Exports = nil
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image:   "nginx:alpine",
		Volumes: []string{"nfs:missing:/data:rw"},
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject NFS volumes that reference missing exports")
	}
	if !strings.Contains(err.Error(), "references missing export") {
		t.Fatalf("error = %q, want missing export error", err)
	}
}

func TestValidateConfigRejectsDisabledRuntimeAgent(t *testing.T) {
	cfg := validNFSValidationConfig("node-b")
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
	cfg := validNFSValidationConfig("node-b")
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
	cfg := validNFSValidationConfig("node-b")
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
	cfg := validNFSValidationConfig("node-b")
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
	cfg := validNFSValidationConfig("node-b")
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
	cfg := validNFSValidationConfig("node-b")
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

func TestEnvironmentUsesNFSVolumes(t *testing.T) {
	env := EnvironmentConfig{
		Services: map[string]ServiceConfig{
			"web": {
				Image:   "nginx:alpine",
				Volumes: []string{"data:/data", "nfs:shared_data:/shared:ro"},
			},
		},
	}

	if !EnvironmentUsesNFSVolumes(env) {
		t.Fatal("EnvironmentUsesNFSVolumes should detect nfs volume specs")
	}
}

func TestEnvironmentUsesNFSVolumesReturnsFalseWithoutNFS(t *testing.T) {
	env := EnvironmentConfig{
		Services: map[string]ServiceConfig{
			"web": {
				Image:   "nginx:alpine",
				Volumes: []string{"data:/data"},
			},
		},
	}

	if EnvironmentUsesNFSVolumes(env) {
		t.Fatal("EnvironmentUsesNFSVolumes should ignore non-NFS volumes")
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

func nfsServerNameTestConfig(server string) *Config {
	return &Config{
		Storage: &StorageConfig{
			NFS: &NFSConfig{
				Enabled: true,
				Server:  server,
				Exports: []NFSExportConfig{
					{Name: "shared_data", Path: "/srv/tako/shared"},
				},
			},
		},
		Servers: map[string]ServerConfig{
			"node-a":  {Host: "10.0.0.1"},
			"node-b":  {Host: "10.0.0.2"},
			"storage": {Host: "10.0.0.9"},
		},
		Environments: map[string]EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b"},
			},
		},
	}
}

func validNFSValidationConfig(server string) *Config {
	cfg := nfsServerNameTestConfig(server)
	cfg.Project = ProjectConfig{Name: "demo", Version: "1.0.0"}
	cfg.Servers["node-a"] = ServerConfig{Host: "10.0.0.1", User: "deploy", Password: "${SSH_PASSWORD}"}
	cfg.Servers["node-b"] = ServerConfig{Host: "10.0.0.2", User: "deploy", Password: "${SSH_PASSWORD}"}
	cfg.Servers["storage"] = ServerConfig{Host: "10.0.0.9", User: "deploy", Password: "${SSH_PASSWORD}"}
	cfg.Environments["production"] = EnvironmentConfig{
		Servers: []string{"node-a", "node-b"},
		Services: map[string]ServiceConfig{
			"web": {Image: "nginx:alpine"},
		},
	}
	return cfg
}
