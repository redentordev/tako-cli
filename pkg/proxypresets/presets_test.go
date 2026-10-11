package proxypresets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	awsURL          = "https://ip-ranges.amazonaws.com/ip-ranges.json"
	cloudflareV4URL = "https://www.cloudflare.com/ips-v4"
	cloudflareV6URL = "https://www.cloudflare.com/ips-v6"
)

type fakeResponse struct {
	status int
	body   []byte
	err    error
}

type fakeDoer struct {
	mu        sync.Mutex
	responses map[string]fakeResponse
	calls     []string
	agents    []string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req.URL.String())
	f.agents = append(f.agents, req.Header.Get("User-Agent"))
	response, ok := f.responses[req.URL.String()]
	if !ok {
		return nil, fmt.Errorf("dial tcp: no route to host")
	}
	if response.err != nil {
		return nil, response.err
	}
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(response.body)), Header: http.Header{}}, nil
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fixtureDoer(t *testing.T) *fakeDoer {
	return &fakeDoer{responses: map[string]fakeResponse{
		awsURL:          {body: fixture(t, "aws-ip-ranges.json")},
		cloudflareV4URL: {body: fixture(t, "cloudflare-ips-v4.txt")},
		cloudflareV6URL: {body: fixture(t, "cloudflare-ips-v6.txt")},
	}}
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func TestParseAWSIPRangesFixtureSelectsOriginFacingOnly(t *testing.T) {
	cidrs, token, err := ParseAWSIPRanges(fixture(t, "aws-ip-ranges.json"))
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := NormalizePrefixes(cidrs)
	if err != nil {
		t.Fatal(err)
	}
	want := "13.124.199.0/24,15.158.0.0/16,2600:9000:ddd::/48,64.252.64.0/18"
	if strings.Join(normalized, ",") != want || token != "1760000000" {
		t.Fatalf("cloudfront = %v (token %q), want %s", normalized, token, want)
	}
}

func TestParseCloudflareFixtures(t *testing.T) {
	cidrs, _, err := parseBodies(Cloudflare, [][]byte{fixture(t, "cloudflare-ips-v4.txt"), fixture(t, "cloudflare-ips-v6.txt")})
	if err != nil {
		t.Fatal(err)
	}
	if len(cidrs) != 22 || cidrs[0] != "103.21.244.0/22" {
		t.Fatalf("cloudflare = %v", cidrs)
	}
}

func TestParseRejectsSuspiciousResponses(t *testing.T) {
	many := &strings.Builder{}
	for i := 0; i < MaxPrefixes+1; i++ {
		fmt.Fprintf(many, "10.%d.%d.0/24\n", i/256, i%256)
	}
	tests := []struct {
		name   string
		preset string
		bodies [][]byte
		want   string
	}{
		{"malformed json", CloudFront, [][]byte{[]byte(`{"syncToken":`)}, "malformed"},
		{"missing lists", CloudFront, [][]byte{[]byte(`{"syncToken":"1"}`)}, "missing"},
		{"empty cloudfront", CloudFront, [][]byte{[]byte(`{"syncToken":"1","prefixes":[],"ipv6_prefixes":[]}`)}, "no prefixes"},
		{"empty cloudflare", Cloudflare, [][]byte{[]byte("\n"), []byte("")}, "no prefixes"},
		{"html instead of list", Cloudflare, [][]byte{[]byte("<html>blocked</html>"), []byte("")}, "malformed"},
		{"too broad ipv4", Cloudflare, [][]byte{[]byte("0.0.0.0/0\n"), []byte("2400:cb00::/32\n")}, "too broad"},
		{"too broad ipv6", Cloudflare, [][]byte{[]byte("173.245.48.0/20\n"), []byte("2400::/16\n")}, "too broad"},
		{"too many prefixes", Cloudflare, [][]byte{[]byte(many.String()), []byte("")}, "exceeds the limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parseBodies(tt.preset, tt.bodies)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestFetchRejectsOversizeBody(t *testing.T) {
	doer := &fakeDoer{responses: map[string]fakeResponse{awsURL: {body: bytes.Repeat([]byte(" "), MaxBodyBytes+1)}}}
	resolver := &Resolver{Client: doer, DisableCache: true}
	_, err := resolver.Resolve(context.Background(), CloudFront)
	var resolveErr *ResolveError
	if !errors.As(err, &resolveErr) || !strings.Contains(err.Error(), "exceeds") || !strings.Contains(err.Error(), awsURL) {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveFetchesAndWritesCache(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	doer := fixtureDoer(t)
	resolver := &Resolver{Client: doer, Now: (&clock{now}).Now, CacheDir: dir, UserAgent: "tako-cli/v1.2.3"}
	snapshot, err := resolver.Resolve(context.Background(), Cloudflare)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.FetchedAt.Equal(now) || len(snapshot.SourceFingerprint) != 64 || len(snapshot.CIDRs) != 22 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if len(doer.calls) != 2 || doer.agents[0] != "tako-cli/v1.2.3" {
		t.Fatalf("calls = %v agents = %v", doer.calls, doer.agents)
	}
	data, err := os.ReadFile(CachePath(dir, Cloudflare))
	if err != nil {
		t.Fatal(err)
	}
	var cached map[string]any
	if err := json.Unmarshal(data, &cached); err != nil || cached["preset"] != Cloudflare || cached["sourceFingerprint"] != snapshot.SourceFingerprint {
		t.Fatalf("cache file = %s (%v)", data, err)
	}
}

func TestResolveFreshCacheMakesNoNetworkCall(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	seed := &Resolver{Client: fixtureDoer(t), Now: (&clock{start}).Now, CacheDir: dir}
	first, err := seed.Resolve(context.Background(), Cloudflare)
	if err != nil {
		t.Fatal(err)
	}
	offline := &fakeDoer{}
	resolver := &Resolver{Client: offline, Now: (&clock{start.Add(2 * time.Hour)}).Now, CacheDir: dir}
	got, err := resolver.Resolve(context.Background(), Cloudflare)
	if err != nil {
		t.Fatal(err)
	}
	if len(offline.calls) != 0 || !got.FetchedAt.Equal(first.FetchedAt) {
		t.Fatalf("calls = %v, snapshot = %+v", offline.calls, got)
	}
}

func TestResolveStaleCacheIsRefetched(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	if _, err := (&Resolver{Client: fixtureDoer(t), Now: (&clock{start}).Now, CacheDir: dir}).Resolve(context.Background(), CloudFront); err != nil {
		t.Fatal(err)
	}
	later := start.Add(25 * time.Hour)
	doer := fixtureDoer(t)
	got, err := (&Resolver{Client: doer, Now: (&clock{later}).Now, CacheDir: dir}).Resolve(context.Background(), CloudFront)
	if err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 1 || !got.FetchedAt.Equal(later) {
		t.Fatalf("calls = %v snapshot = %+v", doer.calls, got)
	}
	cached, ok := ReadCachedSnapshot(dir, CloudFront, later)
	if !ok || !cached.FetchedAt.Equal(later) {
		t.Fatalf("cache was not refreshed: %+v", cached)
	}
}

func TestResolveFetchFailureUsesRecentCacheWithWarningAndKeepsFetchedAt(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	if _, err := (&Resolver{Client: fixtureDoer(t), Now: (&clock{start}).Now, CacheDir: dir}).Resolve(context.Background(), CloudFront); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	resolver := &Resolver{Client: &fakeDoer{}, Now: (&clock{start.Add(72 * time.Hour)}).Now, CacheDir: dir, Warn: func(m string) { warnings = append(warnings, m) }}
	got, err := resolver.Resolve(context.Background(), CloudFront)
	if err != nil {
		t.Fatal(err)
	}
	if !got.FetchedAt.Equal(start) {
		t.Fatalf("stale snapshot fetchedAt = %s, want original %s", got.FetchedAt, start)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "cloudfront") || !strings.Contains(warnings[0], "3 days") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestResolveFetchFailureWithoutUsableCacheFails(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	_, err := (&Resolver{Client: &fakeDoer{}, Now: (&clock{start}).Now, CacheDir: dir}).Resolve(context.Background(), CloudFront)
	var resolveErr *ResolveError
	if !errors.As(err, &resolveErr) || resolveErr.URL != awsURL || !strings.Contains(err.Error(), "list the CIDRs explicitly") {
		t.Fatalf("no-cache error = %v", err)
	}

	if _, err := (&Resolver{Client: fixtureDoer(t), Now: (&clock{start}).Now, CacheDir: dir}).Resolve(context.Background(), CloudFront); err != nil {
		t.Fatal(err)
	}
	_, err = (&Resolver{Client: &fakeDoer{}, Now: (&clock{start.Add(31 * 24 * time.Hour)}).Now, CacheDir: dir}).Resolve(context.Background(), CloudFront)
	if !errors.As(err, &resolveErr) {
		t.Fatalf("expired-cache error = %v", err)
	}
}

func TestResolveFetchErrorIsNotClassifiedAsCancellation(t *testing.T) {
	doer := &fakeDoer{responses: map[string]fakeResponse{awsURL: {err: context.DeadlineExceeded}}}
	_, err := (&Resolver{Client: doer, DisableCache: true}).Resolve(context.Background(), CloudFront)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch timeout must surface as a ResolveError, got %v", err)
	}
}

func TestResolveTreatsCorruptOrFutureCacheAsMissing(t *testing.T) {
	now := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	valid, _, err := parseBodies(Cloudflare, [][]byte{fixture(t, "cloudflare-ips-v4.txt"), fixture(t, "cloudflare-ips-v6.txt")})
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{
		"corrupt json":    `{"preset":`,
		"wrong preset":    mustJSON(t, Snapshot{Name: CloudFront, FetchedAt: now, SourceFingerprint: strings.Repeat("a", 64), CIDRs: valid}),
		"too broad":       mustJSON(t, Snapshot{Name: Cloudflare, FetchedAt: now, SourceFingerprint: strings.Repeat("a", 64), CIDRs: []string{"0.0.0.0/0"}}),
		"future":          mustJSON(t, Snapshot{Name: Cloudflare, FetchedAt: now.Add(6 * time.Minute), SourceFingerprint: strings.Repeat("a", 64), CIDRs: valid}),
		"empty":           mustJSON(t, Snapshot{Name: Cloudflare, FetchedAt: now, SourceFingerprint: strings.Repeat("a", 64)}),
		"bad fingerprint": mustJSON(t, Snapshot{Name: Cloudflare, FetchedAt: now, SourceFingerprint: "nope", CIDRs: valid}),
	}
	for name, content := range entries {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(CachePath(dir, Cloudflare), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			doer := fixtureDoer(t)
			got, err := (&Resolver{Client: doer, Now: (&clock{now}).Now, CacheDir: dir}).Resolve(context.Background(), Cloudflare)
			if err != nil {
				t.Fatal(err)
			}
			if len(doer.calls) != 2 || !got.FetchedAt.Equal(now) {
				t.Fatalf("invalid cache entry was used: calls=%v snapshot=%+v", doer.calls, got)
			}
			offline := &Resolver{Client: &fakeDoer{}, Now: (&clock{now}).Now, CacheDir: t.TempDir()}
			if err := os.WriteFile(CachePath(offline.CacheDir, Cloudflare), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := offline.Resolve(context.Background(), Cloudflare); err == nil {
				t.Fatal("invalid cache entry satisfied an offline resolution")
			}
		})
	}
}

func TestResolveWithoutCacheDirStillFetches(t *testing.T) {
	doer := fixtureDoer(t)
	if _, err := (&Resolver{Client: doer, DisableCache: true}).Resolve(context.Background(), Cloudflare); err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 2 {
		t.Fatalf("calls = %v", doer.calls)
	}
}

func TestResolveUnwritableCacheWarnsAndResolves(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	resolver := &Resolver{Client: fixtureDoer(t), CacheDir: filepath.Join(blocker, "trusted-proxies"), Warn: func(m string) { warnings = append(warnings, m) }}
	if _, err := resolver.Resolve(context.Background(), Cloudflare); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "could not cache") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestResolveRejectsUnknownPreset(t *testing.T) {
	_, err := (&Resolver{Client: &fakeDoer{}, DisableCache: true}).Resolve(context.Background(), "fastly")
	if err == nil || !strings.Contains(err.Error(), "cloudflare, cloudfront") {
		t.Fatalf("error = %v", err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
