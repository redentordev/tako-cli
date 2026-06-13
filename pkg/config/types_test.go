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
