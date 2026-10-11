package takod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	version := 1
	if len(route.AllowPathPrefixes) > 0 {
		version = proxyRouteAllowPathManifestVersion
	}
	return []ProxyRouteManifest{{
		Version:     version,
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

func presetSnapshot(name string, fetchedAt time.Time, fingerprint string, cidrs ...string) ProxyTrustedProxyPreset {
	return ProxyTrustedProxyPreset{Name: name, FetchedAt: fetchedAt, SourceFingerprint: strings.Repeat(fingerprint, 64/len(fingerprint)), CIDRs: cidrs}
}

func TestRenderCaddyfileNewestPresetSnapshotWinsAcrossProjects(t *testing.T) {
	old := time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)
	fresh := old.Add(30 * 24 * time.Hour)
	manifests := []ProxyRouteManifest{{
		Version: 1, Project: "alpha", Environment: "production",
		Routes: []ProxyRoute{{
			Service: "a", Domains: []string{"a.example.com"}, Upstreams: []string{"http://a:3000"},
			TrustedProxies:      []string{"10.0.0.0/8"},
			TrustedProxyPresets: []ProxyTrustedProxyPreset{presetSnapshot("cloudfront", old, "a", "15.158.0.0/16")},
			AllowIPs:            []string{"198.51.100.0/24"},
		}},
	}, {
		Version: 1, Project: "beta", Environment: "production",
		Routes: []ProxyRoute{{
			Service: "b", Domains: []string{"b.example.com"}, Upstreams: []string{"http://b:3000"},
			TrustedProxies:      []string{"10.0.0.0/8"},
			TrustedProxyPresets: []ProxyTrustedProxyPreset{presetSnapshot("cloudfront", fresh, "b", "13.124.199.0/24", "15.158.0.0/16")},
		}},
	}}
	caddyfile, err := renderCaddyfile(manifests)
	if err != nil {
		t.Fatalf("renderCaddyfile returned error: %v", err)
	}
	if !strings.Contains(caddyfile, "\t\ttrusted_proxies static 10.0.0.0/8 13.124.199.0/24 15.158.0.0/16\n") {
		t.Fatalf("newest snapshot was not used:\n%s", caddyfile)
	}
	if !strings.Contains(caddyfile, "@tako_allowed client_ip 198.51.100.0/24") {
		t.Fatalf("preset-only trust did not switch allowIps to client_ip:\n%s", caddyfile)
	}

	// Rendering in the other order gives the same result: A cannot regress B.
	reversed, err := renderCaddyfile([]ProxyRouteManifest{manifests[1], manifests[0]})
	if err != nil || reversed != caddyfile {
		t.Fatalf("render order changed result (err %v)", err)
	}

	// Equal fetch times break ties by the greatest fingerprint.
	manifests[0].Routes[0].TrustedProxyPresets[0].FetchedAt = fresh
	manifests[0].Routes[0].TrustedProxyPresets[0].SourceFingerprint = strings.Repeat("f", 64)
	tied, err := renderCaddyfile(manifests)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tied, "\t\ttrusted_proxies static 10.0.0.0/8 15.158.0.0/16\n") {
		t.Fatalf("fingerprint tie-break was not applied:\n%s", tied)
	}
}

func TestRenderCaddyfilePresetVersusExplicitCIDRsConflict(t *testing.T) {
	now := time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)
	for _, routes := range [][]ProxyRoute{
		{
			{Service: "a", Domains: []string{"a.example.com"}, Upstreams: []string{"http://a:3000"}, TrustedProxyPresets: []ProxyTrustedProxyPreset{presetSnapshot("cloudfront", now, "a", "15.158.0.0/16")}},
			{Service: "b", Domains: []string{"b.example.com"}, Upstreams: []string{"http://b:3000"}, TrustedProxies: []string{"15.158.0.0/16"}},
		},
		{
			{Service: "a", Domains: []string{"a.example.com"}, Upstreams: []string{"http://a:3000"}, TrustedProxyPresets: []ProxyTrustedProxyPreset{presetSnapshot("cloudfront", now, "a", "15.158.0.0/16")}},
			{Service: "b", Domains: []string{"b.example.com"}, Upstreams: []string{"http://b:3000"}, TrustedProxyPresets: []ProxyTrustedProxyPreset{presetSnapshot("cloudflare", now, "a", "173.245.48.0/20")}},
		},
	} {
		_, err := renderCaddyfile([]ProxyRouteManifest{{Version: 1, Project: "demo", Environment: "production", Routes: routes}})
		if err == nil || !strings.Contains(err.Error(), "conflicting trusted proxy CIDR sets") {
			t.Fatalf("error = %v, want conflicting trusted proxy sets", err)
		}
	}
}

func TestValidateProxyRouteManifestRejectsUnsafePresetSnapshots(t *testing.T) {
	now := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	restore := proxyRouteNow
	proxyRouteNow = func() time.Time { return now }
	t.Cleanup(func() { proxyRouteNow = restore })
	many := make([]string, 0, 513)
	for i := 0; i < 513; i++ {
		many = append(many, fmt.Sprintf("10.%d.%d.0/24", i/256, i%256))
	}
	cases := []struct {
		name   string
		preset ProxyTrustedProxyPreset
		want   string
	}{
		{"future snapshot", presetSnapshot("cloudfront", now.Add(time.Hour), "a", "15.158.0.0/16"), "ahead of this node's clock"},
		{"unknown preset", presetSnapshot("fastly", now, "a", "15.158.0.0/16"), "unknown trusted proxy preset"},
		{"broad prefix", presetSnapshot("cloudfront", now, "a", "0.0.0.0/0"), "invalid trusted proxy CIDR"},
		{"noncanonical prefix", presetSnapshot("cloudfront", now, "a", "15.158.1.0/16"), "invalid trusted proxy CIDR"},
		{"injection", presetSnapshot("cloudfront", now, "a", "15.158.0.0/16\n}"), "invalid trusted proxy CIDR"},
		{"empty", presetSnapshot("cloudfront", now, "a"), "between 1 and 512"},
		{"too many", presetSnapshot("cloudfront", now, "a", many...), "between 1 and 512"},
		{"bad fingerprint", ProxyTrustedProxyPreset{Name: "cloudfront", FetchedAt: now, SourceFingerprint: "zz", CIDRs: []string{"15.158.0.0/16"}}, "fingerprint"},
		{"zero time", presetSnapshot("cloudfront", time.Time{}, "a", "15.158.0.0/16"), "no fetch time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := accessControlManifest(ProxyRoute{TrustedProxyPresets: []ProxyTrustedProxyPreset{tc.preset}})[0]
			err := validateProxyRouteManifest(&manifest)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	ok := accessControlManifest(ProxyRoute{TrustedProxyPresets: []ProxyTrustedProxyPreset{presetSnapshot("cloudfront", now.Add(4*time.Minute), "a", "15.158.0.0/16")}})[0]
	if err := validateProxyRouteManifest(&ok); err != nil {
		t.Fatalf("snapshot within clock skew rejected: %v", err)
	}
}

func TestWriteProxyFileRejectsFuturePresetSnapshot(t *testing.T) {
	useTempProxyPaths(t)
	valid := `{"version":1,"project":"demo","environment":"production","routes":[{"service":"web","domains":["example.com"],"upstreams":["http://demo-web:3000"]}]}`
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: "demo-production.json", Content: valid}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	content := `{"version":1,"project":"demo","environment":"production","routes":[{"service":"web","domains":["example.com"],"upstreams":["http://demo-web:3000"],"trustedProxyPresets":[{"name":"cloudfront","fetchedAt":"` + future + `","sourceFingerprint":"` + strings.Repeat("a", 64) + `","cidrs":["15.158.0.0/16"]}]}]}`
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: "demo-production.json", Content: content}); err == nil || !strings.Contains(err.Error(), "ahead of this node's clock") {
		t.Fatalf("future snapshot error = %v", err)
	}
	after, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("rejected snapshot changed the Caddyfile (%v)", err)
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
		DenyPathPrefixes:  []string{"/api/internal", "/.well-known/workflow"},
		AllowPathPrefixes: []string{"/health", "/api"},
		MaxRequestBody:    "10MB",
		ResponseHeaders:   map[string]string{"x-content-type-options": "nosniff"},
	})[0]
	if err := validateProxyRouteManifest(&manifest); err != nil {
		t.Fatal(err)
	}
	route := manifest.Routes[0]
	if strings.Join(route.DenyPathPrefixes, ",") != "/.well-known/workflow,/api/internal" || route.ResponseHeaders["X-Content-Type-Options"] != "nosniff" {
		t.Fatalf("route policy was not canonicalized: %+v", route)
	}
	if strings.Join(route.AllowPathPrefixes, ",") != "/api,/health" {
		t.Fatalf("allowed paths were not canonicalized: %+v", route)
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
		{"allowed path wildcard", ProxyRoute{AllowPathPrefixes: []string{"/api/*"}}, "allowed path"},
		{"allowed path encoding", ProxyRoute{AllowPathPrefixes: []string{"/api/%2fadmin"}}, "allowed path"},
		{"allowed dot segment", ProxyRoute{AllowPathPrefixes: []string{"/api/.."}}, "allowed path"},
		{"allowed noncanonical path", ProxyRoute{AllowPathPrefixes: []string{"/api/"}}, "invalid allowed path"},
		{"bad body limit", ProxyRoute{MaxRequestBody: "10MiB"}, "request body"},
		{"header injection", ProxyRoute{ResponseHeaders: map[string]string{"X-Test": "ok\r\nInjected: yes"}}, "response header"},
		{"framing header", ProxyRoute{ResponseHeaders: map[string]string{"Content-Length": "1"}}, "framing"},
		{"request header newline", ProxyRoute{RequestHeaders: map[string]string{"X-Test": "ok\nrespond 200"}}, "request header value"},
		{"request header quote", ProxyRoute{RequestHeaders: map[string]string{"X-Test": `ok" respond 200`}}, "request header value"},
		{"request header placeholder", ProxyRoute{RequestHeaders: map[string]string{"X-Forwarded-Host": "{http.request.host}"}}, "request header value"},
		{"request header backslash", ProxyRoute{RequestHeaders: map[string]string{"X-Test": `a\b`}}, "request header value"},
		{"request header padded", ProxyRoute{RequestHeaders: map[string]string{"X-Test": " a"}}, "request header value"},
		{"request header host", ProxyRoute{RequestHeaders: map[string]string{"host": "evil.example.com"}}, "hop-by-hop or framing"},
		{"request header http2-settings", ProxyRoute{RequestHeaders: map[string]string{"Http2-Settings": "AAMAAABkAAQAAP__"}}, "hop-by-hop or framing"},
		{"request header xff", ProxyRoute{RequestHeaders: map[string]string{"x-forwarded-for": "1.2.3.4"}}, "client-identity"},
		{"request header x-real-ip", ProxyRoute{RequestHeaders: map[string]string{"X-Real-IP": "1.2.3.4"}}, "client-identity"},
		{"request header forwarded", ProxyRoute{RequestHeaders: map[string]string{"Forwarded": "for=1.2.3.4"}}, "client-identity"},
		{"request header name", ProxyRoute{RequestHeaders: map[string]string{"X Test": "a"}}, "request header name"},
		{"request header duplicate", ProxyRoute{RequestHeaders: map[string]string{"X-Test": "a", "x-test": "b"}}, "duplicate request header"},
		{"too many request headers", ProxyRoute{RequestHeaders: manyRequestHeaders(33)}, "too many request headers"},
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

func manyRequestHeaders(count int) map[string]string {
	headers := make(map[string]string, count)
	for i := 0; i < count; i++ {
		headers["X-Header-"+strings.Repeat("a", i+1)] = "v"
	}
	return headers
}

func TestValidateProxyRouteRequestHeadersCanonicalizes(t *testing.T) {
	manifest := accessControlManifest(ProxyRoute{RequestHeaders: map[string]string{"x-forwarded-host": "www.example.com"}})[0]
	if err := validateProxyRouteManifest(&manifest); err != nil {
		t.Fatal(err)
	}
	if got := manifest.Routes[0].RequestHeaders; len(got) != 1 || got["X-Forwarded-Host"] != "www.example.com" {
		t.Fatalf("request headers were not canonicalized: %#v", got)
	}
	exact := accessControlManifest(ProxyRoute{RequestHeaders: manyRequestHeaders(32)})[0]
	if err := validateProxyRouteManifest(&exact); err != nil {
		t.Fatalf("32 request headers rejected: %v", err)
	}
}

func TestWriteProxyFileRejectsHandCraftedRequestHeaderInjection(t *testing.T) {
	useTempProxyPaths(t)
	valid := `{"version":1,"project":"demo","environment":"production","routes":[{"service":"web","domains":["example.com"],"upstreams":["http://demo-web:3000"]}]}`
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: "demo-production.json", Content: valid}); err != nil {
		t.Fatalf("WriteProxyFile(valid) returned error: %v", err)
	}
	before, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`ok\nrespond 200`, `ok\" respond 200`, `{http.request.host}`} {
		injected := `{"version":1,"project":"demo","environment":"production","routes":[{"service":"web","domains":["example.com"],"upstreams":["http://demo-web:3000"],"requestHeaders":{"X-Forwarded-Host":"` + value + `"}}]}`
		if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: "demo-production.json", Content: injected}); err == nil {
			t.Fatalf("hand-crafted request header %q was accepted", value)
		}
		after, err := os.ReadFile(proxyCaddyfilePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatalf("rejected manifest changed the Caddyfile:\n%s", after)
		}
		stored, err := os.ReadFile(filepath.Join(proxyRoutesDir, "demo-production.json"))
		if err != nil || string(stored) != valid {
			t.Fatalf("rejected manifest replaced the stored route file: %q (%v)", stored, err)
		}
	}
}

func TestAllowPathPrefixesRequireNewManifestVersion(t *testing.T) {
	manifest := accessControlManifest(ProxyRoute{AllowPathPrefixes: []string{"/api"}})[0]
	manifest.Version = 1
	err := validateProxyRouteManifest(&manifest)
	if err == nil || !strings.Contains(err.Error(), "manifest version 3") {
		t.Fatalf("legacy manifest allow-path error = %v", err)
	}
}

func TestCaddyRoutePolicyRunsBeforeAccessAndUpstreamHandlers(t *testing.T) {
	caddyfile, err := renderCaddyfile(accessControlManifest(ProxyRoute{
		BasicAuth: &ProxyRouteBasicAuth{Username: "admin", PasswordBcrypt: takodTestBcryptHash},
		AllowIPs:  []string{"198.51.100.0/24"}, DenyPathPrefixes: []string{"/api/internal"}, MaxRequestBody: "10MB",
		AllowPathPrefixes: []string{"/api", "/health"},
		ResponseHeaders:   map[string]string{"X-Content-Type-Options": "nosniff"},
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
		"handle @tako_allowed_path",
		"handle @tako_body_too_large",
		"handle @tako_body_length_required",
		"request_body @tako_body_unsized {",
		"handle @tako_safe_body_not_allowed",
		"request_body {",
		"handle @tako_allowed {",
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
