package config

import "testing"

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

	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("ValidateConfig should reject an NFS server outside the environment")
	}
}

func TestValidateConfigAcceptsEnvironmentNFSServer(t *testing.T) {
	cfg := validNFSValidationConfig("node-b")

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func nfsServerNameTestConfig(server string) *Config {
	return &Config{
		Storage: &StorageConfig{
			NFS: &NFSConfig{
				Enabled: true,
				Server:  server,
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
