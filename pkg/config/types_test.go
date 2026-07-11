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

func TestGetFullImageNameWithTagUsesExplicitTag(t *testing.T) {
	cfg := &Config{Project: ProjectConfig{Name: "demo", Version: "1.0.0"}}

	got := cfg.GetFullImageNameWithTag("web", "abcdef1234567890")
	if got != "demo/web:abcdef1234567890" {
		t.Fatalf("image = %q, want explicit commit tag", got)
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

func TestLoadConfigResolvesRelativePathsFromConfigDirectory(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	deployDir := filepath.Join(root, "deploy")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatalf("failed to create app dir: %v", err)
	}
	if err := os.MkdirAll(deployDir, 0755); err != nil {
		t.Fatalf("failed to create deploy dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatalf("failed to write Dockerfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deployDir, "service.env"), []byte("PORT=3000\n"), 0600); err != nil {
		t.Fatalf("failed to write env file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deployDir, "id_ed25519"), []byte("test-key"), 0600); err != nil {
		t.Fatalf("failed to write ssh key: %v", err)
	}
	path := filepath.Join(deployDir, "tako.yaml")
	if err := os.WriteFile(path, []byte(`
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 203.0.113.10
    user: deploy
    sshKey: id_ed25519
environments:
  production:
    servers: [node-a]
    services:
      web:
        build: ../app
        envFile: service.env
`), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	otherDir := filepath.Join(root, "other")
	if err := os.MkdirAll(otherDir, 0755); err != nil {
		t.Fatalf("failed to create other dir: %v", err)
	}
	if err := os.Chdir(otherDir); err != nil {
		t.Fatalf("failed to switch cwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
	})

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	web := cfg.Environments["production"].Services["web"]
	if got, want := filepath.Clean(web.Build), appDir; got != want {
		t.Fatalf("build path = %q, want %q", got, want)
	}
	if got, want := filepath.Clean(web.EnvFile), filepath.Join(deployDir, "service.env"); got != want {
		t.Fatalf("envFile = %q, want %q", got, want)
	}
	if got, want := filepath.Clean(cfg.Servers["node-a"].SSHKey), filepath.Join(deployDir, "id_ed25519"); got != want {
		t.Fatalf("sshKey = %q, want %q", got, want)
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

func TestValidateConfigRejectsPersistentServiceWithoutVolume(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["postgres"] = ServiceConfig{
		Image:      "postgres:16-alpine",
		Persistent: true,
		Env: map[string]string{
			"POSTGRES_PASSWORD": "secret",
		},
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject persistent services without volumes")
	}
	if !strings.Contains(err.Error(), "persistent services must declare at least one volume") {
		t.Fatalf("error = %q, want persistent volume error", err)
	}
}

func TestValidateConfigRejectsPersistentServiceWithoutMultiNodePlacement(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["postgres"] = ServiceConfig{
		Image:      "postgres:16-alpine",
		Persistent: true,
		Volumes:    []string{"pgdata:/var/lib/postgresql/data"},
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject floating persistent service in multi-node environment")
	}
	if !strings.Contains(err.Error(), "must set placement.strategy to pinned or global") {
		t.Fatalf("error = %q, want persistent placement guidance", err)
	}
}

func TestValidateConfigRejectsPersistentServiceWithSpreadPlacement(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["postgres"] = ServiceConfig{
		Image:      "postgres:16-alpine",
		Persistent: true,
		Volumes:    []string{"pgdata:/var/lib/postgresql/data"},
		Placement:  &PlacementConfig{Strategy: "spread"},
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject spread placement for persistent service")
	}
	if !strings.Contains(err.Error(), "must use placement.strategy pinned or global") {
		t.Fatalf("error = %q, want pinned/global guidance", err)
	}
}

func TestValidateConfigRejectsPersistentServiceWithReplicas(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["postgres"] = ServiceConfig{
		Image:      "postgres:16-alpine",
		Persistent: true,
		Replicas:   2,
		Volumes:    []string{"pgdata:/var/lib/postgresql/data"},
		Placement:  &PlacementConfig{Strategy: "pinned", Servers: []string{"node-a"}},
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject replicas on persistent services")
	}
	if !strings.Contains(err.Error(), "persistent services do not support replicas > 1") {
		t.Fatalf("error = %q, want persistent replicas guidance", err)
	}
}

func TestValidateConfigAcceptsServiceMemoryLimit(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Resources = &ResourceLimitsConfig{Memory: " 512M "}
	production.Services["web"] = web
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	got := cfg.Environments["production"].Services["web"].Resources.Memory
	if got != "512m" {
		t.Fatalf("resources.memory = %q, want normalized 512m", got)
	}
}

func TestValidateConfigRejectsUnsafeServiceMemoryLimit(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Resources = &ResourceLimitsConfig{Memory: "512m --privileged"}
	production.Services["web"] = web
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject unsafe resources.memory")
	}
	if !strings.Contains(err.Error(), "invalid resources.memory") {
		t.Fatalf("error = %q, want resources.memory guidance", err)
	}
}

func TestValidateConfigAcceptsBackupStorageConfig(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Volumes = []string{"data:/data"}
	web.Backup = &BackupConfig{
		Schedule: "@daily",
		Retain:   14,
		Volumes:  []string{"data"},
		Storage: &BackupStorageConfig{
			Provider:        BackupStorageProviderR2,
			Bucket:          "jardin-backups",
			Endpoint:        "https://account.r2.cloudflarestorage.com",
			AccessKeyID:     "access",
			SecretAccessKey: "secret",
		},
	}
	production.Services["web"] = web
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	got := cfg.Environments["production"].Services["web"].Backup.Storage
	if got.Region != "auto" {
		t.Fatalf("R2 backup region = %q, want auto", got.Region)
	}
}

func TestValidateConfigRejectsBackupVolumeOutsideService(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Volumes = []string{"data:/data"}
	web.Backup = &BackupConfig{Schedule: "@daily", Volumes: []string{"missing"}}
	production.Services["web"] = web
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject backup volume outside service")
	}
	if !strings.Contains(err.Error(), "backup volume") {
		t.Fatalf("error = %q, want backup volume guidance", err)
	}
}

func TestValidateConfigRejectsR2BackupStorageWithoutEndpoint(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Volumes = []string{"data:/data"}
	web.Backup = &BackupConfig{
		Schedule: "@daily",
		Storage: &BackupStorageConfig{
			Provider:        BackupStorageProviderR2,
			Bucket:          "backups",
			AccessKeyID:     "access",
			SecretAccessKey: "secret",
		},
	}
	production.Services["web"] = web
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject R2 storage without endpoint")
	}
	if !strings.Contains(err.Error(), "backup.storage.endpoint") {
		t.Fatalf("error = %q, want endpoint guidance", err)
	}
}

func TestValidateConfigAcceptsPersistentServiceWithPinnedPlacement(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Proxy = &EnvironmentProxyConfig{
		Placement: &PlacementConfig{Strategy: "pinned", Servers: []string{"node-a"}},
	}
	production.Services["postgres"] = ServiceConfig{
		Image:      "postgres:16-alpine",
		Persistent: true,
		Volumes:    []string{"pgdata:/var/lib/postgresql/data"},
		Placement:  &PlacementConfig{Strategy: "pinned", Servers: []string{"node-a"}},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func TestValidateConfigAcceptsPersistentServiceWithGlobalPlacement(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Proxy = &EnvironmentProxyConfig{
		Placement: &PlacementConfig{Strategy: "pinned", Servers: []string{"node-a"}},
	}
	production.Services["redis-agent"] = ServiceConfig{
		Image:      "redis:7-alpine",
		Persistent: true,
		Volumes:    []string{"redis_data:/data"},
		Placement:  &PlacementConfig{Strategy: "global"},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
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
	cfg.State = &StateConfig{}

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	if cfg.Runtime.Agent.Enabled == nil || !*cfg.Runtime.Agent.Enabled {
		t.Fatal("runtime agent should default to enabled")
	}
	if cfg.Mesh.Enabled == nil || !*cfg.Mesh.Enabled {
		t.Fatal("mesh should default to enabled")
	}
	if cfg.State.RemoteCacheEnabled == nil || !*cfg.State.RemoteCacheEnabled {
		t.Fatal("remote cache should default to enabled")
	}
	if !cfg.IsRemoteCacheEnabled() {
		t.Fatal("IsRemoteCacheEnabled should use the enabled default")
	}
}

func TestValidateConfigRejectsDisabledRemoteCache(t *testing.T) {
	cfg := validValidationConfig()
	disabled := false
	cfg.State = &StateConfig{RemoteCacheEnabled: &disabled}

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject disabled remote cache")
	}
	if !strings.Contains(err.Error(), "state.remoteCacheEnabled must be true") {
		t.Fatalf("error = %q, want remote cache error", err)
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
	if !strings.Contains(err.Error(), "only one of command, path, or tcpPort") {
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

func TestValidateConfigAcceptsEnvironmentProxyPlacement(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Servers["node-a"] = ServerConfig{Host: "10.0.0.1", User: "deploy", Password: "${SSH_PASSWORD}", Labels: map[string]string{"role": "edge"}}
	cfg.Servers["node-b"] = ServerConfig{Host: "10.0.0.2", User: "deploy", Password: "${SSH_PASSWORD}", Labels: map[string]string{"role": "worker"}}
	production := cfg.Environments["production"]
	production.Proxy = &EnvironmentProxyConfig{
		Placement: &PlacementConfig{
			Constraints: []string{"node.labels.role==edge"},
		},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}

	got, err := cfg.GetEnvironmentProxyServers("production")
	if err != nil {
		t.Fatalf("GetEnvironmentProxyServers returned error: %v", err)
	}
	if len(got) != 1 || got[0] != "node-a" {
		t.Fatalf("proxy servers = %#v, want node-a", got)
	}
}

func TestValidateConfigRejectsEnvironmentProxyPlacementOutsideEnvironment(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Servers["node-c"] = ServerConfig{Host: "10.0.0.3", User: "deploy", Password: "${SSH_PASSWORD}"}
	production := cfg.Environments["production"]
	production.Proxy = &EnvironmentProxyConfig{
		Placement: &PlacementConfig{
			Strategy: "pinned",
			Servers:  []string{"node-c"},
		},
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject proxy placement outside the environment")
	}
	if !strings.Contains(err.Error(), "environment production proxy placement") ||
		!strings.Contains(err.Error(), "outside the selected takod node set") {
		t.Fatalf("error = %q, want proxy placement environment guidance", err)
	}
}

func TestValidateConfigRejectsAutomaticACMEOnMultipleProxyNodes(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Proxy = &ProxyConfig{Domain: "example.com"}
	production.Services["web"] = web
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject automatic ACME on multiple proxy nodes")
	}
	for _, want := range []string{"automatic ACME TLS", "one proxy node", "node-a, node-b", "environment.proxy.placement"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestValidateConfigAllowsAutomaticACMEWithSingleProxyPlacement(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Proxy = &ProxyConfig{Domain: "example.com"}
	production.Services["web"] = web
	production.Proxy = &EnvironmentProxyConfig{
		Placement: &PlacementConfig{
			Strategy: "pinned",
			Servers:  []string{"node-a"},
		},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func TestValidateConfigRejectsUnsupportedLoadBalancerStrategy(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Replicas = 2
	web.LoadBalancer.Strategy = "ip_hash"
	production.Services["web"] = web
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject unsupported load balancer strategies")
	}
	for _, want := range []string{"invalid load balancer strategy", "round_robin and sticky"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestValidateConfigDefaultsDeploymentStrategy(t *testing.T) {
	cfg := validValidationConfig()

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}

	got := cfg.Environments["production"].Services["web"].Deploy.Strategy
	if got != DeployStrategyRecreate {
		t.Fatalf("deploy strategy = %q, want %q", got, DeployStrategyRecreate)
	}
}

func TestValidateConfigDefaultsBuildStrategyRemote(t *testing.T) {
	cfg := validValidationConfig()

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}

	if got := cfg.GetBuildStrategy(); got != BuildStrategyRemote {
		t.Fatalf("build strategy = %q, want %q", got, BuildStrategyRemote)
	}
}

func TestValidateConfigAcceptsBuildStrategies(t *testing.T) {
	for _, strategy := range []string{BuildStrategyRemote, BuildStrategyLocal, BuildStrategyAuto, " LOCAL "} {
		t.Run(strategy, func(t *testing.T) {
			cfg := validValidationConfig()
			cfg.Deployment = &DeploymentConfig{
				Build: &BuildConfig{Strategy: strategy},
			}

			if err := ValidateConfig(cfg); err != nil {
				t.Fatalf("ValidateConfig returned error: %v", err)
			}
			if got := cfg.GetBuildStrategy(); got == "" || got != strings.ToLower(strings.TrimSpace(strategy)) {
				t.Fatalf("build strategy = %q, want normalized %q", got, strings.ToLower(strings.TrimSpace(strategy)))
			}
		})
	}
}

func TestValidateConfigRejectsUnknownBuildStrategy(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Deployment = &DeploymentConfig{
		Build: &BuildConfig{Strategy: "registry"},
	}

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject unsupported build strategy")
	}
	if !strings.Contains(err.Error(), "deployment.build.strategy must be remote, local, or auto") {
		t.Fatalf("error = %q, want build strategy guidance", err)
	}
}

func TestValidateConfigRejectsUnknownDeploymentStrategy(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Deploy.Strategy = "canary"
	production.Services["web"] = web
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject unsupported deployment strategies")
	}
	for _, want := range []string{"invalid deployment strategy", "recreate, rolling, and blue_green"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestValidateConfigAllowsConstrainedRollingStrategy(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Replicas = 2
	web.Deploy.Strategy = DeployStrategyRolling
	web.Deploy.Readiness.Path = "/ready"
	production.Services["web"] = web
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig should allow constrained rolling strategy: %v", err)
	}
}

func TestValidateConfigAllowsConstrainedBlueGreenStrategy(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Replicas = 2
	web.Deploy.Strategy = DeployStrategyBlueGreen
	web.Deploy.Readiness.Path = "/ready"
	web.Deploy.Promotion = DeployPromotionAutomatic
	configureValidationWebProxy(&production, &web)
	production.Services["web"] = web
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig should allow constrained blue_green strategy: %v", err)
	}
}

func TestValidateConfigAllowsBlueGreenSmokeTest(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Replicas = 2
	web.Deploy.Strategy = DeployStrategyBlueGreen
	web.Deploy.Readiness.Path = "/ready"
	web.Deploy.SmokeTest.Path = "/smoke"
	web.Deploy.SmokeTest.ExpectedStatus = 204
	configureValidationWebProxy(&production, &web)
	production.Services["web"] = web
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig should allow blue_green smoke tests: %v", err)
	}
}

func TestValidateConfigAllowsBlueGreenManualPromotion(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Replicas = 2
	web.Deploy.Strategy = DeployStrategyBlueGreen
	web.Deploy.Promotion = DeployPromotionManual
	configureValidationWebProxy(&production, &web)
	production.Services["web"] = web
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig should allow blue_green manual promotion: %v", err)
	}
}

func TestValidateConfigAllowsBlueGreenGracePeriod(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Replicas = 2
	web.Deploy.Strategy = DeployStrategyBlueGreen
	web.Deploy.GracePeriod = "30s"
	configureValidationWebProxy(&production, &web)
	production.Services["web"] = web
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig should allow blue_green grace periods: %v", err)
	}
}

func TestValidateConfigRejectsUnsupportedNoDowntimeStrategyOptions(t *testing.T) {
	tests := []struct {
		name     string
		strategy string
		mutate   func(*ServiceConfig)
		want     string
	}{
		{
			name:     "blue green requires proxy",
			strategy: DeployStrategyBlueGreen,
			want:     "requires a public proxy route",
		},
		{
			name:     "persistent rolling",
			strategy: DeployStrategyRolling,
			mutate: func(web *ServiceConfig) {
				web.Persistent = true
				web.Volumes = []string{"data:/data"}
			},
			want: "not supported for persistent services",
		},
		{
			name:     "rolling max unavailable",
			strategy: DeployStrategyRolling,
			mutate: func(web *ServiceConfig) {
				web.Deploy.MaxUnavailable = 1
			},
			want: "deploy.maxUnavailable is not supported for rolling yet",
		},
		{
			name:     "rolling max surge smaller than replicas",
			strategy: DeployStrategyRolling,
			mutate: func(web *ServiceConfig) {
				web.Replicas = 3
				web.Deploy.MaxSurge = 1
			},
			want: "deploy.maxSurge must be at least replicas",
		},
		{
			name:     "negative max surge",
			strategy: DeployStrategyRolling,
			mutate: func(web *ServiceConfig) {
				web.Deploy.MaxSurge = -1
			},
			want: "deploy.maxSurge cannot be negative",
		},
		{
			name:     "invalid promotion",
			strategy: DeployStrategyRolling,
			mutate: func(web *ServiceConfig) {
				web.Deploy.Promotion = "now"
			},
			want: "deploy.promotion must be automatic or manual",
		},
		{
			name:     "rolling promotion unsupported",
			strategy: DeployStrategyRolling,
			mutate: func(web *ServiceConfig) {
				web.Deploy.Promotion = DeployPromotionManual
			},
			want: "deploy.promotion is only supported by blue_green",
		},
		{
			name:     "rolling grace period unsupported",
			strategy: DeployStrategyRolling,
			mutate: func(web *ServiceConfig) {
				web.Deploy.GracePeriod = "30s"
			},
			want: "deploy.gracePeriod is only supported by blue_green",
		},
		{
			name:     "invalid readiness",
			strategy: DeployStrategyRolling,
			mutate: func(web *ServiceConfig) {
				web.Deploy.Readiness.Path = "ready"
			},
			want: "invalid deploy.readiness.path",
		},
		{
			name:     "invalid smoke status",
			strategy: DeployStrategyRolling,
			mutate: func(web *ServiceConfig) {
				web.Deploy.SmokeTest.ExpectedStatus = 99
			},
			want: "deploy.smokeTest.expectedStatus must be between 100 and 599",
		},
		{
			name:     "rolling smoke test unsupported",
			strategy: DeployStrategyRolling,
			mutate: func(web *ServiceConfig) {
				web.Deploy.SmokeTest.Path = "/smoke"
			},
			want: "deploy.smokeTest is only supported by blue_green",
		},
		{
			name:     "blue green max unavailable",
			strategy: DeployStrategyBlueGreen,
			mutate: func(web *ServiceConfig) {
				web.Deploy.MaxUnavailable = 1
			},
			want: "deploy.maxUnavailable is not supported for blue_green yet",
		},
		{
			name:     "blue green max surge smaller than replicas",
			strategy: DeployStrategyBlueGreen,
			mutate: func(web *ServiceConfig) {
				web.Replicas = 3
				web.Deploy.MaxSurge = 1
			},
			want: "deploy.maxSurge must be at least replicas",
		},
		{
			name:     "blue green smoke test requires port",
			strategy: DeployStrategyBlueGreen,
			mutate: func(web *ServiceConfig) {
				web.Port = 0
				web.Deploy.SmokeTest.Path = "/smoke"
				web.Deploy.SmokeTest.ExpectedStatus = 200
			},
			want: "deploy.smokeTest requires service port",
		},
		{
			name:     "blue green invalid grace period",
			strategy: DeployStrategyBlueGreen,
			mutate: func(web *ServiceConfig) {
				web.Deploy.GracePeriod = "soon"
			},
			want: "deploy.gracePeriod must be a duration",
		},
		{
			name:     "blue green negative grace period",
			strategy: DeployStrategyBlueGreen,
			mutate: func(web *ServiceConfig) {
				web.Deploy.GracePeriod = "-1s"
			},
			want: "deploy.gracePeriod cannot be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validValidationConfig()
			production := cfg.Environments["production"]
			web := production.Services["web"]
			web.Deploy.Strategy = tt.strategy
			if tt.strategy == DeployStrategyBlueGreen && !strings.Contains(tt.want, "requires a public proxy route") {
				configureValidationWebProxy(&production, &web)
			}
			if tt.mutate != nil {
				tt.mutate(&web)
			}
			production.Services["web"] = web
			cfg.Environments["production"] = production

			err := ValidateConfig(cfg)
			if err == nil {
				t.Fatal("ValidateConfig should reject unsupported no-downtime strategy option")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateConfigRejectsWildcardProxyDomains(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ProxyConfig)
		want   string
	}{
		{
			name: "primary",
			mutate: func(proxy *ProxyConfig) {
				proxy.Domain = "*.example.com"
			},
			want: "wildcard proxy domain",
		},
		{
			name: "redirect",
			mutate: func(proxy *ProxyConfig) {
				proxy.RedirectFrom = []string{"*.old.example.com"}
			},
			want: "wildcard redirect domain",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validValidationConfig()
			production := cfg.Environments["production"]
			web := production.Services["web"]
			web.Proxy = &ProxyConfig{Domain: "example.com"}
			tt.mutate(web.Proxy)
			production.Services["web"] = web
			cfg.Environments["production"] = production

			err := ValidateConfig(cfg)
			if err == nil {
				t.Fatal("ValidateConfig should reject wildcard proxy domains")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err, tt.want)
			}
		})
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
	production.Proxy = &EnvironmentProxyConfig{
		Placement: &PlacementConfig{
			Strategy: "pinned",
			Servers:  []string{"node-a"},
		},
	}
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

func TestValidateConfigDefaultsInternalProxyHost(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image: "nginx:alpine",
		Port:  3000,
		Proxy: &ProxyConfig{
			Visibility: ProxyVisibilityInternal,
		},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	proxy := cfg.Environments["production"].Services["web"].Proxy
	if proxy.Host != "web.production.demo.tako.internal" {
		t.Fatalf("host = %q, want default internal host", proxy.Host)
	}
	if proxy.Domain != "" {
		t.Fatalf("domain = %q, want no public domain", proxy.Domain)
	}
	if proxy.TLS.Mode != ProxyTLSModeOff {
		t.Fatalf("tls.mode = %q, want off", proxy.TLS.Mode)
	}
	if proxy.GetPrimaryDomain() != "" {
		t.Fatalf("primary domain = %q, want empty", proxy.GetPrimaryDomain())
	}
	if got := proxy.GetPrimaryHost(); got != "web.production.demo.tako.internal" {
		t.Fatalf("primary host = %q", got)
	}
}

func TestValidateConfigAllowsInternalProxyOnMultipleProxyNodes(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image: "nginx:alpine",
		Port:  3000,
		Proxy: &ProxyConfig{
			Host:       "web.internal.example",
			Visibility: ProxyVisibilityInternal,
		},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func TestValidateConfigRejectsPublicProxyHost(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image: "nginx:alpine",
		Port:  3000,
		Proxy: &ProxyConfig{
			Domain: "web.example.com",
			Host:   "web.production.demo.tako.internal",
		},
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject proxy.host on public routes")
	}
	if !strings.Contains(err.Error(), "proxy.host is only supported") {
		t.Fatalf("error = %q, want proxy.host guidance", err)
	}
}

func TestValidateConfigAllowsDynamicDomainProxyWithoutFixedDomain(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Proxy = &EnvironmentProxyConfig{
		Placement: &PlacementConfig{
			Strategy: "pinned",
			Servers:  []string{"node-a"},
		},
	}
	production.Services["admin"] = ServiceConfig{
		Image: "nginx:alpine",
		Port:  4000,
	}
	production.Services["web"] = ServiceConfig{
		Image: "nginx:alpine",
		Port:  3000,
		Proxy: &ProxyConfig{
			DynamicDomains: &DynamicDomainsConfig{Ask: "admin:/api/domains/authorize"},
		},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func TestValidateConfigRejectsRedirectFromWithoutFixedDomain(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["admin"] = ServiceConfig{
		Image: "nginx:alpine",
		Port:  4000,
	}
	production.Services["web"] = ServiceConfig{
		Image: "nginx:alpine",
		Port:  3000,
		Proxy: &ProxyConfig{
			RedirectFrom:   []string{"www.example.com"},
			DynamicDomains: &DynamicDomainsConfig{Ask: "admin:/api/domains/authorize"},
		},
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject redirectFrom without a primary domain")
	}
	if !strings.Contains(err.Error(), "redirectFrom requires a primary proxy domain") {
		t.Fatalf("error = %q, want redirectFrom guidance", err)
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

func configureValidationWebProxy(env *EnvironmentConfig, service *ServiceConfig) {
	env.Proxy = &EnvironmentProxyConfig{
		Placement: &PlacementConfig{
			Strategy: "pinned",
			Servers:  []string{"node-a"},
		},
	}
	service.Port = 8080
	service.Proxy = &ProxyConfig{Domain: "web.example.com"}
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

func TestValidateConfigAcceptsJobService(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["report"] = ServiceConfig{
		Kind:     ServiceKindJob,
		Image:    "busybox:1.36",
		Schedule: "*/5 * * * *",
		Timezone: "UTC",
		Timeout:  "30m",
		Command:  StringValue("echo report"),
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig rejected a valid job: %v", err)
	}
}

func TestValidateConfigRejectsInvalidJobServices(t *testing.T) {
	cases := []struct {
		name    string
		service ServiceConfig
		want    string
	}{
		{"missing schedule", ServiceConfig{Kind: ServiceKindJob, Image: "busybox", Command: StringValue("true")}, "requires a schedule"},
		{"bad schedule", ServiceConfig{Kind: ServiceKindJob, Image: "busybox", Schedule: "often", Command: StringValue("true")}, "invalid schedule"},
		{"missing command", ServiceConfig{Kind: ServiceKindJob, Image: "busybox", Schedule: "@hourly"}, "requires a command"},
		{"bad timezone", ServiceConfig{Kind: ServiceKindJob, Image: "busybox", Schedule: "@hourly", Command: StringValue("true"), Timezone: "Mars/Olympus"}, "invalid timezone"},
		{"proxied job", ServiceConfig{Kind: ServiceKindJob, Image: "busybox", Schedule: "@hourly", Command: StringValue("true"), Proxy: &ProxyConfig{Domain: "example.com"}}, "cannot be proxied"},
		{"job replicas", ServiceConfig{Kind: ServiceKindJob, Image: "busybox", Schedule: "@hourly", Command: StringValue("true"), Replicas: 3}, "cannot set replicas"},
		{"persistent job", ServiceConfig{Kind: ServiceKindJob, Image: "busybox", Schedule: "@hourly", Command: StringValue("true"), Persistent: true}, "cannot be persistent"},
		{"bad kind", ServiceConfig{Kind: "cronjob", Image: "busybox"}, "kind must be service, job, or run"},
		{"schedule on plain service", ServiceConfig{Image: "busybox", Schedule: "@hourly"}, "schedule requires kind: job"},
	}
	for _, tc := range cases {
		cfg := validValidationConfig()
		production := cfg.Environments["production"]
		production.Services["candidate"] = tc.service
		cfg.Environments["production"] = production

		err := ValidateConfig(cfg)
		if err == nil {
			t.Fatalf("%s: config accepted", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %q, want %q", tc.name, err, tc.want)
		}
	}
}
