package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const acmeDNSTestConfig = `project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 192.0.2.10
    user: root
    password: test-password
environments:
  production:
    servers: [node-a]
    proxy:
      acme:
        dnsProvider: cloudflare
        credentials:
          apiToken: %s
    services:
      web:
        image: nginx:alpine
        port: 80
        proxy:
          domain: "%s"
          tls:
            challenge: %s
`

func writeACMEDNSTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tako.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigRequiresEnvReferencesForACMECredentials(t *testing.T) {
	content := strings.Replace(acmeDNSTestConfig, "%s", "literal-secret", 1)
	content = strings.Replace(content, "%s", "example.com", 1)
	content = strings.Replace(content, "%s", "dns", 1)
	path := writeACMEDNSTestConfig(t, content)
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "must be an environment variable reference") {
		t.Fatalf("LoadConfig error = %v", err)
	}
}

func TestLoadConfigExpandsAndValidatesACMEDNS(t *testing.T) {
	t.Setenv("CF_ZONE_TOKEN", "super-secret-zone-token")
	content := strings.Replace(acmeDNSTestConfig, "%s", "${CF_ZONE_TOKEN}", 1)
	content = strings.Replace(content, "%s", "*.example.com", 1)
	content = strings.Replace(content, "%s", "auto", 1)
	cfg, err := LoadConfig(writeACMEDNSTestConfig(t, content))
	if err != nil {
		t.Fatalf("LoadConfig error = %v", err)
	}
	acme := cfg.EnvironmentACME("production")
	if acme == nil || acme.DNSProvider != ACMEDNSProviderCloudflare || acme.Credentials["apiToken"] != "super-secret-zone-token" {
		t.Fatalf("acme = %#v", acme)
	}
	if got := cfg.Environments["production"].Services["web"].Proxy.TLS.Challenge; got != ProxyTLSChallengeDNS {
		t.Fatalf("wildcard challenge = %q, want dns", got)
	}
}

func TestValidateConfigRejectsWildcardOrDNSChallengeWithoutProvider(t *testing.T) {
	for _, tc := range []struct {
		name      string
		domain    string
		challenge string
	}{
		{name: "wildcard", domain: "*.example.com"},
		{name: "explicit dns", domain: "app.example.com", challenge: "dns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalACMEValidationConfig(tc.domain, tc.challenge)
			if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "proxy.acme") {
				t.Fatalf("ValidateConfig error = %v", err)
			}
		})
	}
}

func TestValidateConfigAllowsDynamicDomainsWithDNSChallenge(t *testing.T) {
	cfg := minimalACMEValidationConfig("app.example.com", "dns")
	env := cfg.Environments["production"]
	env.Proxy = &EnvironmentProxyConfig{ACME: &EnvironmentACMEConfig{DNSProvider: "digitalocean", Credentials: map[string]string{"apiToken": "token"}}}
	service := env.Services["web"]
	service.Proxy.DynamicDomains = &DynamicDomainsConfig{Ask: "web:/allow"}
	env.Services["web"] = service
	cfg.Environments["production"] = env
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig error = %v", err)
	}
}

func TestValidateConfigRequiresEmailForZeroSSLDNSChallenge(t *testing.T) {
	cfg := minimalACMEValidationConfig("app.example.com", "dns")
	env := cfg.Environments["production"]
	env.Proxy = &EnvironmentProxyConfig{ACME: &EnvironmentACMEConfig{DNSProvider: "hetzner", Credentials: map[string]string{"apiToken": "token"}}}
	service := env.Services["web"]
	service.Proxy.TLS.Provider = "zerossl"
	env.Services["web"] = service
	cfg.Environments["production"] = env
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "proxy.email is required") {
		t.Fatalf("ValidateConfig error = %v", err)
	}
	service.Proxy.Email = "ops@example.com"
	env.Services["web"] = service
	cfg.Environments["production"] = env
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig with email error = %v", err)
	}
}

func minimalACMEValidationConfig(domain, challenge string) *Config {
	return &Config{
		Project: ProjectConfig{Name: "demo", Version: "1.0.0"},
		Servers: map[string]ServerConfig{"node-a": {Host: "192.0.2.10", User: "root", Password: "test-password"}},
		Environments: map[string]EnvironmentConfig{
			"production": {
				Servers: []string{"node-a"},
				Services: map[string]ServiceConfig{
					"web": {Image: "nginx:alpine", Port: 80, Proxy: &ProxyConfig{Domain: domain, TLS: TLSConfig{Challenge: challenge}}},
				},
			},
		},
	}
}
