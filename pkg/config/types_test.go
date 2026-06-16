package config

import (
	"io"
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

func TestValidateConfigRejectsUnsafeServerName(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Servers["node-a\nbad"] = cfg.Servers["node-a"]
	delete(cfg.Servers, "node-a")
	production := cfg.Environments["production"]
	production.Servers = []string{"node-a\nbad", "node-b"}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject unsafe server names")
	}
	if !strings.Contains(err.Error(), "server name") {
		t.Fatalf("error = %q, want server name error", err)
	}
}

func TestValidateConfigRejectsUnsafeEnvironmentName(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Environments["prod/../../bad"] = cfg.Environments["production"]
	delete(cfg.Environments, "production")

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject unsafe environment names")
	}
	if !strings.Contains(err.Error(), "environment name") {
		t.Fatalf("error = %q, want environment name error", err)
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

func TestValidateConfigAcceptsDockerfileRelativeToBuildContext(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	root := t.TempDir()
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
	})
	if err := os.Chdir(root); err != nil {
		t.Fatalf("failed to switch cwd: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "packages", "web"), 0755); err != nil {
		t.Fatalf("failed to create package dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "packages", "web", "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatalf("failed to write Dockerfile: %v", err)
	}

	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Build:      ".",
		Dockerfile: "packages/web/Dockerfile",
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func TestValidateConfigRejectsUnsafeDockerfilePath(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Build:      ".",
		Dockerfile: "../Dockerfile",
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject dockerfile paths outside the build context")
	}
	if !strings.Contains(err.Error(), "invalid dockerfile path") {
		t.Fatalf("error = %q, want dockerfile context", err)
	}
}

func TestValidateConfigRejectsDockerfileWithoutBuild(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image:      "nginx:alpine",
		Dockerfile: "Dockerfile",
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject dockerfile without build")
	}
	if !strings.Contains(err.Error(), "dockerfile requires build") {
		t.Fatalf("error = %q, want dockerfile requires build", err)
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

func TestValidateConfigRejectsInvalidHealthCheckTiming(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Port = 8080
	web.HealthCheck.Path = "/health"
	web.HealthCheck.Interval = "not-a-duration"
	production.Services["web"] = web
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject invalid health check interval")
	}
	if !strings.Contains(err.Error(), "invalid health check interval") {
		t.Fatalf("error = %q, want health check interval context", err)
	}
}

func TestValidateConfigRejectsOversizedHealthCheckRetries(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Port = 8080
	web.HealthCheck.Path = "/health"
	web.HealthCheck.Retries = maxServiceHealthRetries + 1
	production.Services["web"] = web
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject oversized health check retries")
	}
	if !strings.Contains(err.Error(), "health check retries") {
		t.Fatalf("error = %q, want health check retries context", err)
	}
}

func TestValidateConfigAcceptsTCPHealthCheck(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	db := production.Services["web"]
	db.Port = 5432
	db.Proxy = nil
	db.HealthCheck.TCPPort = 5432
	production.Services["db"] = db
	delete(production.Services, "web")
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	got := cfg.Environments["production"].Services["db"].HealthCheck
	if got.Interval != "10s" || got.Timeout != "5s" || got.Retries != 3 {
		t.Fatalf("tcp health defaults = %#v", got)
	}
}

func TestValidateConfigRejectsAmbiguousHealthCheckProtocol(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Port = 8080
	web.HealthCheck.Path = "/health"
	web.HealthCheck.TCPPort = 8080
	production.Services["web"] = web
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject health checks with both path and tcpPort")
	}
	if !strings.Contains(err.Error(), "both path and tcpPort") {
		t.Fatalf("error = %q, want ambiguity guidance", err)
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

func TestNormalizeProxyDomainTrimsAndPreservesWildcard(t *testing.T) {
	got, err := NormalizeProxyDomain("  *.example.com  ")
	if err != nil {
		t.Fatalf("NormalizeProxyDomain returned error: %v", err)
	}
	if got != "*.example.com" {
		t.Fatalf("domain = %q, want wildcard preserved", got)
	}
}

func TestNormalizeProxyDomainRejectsRuleInjection(t *testing.T) {
	_, err := NormalizeProxyDomain("example.com`) || PathPrefix(`/")
	if err == nil {
		t.Fatal("NormalizeProxyDomain should reject rule injection characters")
	}
	if !strings.Contains(err.Error(), "invalid domain") {
		t.Fatalf("error = %q, want invalid domain", err)
	}
}

func TestValidateConfigTrimsProxyDomains(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image: "nginx:alpine",
		Proxy: &ProxyConfig{
			Domain:       " example.com ",
			RedirectFrom: []string{" www.example.com "},
		},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	got := cfg.Environments["production"].Services["web"].Proxy
	if got.Domain != "example.com" {
		t.Fatalf("domain = %q, want trimmed", got.Domain)
	}
	if got.RedirectFrom[0] != "www.example.com" {
		t.Fatalf("redirect domain = %q, want trimmed", got.RedirectFrom[0])
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

func TestLoadConfigWarningsUseStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tako.yaml")
	if err := os.WriteFile(path, []byte(`
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
    password: hardcoded-password
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
`), 0600); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	var loadErr error
	stdout, stderr := captureConfigOutput(t, func() {
		_, loadErr = LoadConfig(path)
	})
	if loadErr != nil {
		t.Fatalf("LoadConfig returned error: %v", loadErr)
	}
	if stdout != "" {
		t.Fatalf("LoadConfig warning wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "hardcoded password") {
		t.Fatalf("stderr = %q, want hardcoded password warning", stderr)
	}
}

func TestLoadEnvFileWarningsUseStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("INVALID_LINE\nGOOD=value\n"), 0600); err != nil {
		t.Fatalf("failed to write env file: %v", err)
	}

	var env map[string]string
	var loadErr error
	stdout, stderr := captureConfigOutput(t, func() {
		env, loadErr = LoadEnvFile(path)
	})
	if loadErr != nil {
		t.Fatalf("LoadEnvFile returned error: %v", loadErr)
	}
	if stdout != "" {
		t.Fatalf("LoadEnvFile warning wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "Invalid line") {
		t.Fatalf("stderr = %q, want invalid line warning", stderr)
	}
	if env["GOOD"] != "value" {
		t.Fatalf("GOOD = %q, want value", env["GOOD"])
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

func captureConfigOutput(t *testing.T, fn func()) (string, string) {
	t.Helper()

	originalStdout := os.Stdout
	originalStderr := os.Stderr
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stdout pipe: %v", err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stderr pipe: %v", err)
	}

	os.Stdout = stdoutWriter
	os.Stderr = stderrWriter
	defer func() {
		os.Stdout = originalStdout
		os.Stderr = originalStderr
	}()

	fn()

	if err := stdoutWriter.Close(); err != nil {
		t.Fatalf("failed to close stdout writer: %v", err)
	}
	if err := stderrWriter.Close(); err != nil {
		t.Fatalf("failed to close stderr writer: %v", err)
	}

	stdout, err := io.ReadAll(stdoutReader)
	if err != nil {
		t.Fatalf("failed to read stdout: %v", err)
	}
	stderr, err := io.ReadAll(stderrReader)
	if err != nil {
		t.Fatalf("failed to read stderr: %v", err)
	}
	if err := stdoutReader.Close(); err != nil {
		t.Fatalf("failed to close stdout reader: %v", err)
	}
	if err := stderrReader.Close(); err != nil {
		t.Fatalf("failed to close stderr reader: %v", err)
	}

	return string(stdout), string(stderr)
}
