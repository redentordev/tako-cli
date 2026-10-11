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

func TestValidateRegistriesProviderRules(t *testing.T) {
	valid := map[string]RegistryConfig{
		"123456789012.dkr.ecr.us-west-1.amazonaws.com":          {Provider: "ecr"},
		"123456789012.dkr.ecr-fips.us-gov-west-1.amazonaws.com": {Provider: "ecr", Profile: "prod"},
		"123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn":      {Provider: "ecr", Region: "cn-north-1"},
		"gcr.io":                      {Provider: "gcr"},
		"eu.gcr.io":                   {Provider: "gcr", Account: "deploy@example.iam.gserviceaccount.com"},
		"us-docker.pkg.dev":           {Provider: "gcr"},
		"europe-west1-docker.pkg.dev": {Provider: "artifact-registry"},
		"ghcr.io":                     {Provider: "ghcr", Username: "octocat"},
	}
	for host, registry := range valid {
		if err := validateRegistries(map[string]RegistryConfig{host: registry}); err != nil {
			t.Fatalf("%s rejected: %v", host, err)
		}
	}
	cases := []struct {
		name     string
		host     string
		registry RegistryConfig
		want     string
	}{
		{"mismatched host", "ghcr.io", RegistryConfig{Provider: "ecr"}, "provider ecr requires an ECR registry host"},
		{"gcr on ecr host", "123456789012.dkr.ecr.us-west-1.amazonaws.com", RegistryConfig{Provider: "gcr"}, "provider gcr requires"},
		{"ghcr on other host", "registry.example.com", RegistryConfig{Provider: "ghcr"}, "provider ghcr requires"},
		{"password plus provider", "123456789012.dkr.ecr.us-west-1.amazonaws.com", RegistryConfig{Provider: "ecr", Password: "p"}, "provider and password are mutually exclusive"},
		{"username with ecr", "123456789012.dkr.ecr.us-west-1.amazonaws.com", RegistryConfig{Provider: "ecr", Username: "AWS"}, "username is only valid with provider ghcr"},
		{"conflicting region", "123456789012.dkr.ecr.eu-central-1.amazonaws.com", RegistryConfig{Provider: "ecr", Region: "us-east-1"}, "conflicts with region \"eu-central-1\""},
		{"unknown provider", "quay.io", RegistryConfig{Provider: "quay"}, "supported: ecr, gcr, artifact-registry, ghcr"},
		{"account with ecr", "123456789012.dkr.ecr.us-west-1.amazonaws.com", RegistryConfig{Provider: "ecr", Account: "a"}, "account is only valid"},
		{"profile with gcr", "gcr.io", RegistryConfig{Provider: "gcr", Profile: "p"}, "region and profile are only valid"},
		{"option looks like flag", "123456789012.dkr.ecr.us-west-1.amazonaws.com", RegistryConfig{Provider: "ecr", Profile: "--debug"}, "must not contain control characters or start with '-'"},
		{"provider keys without provider", "ghcr.io", RegistryConfig{Username: "u", Password: "p", Region: "us-east-1"}, "only valid with provider"},
		{"bad account digits", "12345.dkr.ecr.us-west-1.amazonaws.com", RegistryConfig{Provider: "ecr"}, "requires an ECR registry host"},
	}
	for _, tc := range cases {
		err := validateRegistries(map[string]RegistryConfig{tc.host: tc.registry})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestECRRegionFromHost(t *testing.T) {
	cases := map[string]string{
		"123456789012.dkr.ecr.eu-central-1.amazonaws.com":       "eu-central-1",
		"123456789012.dkr.ecr-fips.us-gov-west-1.amazonaws.com": "us-gov-west-1",
		"123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn":      "cn-north-1",
	}
	for host, want := range cases {
		if got, ok := ECRRegionFromHost(host); !ok || got != want {
			t.Fatalf("ECRRegionFromHost(%q) = %q, %v; want %q", host, got, ok, want)
		}
	}
	if _, ok := ECRRegionFromHost("public.ecr.aws"); ok {
		t.Fatal("public.ecr.aws parsed as a private ECR host")
	}
}

func TestImageRegistryHost(t *testing.T) {
	cases := map[string]string{
		"nginx:1.27":                      "docker.io",
		"library/nginx":                   "docker.io",
		"acme/web:v1":                     "docker.io",
		"ghcr.io/acme/web:v1":             "ghcr.io",
		"registry.example.com:5000/a/b:1": "registry.example.com:5000",
		"localhost/app:dev":               "localhost",
		"localhost:5000/app":              "localhost:5000",
		"123456789012.dkr.ecr.us-west-1.amazonaws.com/web@sha256:abc": "123456789012.dkr.ecr.us-west-1.amazonaws.com",
	}
	for image, want := range cases {
		if got := ImageRegistryHost(image); got != want {
			t.Fatalf("ImageRegistryHost(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestRegistryConfigDecodesProviderFieldsFromYAMLAndJSON(t *testing.T) {
	yamlConfig := `project:
  name: demo
  version: 1.0.0
registries:
  123456789012.dkr.ecr.us-west-1.amazonaws.com:
    provider: ecr
    profile: prod
  europe-docker.pkg.dev:
    provider: artifact-registry
    account: deploy@example.com
  ghcr.io:
    provider: ghcr
    username: octocat
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
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "tako.yaml")
	if err := os.WriteFile(yamlPath, []byte(yamlConfig), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(yamlPath)
	if err != nil {
		t.Fatalf("LoadConfig yaml: %v", err)
	}
	ecr := cfg.Registries["123456789012.dkr.ecr.us-west-1.amazonaws.com"]
	if ecr.Provider != "ecr" || ecr.Profile != "prod" {
		t.Fatalf("ecr entry = %+v", ecr)
	}
	if gar := cfg.Registries["europe-docker.pkg.dev"]; gar.Provider != "artifact-registry" || gar.Account != "deploy@example.com" {
		t.Fatalf("artifact registry entry = %+v", gar)
	}
	if gh := cfg.Registries["ghcr.io"]; gh.Provider != "ghcr" || gh.Username != "octocat" {
		t.Fatalf("ghcr entry = %+v", gh)
	}

	jsonConfig := `{"project":{"name":"demo","version":"1.0.0"},
"registries":{"123456789012.dkr.ecr.us-west-1.amazonaws.com":{"provider":"ecr","region":"us-west-1"}},
"servers":{"node-a":{"host":"10.0.0.1","user":"deploy","password":"sshpass"}},
"environments":{"production":{"servers":["node-a"],"services":{"web":{"image":"nginx:1.27","port":3000}}}}}`
	jsonPath := filepath.Join(dir, "tako.json")
	if err := os.WriteFile(jsonPath, []byte(jsonConfig), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(jsonPath)
	if err != nil {
		t.Fatalf("LoadConfig json: %v", err)
	}
	if entry := cfg.Registries["123456789012.dkr.ecr.us-west-1.amazonaws.com"]; entry.Provider != "ecr" || entry.Region != "us-west-1" {
		t.Fatalf("json ecr entry = %+v", entry)
	}
}
