package config

import (
	"crypto/sha256"
	"encoding/hex"
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

func TestValidateConfigRejectsRelativeBindMounts(t *testing.T) {
	tests := []string{
		"./data:/data",
		"../data:/data",
		"data/files:/data",
		"data:relative",
		"cache:/cache:shared",
		"data",
	}
	for _, volume := range tests {
		t.Run(volume, func(t *testing.T) {
			cfg := validValidationConfig()
			production := cfg.Environments["production"]
			service := production.Services["web"]
			service.Volumes = []string{volume}
			production.Services["web"] = service
			cfg.Environments["production"] = production

			err := ValidateConfig(cfg)
			if err == nil {
				t.Fatal("ValidateConfig should reject invalid relative volume specs")
			}
			if !strings.Contains(err.Error(), "volume") {
				t.Fatalf("error = %q, want volume context", err)
			}
		})
	}
}

func TestValidateConfigAcceptsSupportedVolumeSpecs(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	service := production.Services["web"]
	service.Volumes = []string{"/data", "cache:/cache", "cache:/readonly:ro", "/srv/app/data:/data", "/srv/app/config:/config:ro"}
	production.Services["web"] = service
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func TestValidateConfigAcceptsConfigFileMount(t *testing.T) {
	root := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	mustWriteConfigTestFile(t, filepath.Join(root, "ops", "caddy", "Caddyfile"), ":80 {\n respond \"ok\"\n}\n")
	cfg := validValidationConfig()
	cfg.Configs = map[string]ConfigFileConfig{
		"caddyfile": {Source: "./ops/caddy/Caddyfile"},
	}
	production := cfg.Environments["production"]
	service := production.Services["web"]
	service.Configs = []ServiceConfigFileMount{{
		Source: "caddyfile",
		Target: "/etc/caddy/../caddy/Caddyfile",
	}}
	production.Services["web"] = service
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	if cfg.Configs["caddyfile"].Source != "ops/caddy/Caddyfile" {
		t.Fatalf("config source = %q, want normalized path", cfg.Configs["caddyfile"].Source)
	}
	got := cfg.Environments["production"].Services["web"].Configs[0]
	if got.Target != "/etc/caddy/Caddyfile" {
		t.Fatalf("target = %q, want normalized target", got.Target)
	}
	if got.Mode != "0444" {
		t.Fatalf("mode = %q, want default 0444", got.Mode)
	}
	sum := sha256.Sum256([]byte(":80 {\n respond \"ok\"\n}\n"))
	if got.ContentHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("content hash = %q, want SHA-256", got.ContentHash)
	}
}

func TestValidateConfigAcceptsGeneratedCaddyConfigMount(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Project.Name = "edge"
	cfg.Imports = map[string]ImportConfig{
		"jardin_admin": {
			Project:     "jardin-cms",
			Environment: "production",
			Service:     "admin",
			Port:        "web",
			Servers:     []string{"node-a"},
		},
		"jardin_renderer": {
			Project:     "jardin-cms",
			Environment: "production",
			Service:     "renderer",
			Port:        "web",
			Servers:     []string{"node-a"},
		},
	}
	cfg.Configs = map[string]ConfigFileConfig{
		"caddyfile": {
			Generate: &GeneratedConfigConfig{
				Caddy: &GeneratedCaddyConfig{
					Email:          "ops@example.com",
					AdminHost:      " admin.example.com ",
					SiteHost:       " sites.example.com ",
					AdminImport:    "jardin_admin",
					RendererImport: "jardin_renderer",
					OnDemandTLS:    true,
					AskPath:        "/api/platform/domains/ask",
				},
			},
		},
	}
	production := cfg.Environments["production"]
	service := production.Services["web"]
	service.Configs = []ServiceConfigFileMount{{
		Source: "caddyfile",
		Target: "/etc/caddy/Caddyfile",
	}}
	production.Services["web"] = service
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	caddy := cfg.Configs["caddyfile"].Generate.Caddy
	if caddy.AdminHost != "admin.example.com" || caddy.SiteHost != "sites.example.com" {
		t.Fatalf("hosts were not normalized: %#v", caddy)
	}
	if caddy.AskImport != "jardin_admin" {
		t.Fatalf("ask import = %q, want default admin import", caddy.AskImport)
	}
	mount := cfg.Environments["production"].Services["web"].Configs[0]
	if mount.ContentHash != "" {
		t.Fatalf("generated content hash should be resolved at deploy time, got %q", mount.ContentHash)
	}
}

func TestValidateConfigRejectsInvalidGeneratedCaddyConfig(t *testing.T) {
	tests := []struct {
		name   string
		update func(*Config)
	}{
		{
			name: "source and generate",
			update: func(cfg *Config) {
				cfg.Configs["caddyfile"] = ConfigFileConfig{
					Source: "Caddyfile",
					Generate: &GeneratedConfigConfig{
						Caddy: &GeneratedCaddyConfig{Email: "ops@example.com", AdminHost: "admin.example.com", SiteHost: "sites.example.com", AdminImport: "jardin_admin", RendererImport: "jardin_renderer"},
					},
				}
			},
		},
		{
			name: "unknown import",
			update: func(cfg *Config) {
				configFile := generatedCaddyTestConfig()
				configFile.Generate.Caddy.AdminImport = "missing"
				cfg.Configs["caddyfile"] = configFile
			},
		},
		{
			name: "unsafe email",
			update: func(cfg *Config) {
				configFile := generatedCaddyTestConfig()
				configFile.Generate.Caddy.Email = "ops@example.com }"
				cfg.Configs["caddyfile"] = configFile
			},
		},
		{
			name: "on demand missing ask path",
			update: func(cfg *Config) {
				configFile := generatedCaddyTestConfig()
				configFile.Generate.Caddy.AskPath = ""
				cfg.Configs["caddyfile"] = configFile
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validGeneratedCaddyValidationConfig()
			tt.update(cfg)
			if err := ValidateConfig(cfg); err == nil {
				t.Fatal("ValidateConfig should reject invalid generated config")
			}
		})
	}
}

func validGeneratedCaddyValidationConfig() *Config {
	cfg := validValidationConfig()
	cfg.Project.Name = "edge"
	cfg.Imports = map[string]ImportConfig{
		"jardin_admin": {
			Project:     "jardin-cms",
			Environment: "production",
			Service:     "admin",
			Port:        "web",
			Servers:     []string{"node-a"},
		},
		"jardin_renderer": {
			Project:     "jardin-cms",
			Environment: "production",
			Service:     "renderer",
			Port:        "web",
			Servers:     []string{"node-a"},
		},
	}
	cfg.Configs = map[string]ConfigFileConfig{
		"caddyfile": generatedCaddyTestConfig(),
	}
	production := cfg.Environments["production"]
	service := production.Services["web"]
	service.Configs = []ServiceConfigFileMount{{
		Source: "caddyfile",
		Target: "/etc/caddy/Caddyfile",
	}}
	production.Services["web"] = service
	cfg.Environments["production"] = production
	return cfg
}

func generatedCaddyTestConfig() ConfigFileConfig {
	return ConfigFileConfig{
		Generate: &GeneratedConfigConfig{
			Caddy: &GeneratedCaddyConfig{
				Email:          "ops@example.com",
				AdminHost:      "admin.example.com",
				SiteHost:       "sites.example.com",
				AdminImport:    "jardin_admin",
				RendererImport: "jardin_renderer",
				OnDemandTLS:    true,
				AskPath:        "/api/platform/domains/ask",
			},
		},
	}
}

func TestValidateConfigRejectsInvalidConfigFileMounts(t *testing.T) {
	root := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	mustWriteConfigTestFile(t, filepath.Join(root, "Caddyfile"), ":80 {}\n")
	tests := []struct {
		name   string
		source string
		target string
		mode   string
	}{
		{name: "undefined source", source: "missing", target: "/etc/caddy/Caddyfile", mode: "0444"},
		{name: "relative target", source: "caddyfile", target: "etc/caddy/Caddyfile", mode: "0444"},
		{name: "writable mode", source: "caddyfile", target: "/etc/caddy/Caddyfile", mode: "0644"},
		{name: "target comma", source: "caddyfile", target: "/etc/caddy/Caddy,file", mode: "0444"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validValidationConfig()
			cfg.Configs = map[string]ConfigFileConfig{
				"caddyfile": {Source: "Caddyfile"},
			}
			production := cfg.Environments["production"]
			service := production.Services["web"]
			service.Configs = []ServiceConfigFileMount{{
				Source: tt.source,
				Target: tt.target,
				Mode:   tt.mode,
			}}
			production.Services["web"] = service
			cfg.Environments["production"] = production

			if err := ValidateConfig(cfg); err == nil {
				t.Fatal("ValidateConfig should reject invalid config mount")
			}
		})
	}
}

func TestValidateConfigRejectsConfigFileSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	mustWriteConfigTestFile(t, filepath.Join(outside, "Caddyfile"), ":80 {}\n")
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	cfg := validValidationConfig()
	cfg.Configs = map[string]ConfigFileConfig{
		"caddyfile": {Source: "linked/Caddyfile"},
	}
	production := cfg.Environments["production"]
	service := production.Services["web"]
	service.Configs = []ServiceConfigFileMount{{
		Source: "caddyfile",
		Target: "/etc/caddy/Caddyfile",
	}}
	production.Services["web"] = service
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("ValidateConfig should reject config source outside checkout through parent symlink")
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

func TestValidateConfigAcceptsExplicitExportsAndImports(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Servers["app-node"] = ServerConfig{Host: "10.0.0.2", User: "root", Password: "${SSH_PASSWORD}"}
	cfg.Imports = map[string]ImportConfig{
		"jardin_renderer": {
			Project:     "jardin-cms",
			Environment: "production",
			Service:     "site-renderer",
			Port:        "web",
			Servers:     []string{"app-node"},
		},
	}
	production := cfg.Environments["production"]
	service := production.Services["web"]
	service.Port = 3000
	service.Export = &ServiceExportConfig{
		Ports: map[string]int{"web": 3000},
	}
	production.Services["web"] = service
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	if cfg.Imports["jardin_renderer"].Servers[0] != "app-node" {
		t.Fatalf("import servers = %#v, want normalized server list", cfg.Imports["jardin_renderer"].Servers)
	}
	if cfg.Environments["production"].Services["web"].Export.Ports["web"] != 3000 {
		t.Fatalf("export ports not preserved: %#v", cfg.Environments["production"].Services["web"].Export)
	}
}

func TestValidateConfigAcceptsShareAndLocalEnvLink(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["admin"] = ServiceConfig{
		Image: "nginx:alpine",
		Port:  3000,
		Share: &ServiceShareConfig{Enabled: true},
	}
	production.Services["renderer"] = ServiceConfig{
		Image: "nginx:alpine",
		Port:  3000,
		Env: map[string]EnvValue{
			"CMS_ADMIN_API_BASE_URL": {Link: &ServiceLinkRef{Service: "admin"}},
		},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	admin := cfg.Environments["production"].Services["admin"]
	if admin.Export == nil || admin.Export.Ports[DefaultSharedPortName] != 3000 {
		t.Fatalf("share did not normalize to default export: %#v", admin.Export)
	}
}

func TestLoadConfigAcceptsCleanLinkSyntax(t *testing.T) {
	t.Setenv("SSH_PASSWORD", "test-password")
	path := filepath.Join(t.TempDir(), "tako.yaml")
	if err := os.WriteFile(path, []byte(`
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
    password: ${SSH_PASSWORD}
environments:
  production:
    servers: [node-a]
    services:
      admin:
        image: nginx:alpine
        port: 3000
        share: true
      renderer:
        image: nginx:alpine
        port: 3000
        env:
          CMS_ADMIN_API_BASE_URL:
            link: admin
          PUBLIC_ADMIN_URL:
            url: https://admin.example.com
`), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	renderer := cfg.Environments["production"].Services["renderer"]
	if renderer.Env["CMS_ADMIN_API_BASE_URL"].Link == nil || renderer.Env["CMS_ADMIN_API_BASE_URL"].Link.Service != "admin" {
		t.Fatalf("link env did not parse: %#v", renderer.Env["CMS_ADMIN_API_BASE_URL"])
	}
	if got := renderer.Env["PUBLIC_ADMIN_URL"].URL; got != "https://admin.example.com" {
		t.Fatalf("url env = %q", got)
	}
}

func TestLoadConfigRejectsUnknownCleanLinkFields(t *testing.T) {
	tests := map[string]string{
		"env object": `
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
environments:
  production:
    servers: [node-a]
    services:
      admin:
        image: nginx:alpine
        port: 3000
      renderer:
        image: nginx:alpine
        env:
          ADMIN_URL:
            link: admin
            extra: nope
`,
		"link object": `
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
        env:
          API_URL:
            link:
              app: backend-api
              stage: production
              service: api
              target: nope
`,
		"share object": `
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
environments:
  production:
    servers: [node-a]
    services:
      api:
        image: nginx:alpine
        port: 3000
        share:
          ports: []
          expose: true
`,
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tako.yaml")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatalf("failed to write config: %v", err)
			}
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("LoadConfig should reject unknown clean link/share fields")
			}
			if !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("error = %q, want unknown field", err)
			}
		})
	}
}

func TestLoadConfigRejectsUnknownJSONCleanLinkField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tako.json")
	if err := os.WriteFile(path, []byte(`{
  "project": {"name": "demo", "version": "1.0.0"},
  "servers": {"node-a": {"host": "10.0.0.1", "user": "deploy"}},
  "environments": {
    "production": {
      "servers": ["node-a"],
      "services": {
        "web": {
          "image": "nginx:alpine",
          "env": {
            "API_URL": {
              "link": {
                "app": "backend-api",
                "stage": "production",
                "service": "api",
                "target": "nope"
              }
            }
          }
        }
      }
    }
  }
}`), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig should reject unknown JSON link fields")
	}
	if !strings.Contains(err.Error(), `unknown field "target"`) {
		t.Fatalf("error = %q, want unknown target field", err)
	}
}

func TestValidateConfigRejectsInvalidExportsAndImports(t *testing.T) {
	tests := map[string]func(*Config){
		"same project import": func(cfg *Config) {
			cfg.Imports = map[string]ImportConfig{
				"self": {Project: cfg.Project.Name, Environment: "production", Service: "api", Port: "web"},
			}
		},
		"missing import server": func(cfg *Config) {
			cfg.Imports = map[string]ImportConfig{
				"app": {Project: "app", Environment: "production", Service: "api", Port: "web", Servers: []string{"missing"}},
			}
		},
		"empty export ports": func(cfg *Config) {
			production := cfg.Environments["production"]
			service := production.Services["web"]
			service.Port = 3000
			service.Export = &ServiceExportConfig{}
			production.Services["web"] = service
			cfg.Environments["production"] = production
		},
		"unknown export target": func(cfg *Config) {
			production := cfg.Environments["production"]
			service := production.Services["web"]
			service.Port = 3000
			service.Export = &ServiceExportConfig{Ports: map[string]int{"web": 9999}}
			production.Services["web"] = service
			cfg.Environments["production"] = production
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := validValidationConfig()
			mutate(cfg)
			if err := ValidateConfig(cfg); err == nil {
				t.Fatal("ValidateConfig should reject invalid export/import config")
			}
		})
	}
}

func TestLoadConfigParsesReplicatedVolume(t *testing.T) {
	t.Setenv("SSH_PASSWORD", "test-password")
	path := filepath.Join(t.TempDir(), "tako.yaml")
	err := os.WriteFile(path, []byte(`
project:
  name: demo
  version: 1.0.0
volumes:
  shared_data:
    replicated: true
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
    password: ${SSH_PASSWORD}
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
        volumes:
          - shared_data:/data
`), 0600)
	if err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	volume, ok := cfg.GetVolume("shared_data")
	if !ok {
		t.Fatal("shared_data volume not loaded")
	}
	if !volume.Replicated {
		t.Fatalf("Replicated = false, want true: %#v", volume)
	}
}

func TestLoadConfigParsesRegistryAuth(t *testing.T) {
	t.Setenv("SSH_PASSWORD", "test-password")
	t.Setenv("REGISTRY_TOKEN", "test-token")
	path := filepath.Join(t.TempDir(), "tako.yaml")
	err := os.WriteFile(path, []byte(`
project:
  name: demo
  version: 1.0.0
registry:
  url: https://ghcr.io
  username: redentor
  password: ${REGISTRY_TOKEN}
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
    password: ${SSH_PASSWORD}
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: ghcr.io/redentor/web:latest
`), 0600)
	if err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	auth := cfg.RegistryAuthForImage("ghcr.io/redentor/web:latest")
	if auth == nil {
		t.Fatal("expected registry auth for ghcr image")
	}
	if auth.Server != "ghcr.io" || auth.Username != "redentor" || auth.Password != "test-token" {
		t.Fatalf("unexpected registry auth: %#v", auth)
	}
	if cfg.RegistryAuthForImage("registry.example.com/redentor/web:latest") != nil {
		t.Fatal("registry auth should not match a different registry host")
	}
}

func TestValidateConfigRejectsIncompleteRegistryAuth(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Registry = &RegistryConfig{
		URL:      "ghcr.io",
		Username: "redentor",
	}

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject incomplete registry auth")
	}
	if !strings.Contains(err.Error(), "registry.password") {
		t.Fatalf("error = %q, want registry password validation", err)
	}
}

func TestValidateConfigRejectsRegistryCacheWithoutRef(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Deployment = &DeploymentConfig{
		Cache: &CacheConfig{
			Enabled: true,
			Type:    "registry",
		},
	}

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject registry cache without ref")
	}
	if !strings.Contains(err.Error(), "deployment.cache.ref") {
		t.Fatalf("error = %q, want cache ref validation", err)
	}
}

func TestValidateConfigNormalizesLocalCacheType(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Deployment = &DeploymentConfig{
		Cache: &CacheConfig{
			Enabled: true,
		},
	}

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	if cfg.Deployment.Cache.Type != "local" {
		t.Fatalf("cache type = %q, want local", cfg.Deployment.Cache.Type)
	}
}

func TestValidateConfigNormalizesDeploymentSource(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Deployment = &DeploymentConfig{}

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	if cfg.Deployment.Source != DeploymentSourceLocal {
		t.Fatalf("deployment source = %q, want local", cfg.Deployment.Source)
	}
}

func TestValidateConfigRejectsInvalidDeploymentSource(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Deployment = &DeploymentConfig{Source: "workspace"}

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject invalid deployment source")
	}
	if !strings.Contains(err.Error(), "deployment.source") {
		t.Fatalf("error = %q, want deployment source validation", err)
	}
}

func TestRegistryAuthForImageMatchesDockerHubAliases(t *testing.T) {
	cfg := &Config{
		Registry: &RegistryConfig{
			URL:      "docker.io",
			Username: "user",
			Password: "token",
		},
	}

	auth := cfg.RegistryAuthForImage("library/alpine:latest")
	if auth == nil {
		t.Fatal("expected docker hub auth for implicit docker hub image")
	}
	if auth.Server != "https://index.docker.io/v1/" {
		t.Fatalf("server = %q, want docker hub config key", auth.Server)
	}
}

func TestValidateConfigAcceptsRollingDeployDefaults(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image: "nginx:alpine",
		Deploy: DeployConfig{
			Strategy: "rolling",
			Monitor:  "30s",
		},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	service := cfg.Environments["production"].Services["web"]
	if service.Deploy.Order != "start-first" {
		t.Fatalf("rolling order = %q, want start-first", service.Deploy.Order)
	}
}

func TestValidateConfigAcceptsBuildPlatform(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image:    "nginx:alpine",
		Platform: "linux/amd64",
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func TestValidateConfigAcceptsCustomDockerfile(t *testing.T) {
	root := t.TempDir()
	mustWriteConfigTestFile(t, filepath.Join(root, "Dockerfile.renderer"), "FROM scratch\n")
	t.Chdir(root)

	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Build:      ".",
		Dockerfile: "./Dockerfile.renderer",
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	service := cfg.Environments["production"].Services["web"]
	if service.Dockerfile != "Dockerfile.renderer" {
		t.Fatalf("dockerfile = %q, want normalized Dockerfile.renderer", service.Dockerfile)
	}
}

func TestValidateConfigRejectsDockerfileWithoutBuild(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image:      "nginx:alpine",
		Dockerfile: "Dockerfile.renderer",
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject dockerfile without build")
	}
	if !strings.Contains(err.Error(), "dockerfile") || !strings.Contains(err.Error(), "requires 'build'") {
		t.Fatalf("error = %q, want dockerfile requires build validation", err)
	}
}

func TestValidateConfigRejectsDockerfileOutsideBuildContext(t *testing.T) {
	root := t.TempDir()
	mustWriteConfigTestFile(t, filepath.Join(root, "Dockerfile.renderer"), "FROM scratch\n")
	mustWriteConfigTestFile(t, filepath.Join(root, "app", "Dockerfile"), "FROM scratch\n")
	t.Chdir(root)

	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Build:      "./app",
		Dockerfile: "../Dockerfile.renderer",
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject dockerfile outside build context")
	}
	if !strings.Contains(err.Error(), "relative path inside the build context") {
		t.Fatalf("error = %q, want build context validation", err)
	}
}

func TestValidateConfigRejectsInvalidBuildPlatform(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image:    "nginx:alpine",
		Platform: "darwin/amd64",
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject invalid platform")
	}
	if !strings.Contains(err.Error(), "invalid platform") {
		t.Fatalf("error = %q, want platform validation", err)
	}
}

func TestValidateConfigRejectsInvalidRollingDeploy(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image: "nginx:alpine",
		Deploy: DeployConfig{
			Strategy:       "rolling",
			Order:          "sideways",
			MaxUnavailable: -1,
		},
	}
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject invalid rolling deploy settings")
	}
	if !strings.Contains(err.Error(), "invalid rolling deploy order") {
		t.Fatalf("error = %q, want rolling order validation", err)
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

func TestValidateConfigAcceptsExplicitPorts(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Services["web"] = ServiceConfig{
		Image: "nginx:alpine",
		Ports: []PortConfig{
			{
				Name:   "http",
				Target: 3000,
				Proxy:  &ProxyConfig{Domain: " app.example.com "},
			},
			{
				Name:     "metrics",
				Target:   9090,
				Internal: true,
			},
			{
				Name:      "dns",
				Target:    5353,
				Published: 15353,
				Protocol:  "udp",
				Mode:      "host",
				HostIP:    "10.0.0.0/8",
			},
		},
	}
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	ports := cfg.Environments["production"].Services["web"].Ports
	if ports[0].Mode != "proxy" || ports[0].Protocol != "http" || ports[0].Proxy.Domain != "app.example.com" {
		t.Fatalf("proxy port defaults = %#v, want normalized HTTP proxy", ports[0])
	}
	if ports[1].Mode != "internal" || ports[1].Protocol != "tcp" {
		t.Fatalf("internal port defaults = %#v, want internal tcp", ports[1])
	}
	if ports[2].Published != 15353 || ports[2].Protocol != "udp" || ports[2].HostIP != "10.0.0.0/8" {
		t.Fatalf("host port = %#v, want UDP host bind", ports[2])
	}
}

func TestValidateConfigRejectsInvalidExplicitPorts(t *testing.T) {
	tests := map[string]struct {
		service ServiceConfig
		want    string
	}{
		"legacyPortAndPorts": {
			service: ServiceConfig{Image: "nginx:alpine", Port: 3000, Ports: []PortConfig{{Name: "http", Target: 3000}}},
			want:    "cannot specify both port and ports",
		},
		"topLevelProxyWithPorts": {
			service: ServiceConfig{Image: "nginx:alpine", Proxy: &ProxyConfig{Domain: "example.com"}, Ports: []PortConfig{{Name: "http", Target: 3000}}},
			want:    "top-level proxy cannot be combined with ports",
		},
		"duplicateNames": {
			service: ServiceConfig{Image: "nginx:alpine", Ports: []PortConfig{{Name: "http", Target: 3000}, {Name: "http", Target: 3001}}},
			want:    "duplicate port name",
		},
		"hostProxyConflict": {
			service: ServiceConfig{Image: "nginx:alpine", Ports: []PortConfig{{Name: "admin", Target: 3000, Mode: "host", Proxy: &ProxyConfig{Domain: "admin.example.com"}}}},
			want:    "host port admin cannot define proxy",
		},
		"invalidCIDR": {
			service: ServiceConfig{Image: "nginx:alpine", Ports: []PortConfig{{Name: "admin", Target: 3000, Mode: "host", HostIP: "not cidr"}}},
			want:    "hostIP must be an IP address or CIDR",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := validValidationConfig()
			production := cfg.Environments["production"]
			production.Services["web"] = tt.service
			cfg.Environments["production"] = production

			err := ValidateConfig(cfg)
			if err == nil {
				t.Fatal("ValidateConfig should reject invalid ports")
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

func mustWriteConfigTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create parent directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
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
