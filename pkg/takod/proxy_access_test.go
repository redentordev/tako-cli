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
	if !strings.Contains(caddyfile, "\thandle {\n\t\theader Connection close\n\t\trespond 403\n\t}\n") {
		t.Fatalf("missing deny handle block:\n%s", caddyfile)
	}
	if !strings.Contains(caddyfile, "\thandle_errors {\n\t\theader Connection close\n\t\trespond \"\" {http.error.status_code}\n\t}\n") {
		t.Fatalf("missing authentication error close handler:\n%s", caddyfile)
	}
	// Both directives must live inside the allowed handle block so the
	// allowlist is decided before basic_auth challenges the client.
	allowed := caddyfile[strings.Index(caddyfile, "handle @tako_allowed"):]
	allowed = allowed[:strings.Index(allowed, "\thandle {")]
	if !strings.Contains(allowed, "\t\tbasic_auth {") || !strings.Contains(allowed, "\t\treverse_proxy ") {
		t.Fatalf("basic_auth/reverse_proxy not nested in allowed handle:\n%s", caddyfile)
	}
}

func TestRenderCaddyfileTrustedProxiesAreGlobalButAllowIPOptInIsRouteLocal(t *testing.T) {
	manifests := []ProxyRouteManifest{{
		Version: 1, Project: "demo", Environment: "production",
		Routes: []ProxyRoute{
			{
				Service: "cdn", Domains: []string{"cdn.example.com"}, Upstreams: []string{"http://cdn:3000"},
				AllowIPs: []string{"198.51.100.0/24"}, TrustedProxies: []string{"2001:db8::/32", "203.0.113.0/24"},
			},
			{
				Service: "direct", Domains: []string{"direct.example.com"}, Upstreams: []string{"http://direct:3000"},
				AllowIPs: []string{"198.51.100.0/24"},
			},
		},
	}, {
		Version: 1, Project: "other", Environment: "production",
		Routes: []ProxyRoute{{
			Service: "other", Domains: []string{"other.example.com"}, Upstreams: []string{"http://other:3000"},
			TrustedProxies: []string{"203.0.113.0/24", "2001:db8::/32"},
		}},
	}}
	caddyfile, err := renderCaddyfile(manifests)
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	if !strings.Contains(caddyfile, "\tservers {\n\t\ttrusted_proxies static 2001:db8::/32 203.0.113.0/24\n\t\ttrusted_proxies_strict\n\t}\n") {
		t.Fatalf("missing sorted node trusted proxy set:\n%s", caddyfile)
	}
	cdn := caddyfile[strings.Index(caddyfile, "cdn.example.com {"):]
	cdn = cdn[:strings.Index(cdn, "\n}")+2]
	if !strings.Contains(cdn, "@tako_allowed client_ip 198.51.100.0/24") {
		t.Fatalf("CDN route did not use client_ip:\n%s", cdn)
	}
	direct := caddyfile[strings.Index(caddyfile, "direct.example.com {"):]
	direct = direct[:strings.Index(direct, "\n}")+2]
	if !strings.Contains(direct, "@tako_allowed remote_ip 198.51.100.0/24") {
		t.Fatalf("direct route did not retain remote_ip:\n%s", direct)
	}
}

func TestRenderCaddyfileRejectsConflictingTrustedProxySets(t *testing.T) {
	_, err := renderCaddyfile([]ProxyRouteManifest{{
		Version: 1, Project: "demo", Environment: "production",
		Routes: []ProxyRoute{
			{Service: "cdn-a", Domains: []string{"a.example.com"}, Upstreams: []string{"http://a:3000"}, TrustedProxies: []string{"203.0.113.0/24"}},
			{Service: "cdn-b", Domains: []string{"b.example.com"}, Upstreams: []string{"http://b:3000"}, TrustedProxies: []string{"198.51.100.0/24"}},
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "conflicting trusted proxy CIDR sets") {
		t.Fatalf("error = %v, want conflicting trusted proxy sets", err)
	}
}

func TestRenderCaddyfileWithoutTrustedProxiesIsUnchanged(t *testing.T) {
	got, err := renderCaddyfile(accessControlManifest(ProxyRoute{}))
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	if strings.Contains(got, "trusted_proxies") || strings.Contains(got, "servers {") {
		t.Fatalf("unset trusted proxies changed global options:\n%s", got)
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
		{"trusted proxy requires CIDR", ProxyRoute{TrustedProxies: []string{"203.0.113.1"}}, "invalid trusted proxy CIDR"},
		{"trusted proxy rejects broad IPv4", ProxyRoute{TrustedProxies: []string{"10.0.0.0/7"}}, "invalid trusted proxy CIDR"},
		{"trusted proxy rejects broad IPv6", ProxyRoute{TrustedProxies: []string{"2001:db8::/23"}}, "invalid trusted proxy CIDR"},
		{"trusted proxy rejects noncanonical", ProxyRoute{TrustedProxies: []string{"203.0.113.99/24"}}, "invalid trusted proxy CIDR"},
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

func TestValidateProxyRoutePolicyCanonicalizesManifest(t *testing.T) {
	manifest := accessControlManifest(ProxyRoute{
		DenyPathPrefixes: []string{"/api/internal", "/.well-known/workflow"},
		MaxRequestBody:   "10MB",
		ResponseHeaders:  map[string]string{"x-content-type-options": "nosniff"},
	})[0]
	if err := validateProxyRouteManifest(&manifest); err != nil {
		t.Fatal(err)
	}
	route := manifest.Routes[0]
	if strings.Join(route.DenyPathPrefixes, ",") != "/.well-known/workflow,/api/internal" || route.ResponseHeaders["X-Content-Type-Options"] != "nosniff" {
		t.Fatalf("route policy was not canonicalized: %+v", route)
	}
}

func TestValidateProxyRoutePolicyRejectsUnsafeManifest(t *testing.T) {
	tests := []struct {
		name   string
		route  ProxyRoute
		needle string
	}{
		{"path wildcard", ProxyRoute{DenyPathPrefixes: []string{"/api/*"}}, "denied path"},
		{"path encoding", ProxyRoute{DenyPathPrefixes: []string{"/api/%69nternal"}}, "denied path"},
		{"bad body limit", ProxyRoute{MaxRequestBody: "10MiB"}, "request body"},
		{"header injection", ProxyRoute{ResponseHeaders: map[string]string{"X-Test": "ok\r\nInjected: yes"}}, "response header"},
		{"framing header", ProxyRoute{ResponseHeaders: map[string]string{"Content-Length": "1"}}, "framing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := accessControlManifest(tt.route)[0]
			err := validateProxyRouteManifest(&manifest)
			if err == nil || !strings.Contains(err.Error(), tt.needle) {
				t.Fatalf("error = %v, want %q", err, tt.needle)
			}
		})
	}
}

func TestCaddyRoutePolicyRunsBeforeAccessAndUpstreamHandlers(t *testing.T) {
	caddyfile, err := renderCaddyfile(accessControlManifest(ProxyRoute{
		BasicAuth: &ProxyRouteBasicAuth{Username: "admin", PasswordBcrypt: takodTestBcryptHash},
		AllowIPs:  []string{"198.51.100.0/24"}, DenyPathPrefixes: []string{"/api/internal"}, MaxRequestBody: "10MB",
		ResponseHeaders: map[string]string{"X-Content-Type-Options": "nosniff"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if siteIndex := strings.LastIndex(caddyfile, "\nexample.com {"); siteIndex >= 0 {
		caddyfile = caddyfile[siteIndex:]
	}
	fragments := []string{
		">X-Content-Type-Options \"nosniff\"",
		"handle @tako_denied_path",
		"handle @tako_body_too_large",
		"handle @tako_body_length_required",
		"request_body @tako_body_unsized {",
		"handle @tako_safe_body_not_allowed",
		"request_body {",
		"handle @tako_allowed",
		"basic_auth {",
		"reverse_proxy http://demo-web:3000",
	}
	previous := -1
	for _, fragment := range fragments {
		index := strings.Index(caddyfile, fragment)
		if index < 0 || index <= previous {
			t.Fatalf("route policy ordering failed at %q (index %d after %d):\n%s", fragment, index, previous, caddyfile)
		}
		previous = index
	}
}
