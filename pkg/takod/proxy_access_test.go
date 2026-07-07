package takod

import (
	"strings"
	"testing"
)

// takodTestBcryptHash is bcrypt("s3cret", cost 10) — a fixed fixture, not a secret.
const takodTestBcryptHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

func accessControlManifest(route ProxyRoute) []ProxyRouteManifest {
	if route.Service == "" {
		route.Service = "web"
	}
	if len(route.Domains) == 0 {
		route.Domains = []string{"example.com"}
	}
	if len(route.Upstreams) == 0 {
		route.Upstreams = []string{"http://demo-web:3000"}
	}
	return []ProxyRouteManifest{{
		Version:     1,
		Project:     "demo",
		Environment: "production",
		Routes:      []ProxyRoute{route},
	}}
}

func TestRenderCaddyfileEmitsBasicAuth(t *testing.T) {
	caddyfile, err := renderCaddyfile(accessControlManifest(ProxyRoute{
		BasicAuth: &ProxyRouteBasicAuth{Username: "admin", PasswordBcrypt: takodTestBcryptHash},
	}))
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	if !strings.Contains(caddyfile, "\tbasic_auth {\n\t\tadmin "+takodTestBcryptHash+"\n\t}\n") {
		t.Fatalf("missing basic_auth block:\n%s", caddyfile)
	}
	if !strings.Contains(caddyfile, "reverse_proxy http://demo-web:3000") {
		t.Fatalf("missing upstream:\n%s", caddyfile)
	}
}

func TestRenderCaddyfileEmitsAllowIPGuards(t *testing.T) {
	caddyfile, err := renderCaddyfile(accessControlManifest(ProxyRoute{
		AllowIPs:  []string{"203.0.113.10", "10.0.0.0/8"},
		BasicAuth: &ProxyRouteBasicAuth{Username: "admin", PasswordBcrypt: takodTestBcryptHash},
	}))
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	if !strings.Contains(caddyfile, "@tako_allowed remote_ip 203.0.113.10 10.0.0.0/8\n") {
		t.Fatalf("missing remote_ip matcher:\n%s", caddyfile)
	}
	if !strings.Contains(caddyfile, "\thandle @tako_allowed {\n") {
		t.Fatalf("missing allowed handle block:\n%s", caddyfile)
	}
	if !strings.Contains(caddyfile, "\thandle {\n\t\trespond 403\n\t}\n") {
		t.Fatalf("missing deny handle block:\n%s", caddyfile)
	}
	// Both directives must live inside the allowed handle block so the
	// allowlist is decided before basic_auth challenges the client.
	allowed := caddyfile[strings.Index(caddyfile, "handle @tako_allowed"):]
	allowed = allowed[:strings.Index(allowed, "\thandle {")]
	if !strings.Contains(allowed, "\t\tbasic_auth {") || !strings.Contains(allowed, "\t\treverse_proxy ") {
		t.Fatalf("basic_auth/reverse_proxy not nested in allowed handle:\n%s", caddyfile)
	}
}

func TestValidateProxyRouteManifestRejectsUnsafeAccessControls(t *testing.T) {
	cases := []struct {
		name    string
		route   ProxyRoute
		wantErr string
	}{
		{"unsafe username", ProxyRoute{BasicAuth: &ProxyRouteBasicAuth{Username: "admin\"}", PasswordBcrypt: takodTestBcryptHash}}, "invalid basic auth username"},
		{"plaintext password", ProxyRoute{BasicAuth: &ProxyRouteBasicAuth{Username: "admin", PasswordBcrypt: "hunter2"}}, "must be a bcrypt hash"},
		{"unsafe allow ip", ProxyRoute{AllowIPs: []string{"1.2.3.4 {"}}, "invalid allow IP"},
		{"allow ip injection", ProxyRoute{AllowIPs: []string{"1.2.3.4\nrespond 200"}}, "invalid allow IP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := accessControlManifest(tc.route)[0]
			err := validateProxyRouteManifest(&manifest)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}
