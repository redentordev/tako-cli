package config

import (
	"strings"
	"testing"
)

// testBcryptHash is bcrypt("s3cret", cost 10) — a fixed fixture, not a secret.
const testBcryptHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

func proxyAccessValidationConfig(mutate func(*ProxyConfig)) *Config {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Proxy = &EnvironmentProxyConfig{
		Placement: &PlacementConfig{Strategy: "pinned", Servers: []string{"node-a"}},
	}
	web := production.Services["web"]
	web.Proxy = &ProxyConfig{Domain: "example.com"}
	if mutate != nil {
		mutate(web.Proxy)
	}
	production.Services["web"] = web
	cfg.Environments["production"] = production
	return cfg
}

func TestValidateProxyBasicAuth(t *testing.T) {
	cfg := proxyAccessValidationConfig(func(p *ProxyConfig) {
		p.BasicAuth = &ProxyBasicAuthConfig{Username: " admin ", PasswordBcrypt: " " + testBcryptHash + " "}
	})
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	auth := cfg.Environments["production"].Services["web"].Proxy.BasicAuth
	if auth.Username != "admin" || auth.PasswordBcrypt != testBcryptHash {
		t.Fatalf("basic auth not trimmed: %+v", auth)
	}

	cases := []struct {
		name    string
		auth    ProxyBasicAuthConfig
		wantErr string
	}{
		{"missing username", ProxyBasicAuthConfig{PasswordBcrypt: testBcryptHash}, "username is required"},
		{"unsafe username", ProxyBasicAuthConfig{Username: "admin{oops}", PasswordBcrypt: testBcryptHash}, "invalid proxy.basicAuth.username"},
		{"missing hash", ProxyBasicAuthConfig{Username: "admin"}, "passwordBcrypt is required"},
		{"plaintext password", ProxyBasicAuthConfig{Username: "admin", PasswordBcrypt: "hunter2"}, "tako proxy hash-password"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := tc.auth
			cfg := proxyAccessValidationConfig(func(p *ProxyConfig) { p.BasicAuth = &auth })
			err := ValidateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateProxyBasicAuthAppliesToInternalRoutes(t *testing.T) {
	cfg := proxyAccessValidationConfig(func(p *ProxyConfig) {
		p.Domain = ""
		p.Visibility = ProxyVisibilityInternal
		p.BasicAuth = &ProxyBasicAuthConfig{Username: "admin", PasswordBcrypt: "plaintext"}
	})
	err := ValidateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "bcrypt") {
		t.Fatalf("internal route basic auth not validated: %v", err)
	}
}

func TestValidateProxyAllowIps(t *testing.T) {
	cfg := proxyAccessValidationConfig(func(p *ProxyConfig) {
		p.AllowIps = []string{" 203.0.113.10 ", "10.0.0.0/8", "2001:db8::1"}
	})
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	got := cfg.Environments["production"].Services["web"].Proxy.AllowIps
	if got[0] != "203.0.113.10" {
		t.Fatalf("allowIps entry not trimmed: %v", got)
	}

	for _, bad := range []string{"", "example.com", "10.0.0.0/64", "1.2.3.4; rm -rf /"} {
		cfg := proxyAccessValidationConfig(func(p *ProxyConfig) {
			p.AllowIps = []string{bad}
		})
		if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "allowIps") {
			t.Fatalf("allowIps entry %q: error = %v, want allowIps error", bad, err)
		}
	}
}

func TestValidateResourcesCPUs(t *testing.T) {
	for _, good := range []string{"0.5", "2", "1.25"} {
		cfg := proxyAccessValidationConfig(nil)
		production := cfg.Environments["production"]
		web := production.Services["web"]
		web.Resources = &ResourceLimitsConfig{CPUs: good}
		production.Services["web"] = web
		cfg.Environments["production"] = production
		if err := ValidateConfig(cfg); err != nil {
			t.Fatalf("cpus %q rejected: %v", good, err)
		}
	}
	for _, bad := range []string{"abc", "0", "0.0", "-1", "1e3", "2 cores"} {
		cfg := proxyAccessValidationConfig(nil)
		production := cfg.Environments["production"]
		web := production.Services["web"]
		web.Resources = &ResourceLimitsConfig{CPUs: bad}
		production.Services["web"] = web
		cfg.Environments["production"] = production
		if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "resources.cpus") {
			t.Fatalf("cpus %q: error = %v, want resources.cpus error", bad, err)
		}
	}
}
