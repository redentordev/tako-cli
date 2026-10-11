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

func TestValidateProxyTrustedProxies(t *testing.T) {
	cfg := proxyAccessValidationConfig(func(p *ProxyConfig) {
		p.TrustedProxies = []string{" 2001:db8::1/32 ", "203.0.113.99/24", "203.0.113.0/24"}
	})
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	got := cfg.Environments["production"].Services["web"].Proxy.TrustedProxies
	want := []string{"2001:db8::/32", "203.0.113.0/24"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("trustedProxies = %v, want canonical sorted %v", got, want)
	}

	for _, bad := range []string{"", "203.0.113.1", "0.0.0.0/0", "10.0.0.0/7", "::/0", "2001:db8::/23"} {
		cfg := proxyAccessValidationConfig(func(p *ProxyConfig) {
			p.TrustedProxies = []string{bad}
		})
		if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "trustedProxies") {
			t.Fatalf("trustedProxies entry %q: error = %v, want trustedProxies error", bad, err)
		}
	}
}

func TestValidateProxyRoutePolicy(t *testing.T) {
	cfg := proxyAccessValidationConfig(func(p *ProxyConfig) {
		p.DenyPathPrefixes = []string{"/.well-known/workflow/", "/api/internal"}
		p.AllowPathPrefixes = []string{"/api", "/health"}
		p.MaxRequestBody = "10mb"
		p.ResponseHeaders = map[string]string{
			"content-security-policy": "default-src 'self'",
			"x-content-type-options":  "nosniff",
		}
	})
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	proxy := cfg.Environments["production"].Services["web"].Proxy
	if got := strings.Join(proxy.DenyPathPrefixes, ","); got != "/.well-known/workflow,/api/internal" {
		t.Fatalf("denyPathPrefixes = %q", got)
	}
	if got := strings.Join(proxy.AllowPathPrefixes, ","); got != "/api,/health" {
		t.Fatalf("allowPathPrefixes = %q", got)
	}
	if proxy.MaxRequestBody != "10MB" {
		t.Fatalf("maxRequestBody = %q, want 10MB", proxy.MaxRequestBody)
	}
	if proxy.ResponseHeaders["Content-Security-Policy"] != "default-src 'self'" || proxy.ResponseHeaders["X-Content-Type-Options"] != "nosniff" {
		t.Fatalf("responseHeaders were not canonicalized: %#v", proxy.ResponseHeaders)
	}
}

func TestValidateProxyRoutePolicyRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ProxyConfig)
		wantErr string
	}{
		{"relative denied path", func(p *ProxyConfig) { p.DenyPathPrefixes = []string{"api/internal"} }, "denyPathPrefixes"},
		{"wildcard denied path", func(p *ProxyConfig) { p.DenyPathPrefixes = []string{"/api/*"} }, "denyPathPrefixes"},
		{"spaced denied path", func(p *ProxyConfig) { p.DenyPathPrefixes = []string{"/api/internal respond"} }, "denyPathPrefixes"},
		{"encoded denied path", func(p *ProxyConfig) { p.DenyPathPrefixes = []string{"/api/%69nternal"} }, "denyPathPrefixes"},
		{"padded denied path", func(p *ProxyConfig) { p.DenyPathPrefixes = []string{" /api/internal"} }, "whitespace"},
		{"duplicate denied path", func(p *ProxyConfig) { p.DenyPathPrefixes = []string{"/api/internal", "/api/internal/"} }, "duplicate"},
		{"dot allowed path", func(p *ProxyConfig) { p.AllowPathPrefixes = []string{"/api/.."} }, "allowPathPrefixes"},
		{"relative allowed path", func(p *ProxyConfig) { p.AllowPathPrefixes = []string{"api"} }, "allowPathPrefixes"},
		{"wildcard allowed path", func(p *ProxyConfig) { p.AllowPathPrefixes = []string{"/api/*"} }, "allowPathPrefixes"},
		{"encoded allowed path", func(p *ProxyConfig) { p.AllowPathPrefixes = []string{"/api/%2fadmin"} }, "allowPathPrefixes"},
		{"noncanonical allowed path", func(p *ProxyConfig) { p.AllowPathPrefixes = []string{"/api/"} }, "canonical"},
		{"invalid body size", func(p *ProxyConfig) { p.MaxRequestBody = "10MiB" }, "maxRequestBody"},
		{"zero body size", func(p *ProxyConfig) { p.MaxRequestBody = "0MB" }, "maxRequestBody"},
		{"oversized body limit", func(p *ProxyConfig) { p.MaxRequestBody = "1025GB" }, "1TB"},
		{"invalid header name", func(p *ProxyConfig) { p.ResponseHeaders = map[string]string{"Bad Header": "value"} }, "responseHeaders"},
		{"header injection", func(p *ProxyConfig) { p.ResponseHeaders = map[string]string{"X-Test": "ok\r\nInjected: yes"} }, "responseHeaders"},
		{"framing header", func(p *ProxyConfig) { p.ResponseHeaders = map[string]string{"Content-Length": "1"} }, "framing"},
		{"hop-by-hop header", func(p *ProxyConfig) { p.ResponseHeaders = map[string]string{"Keep-Alive": "timeout=5"} }, "hop-by-hop"},
		{"duplicate header", func(p *ProxyConfig) { p.ResponseHeaders = map[string]string{"X-Test": "one", "x-test": "two"} }, "duplicate"},
		{"padded header value", func(p *ProxyConfig) { p.ResponseHeaders = map[string]string{"X-Test": " value "} }, "whitespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := proxyAccessValidationConfig(tt.mutate)
			err := ValidateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateProxyRequestHeadersAllowedOnPublicAndInternal(t *testing.T) {
	for _, internal := range []bool{false, true} {
		cfg := proxyAccessValidationConfig(func(p *ProxyConfig) {
			if internal {
				p.Domain = ""
				p.Visibility = ProxyVisibilityInternal
			}
			p.RequestHeaders = map[string]string{"x-forwarded-host": "www.example.com", "X-Tenant": "acme corp"}
		})
		if err := ValidateConfig(cfg); err != nil {
			t.Fatalf("internal=%v: ValidateConfig returned error: %v", internal, err)
		}
		got := cfg.Environments["production"].Services["web"].Proxy.RequestHeaders
		if got["X-Forwarded-Host"] != "www.example.com" || got["X-Tenant"] != "acme corp" || len(got) != 2 {
			t.Fatalf("internal=%v: requestHeaders not canonicalized: %#v", internal, got)
		}
	}
}

func TestValidateProxyRequestHeadersRejectsUnsafeEntries(t *testing.T) {
	tooMany := make(map[string]string, 33)
	for i := 0; i < 33; i++ {
		tooMany["X-Header-"+strings.Repeat("a", i+1)] = "v"
	}
	tests := []struct {
		name    string
		headers map[string]string
		wantErr string
	}{
		{"host", map[string]string{"Host": "example.com"}, "framing"},
		{"host lowercase", map[string]string{"host": "example.com"}, "framing"},
		{"content-length", map[string]string{"Content-Length": "1"}, "framing"},
		{"http2-settings", map[string]string{"http2-settings": "AAMAAABkAAQAAP__"}, "hop-by-hop"},
		{"upgrade", map[string]string{"UPGRADE": "websocket"}, "hop-by-hop"},
		{"te", map[string]string{"te": "trailers"}, "hop-by-hop"},
		{"x-forwarded-for", map[string]string{"x-forwarded-for": "1.2.3.4"}, "client-identity headers are managed by the proxy"},
		{"forwarded", map[string]string{"FORWARDED": "for=1.2.3.4"}, "client-identity"},
		{"x-real-ip", map[string]string{"X-Real-IP": "1.2.3.4"}, "client-identity"},
		{"placeholder", map[string]string{"X-Forwarded-Host": "{http.request.host}"}, "values must be literal"},
		{"closing brace", map[string]string{"X-Test": "a}"}, "literal"},
		{"quote", map[string]string{"X-Test": `a"b`}, "requestHeaders value"},
		{"backslash", map[string]string{"X-Test": `a\b`}, "requestHeaders value"},
		{"newline", map[string]string{"X-Test": "a\nb"}, "requestHeaders value"},
		{"tab", map[string]string{"X-Test": "a\tb"}, "requestHeaders value"},
		{"non-ascii", map[string]string{"X-Test": "café"}, "requestHeaders value"},
		{"empty value", map[string]string{"X-Test": ""}, "requestHeaders value"},
		{"long value", map[string]string{"X-Test": strings.Repeat("a", 1025)}, "requestHeaders value"},
		{"padded value", map[string]string{"X-Test": " a"}, "whitespace"},
		{"bad name", map[string]string{"Bad Header": "v"}, "requestHeaders name"},
		{"long name", map[string]string{strings.Repeat("a", 129): "v"}, "requestHeaders name"},
		{"duplicate", map[string]string{"X-Test": "one", "x-test": "two"}, "duplicate"},
		{"too many", tooMany, "at most 32"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := proxyAccessValidationConfig(func(p *ProxyConfig) { p.RequestHeaders = tt.headers })
			err := ValidateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
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
