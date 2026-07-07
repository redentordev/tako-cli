package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const registriesTestConfigTemplate = `project:
  name: demo
  version: 1.0.0
registries:
  ghcr.io:
    username: octocat
    password: %s
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
    password: sshpass
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: ghcr.io/acme/web:v1
        port: 3000
`

func writeRegistriesTestConfig(t *testing.T, password string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tako.yaml")
	content := strings.Replace(registriesTestConfigTemplate, "%s", password, 1)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigAcceptsEnvRefRegistryPassword(t *testing.T) {
	t.Setenv("TAKO_TEST_REGISTRY_TOKEN", "gh-token-value")
	path := writeRegistriesTestConfig(t, "${TAKO_TEST_REGISTRY_TOKEN}")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	registry, ok := cfg.Registries["ghcr.io"]
	if !ok {
		t.Fatalf("registries = %v, want ghcr.io", cfg.Registries)
	}
	if registry.Username != "octocat" || registry.Password != "gh-token-value" {
		t.Fatalf("registry = %+v, want expanded credentials", registry)
	}
}

func TestLoadConfigRejectsLiteralRegistryPassword(t *testing.T) {
	path := writeRegistriesTestConfig(t, "literal-secret-token")
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig accepted a literal registry password")
	}
	if !strings.Contains(err.Error(), "environment variable reference") {
		t.Fatalf("error = %q, want env-ref requirement", err)
	}
}

func TestLoadConfigRejectsPartialEnvRefRegistryPassword(t *testing.T) {
	t.Setenv("TAKO_TEST_REGISTRY_TOKEN", "gh-token-value")
	path := writeRegistriesTestConfig(t, "prefix-${TAKO_TEST_REGISTRY_TOKEN}")
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig accepted a partially-literal registry password")
	}
}

func TestValidateRegistriesRejectsBadEntries(t *testing.T) {
	cases := []struct {
		name       string
		registries map[string]RegistryConfig
		want       string
	}{
		{"empty host", map[string]RegistryConfig{" ": {Username: "u", Password: "p"}}, "registry host cannot be empty"},
		{"host with space", map[string]RegistryConfig{"ghcr io": {Username: "u", Password: "p"}}, "invalid registry host"},
		{"missing username", map[string]RegistryConfig{"ghcr.io": {Password: "p"}}, "username is required"},
		{"missing password", map[string]RegistryConfig{"ghcr.io": {Username: "u"}}, "password is required"},
	}
	for _, tc := range cases {
		err := validateRegistries(tc.registries)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
	if err := validateRegistries(map[string]RegistryConfig{"ghcr.io": {Username: "u", Password: "p"}}); err != nil {
		t.Fatalf("valid registries rejected: %v", err)
	}
}
