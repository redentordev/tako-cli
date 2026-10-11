package proxypresets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Doer performs HTTP requests; *http.Client satisfies it. Tests inject a fake.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// ResolveError reports a preset that could not be resolved from the network
// or a usable cache. The operation must stop before any mutation.
type ResolveError struct {
	Preset string
	URL    string
	Cause  string
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("trusted proxy preset %s could not be resolved: fetching %s failed: %s; no cached ranges younger than 30 days are available. Retry when the range URL is reachable, or list the CIDRs explicitly in proxy.trustedProxies", e.Preset, e.URL, e.Cause)
}

// Resolver resolves presets with a 24h cache and a 30-day stale fallback.
// Every field is optional; zero values use production defaults.
type Resolver struct {
	// Client performs range fetches. Defaults to an HTTPS-only client.
	Client Doer
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// CacheDir is the directory holding <preset>.json. When empty, the
	// default <os.UserCacheDir()>/tako/trusted-proxies is used.
	CacheDir string
	// DisableCache resolves without reading or writing any cache.
	DisableCache bool
	// UserAgent is sent with each fetch. Defaults to "tako-cli/unknown".
	UserAgent string
	// Warn receives non-fatal diagnostics (stale cache use, cache write
	// failures).
	Warn func(message string)
}

// DefaultCacheDir returns <os.UserCacheDir()>/tako/trusted-proxies.
func DefaultCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(base) == "" {
		return "", fmt.Errorf("user cache directory is empty")
	}
	return filepath.Join(base, "tako", "trusted-proxies"), nil
}

// CachePath returns the cache file path for a preset in dir.
func CachePath(dir, name string) string {
	return filepath.Join(dir, name+".json")
}

func (r *Resolver) now() time.Time {
	if r != nil && r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Resolver) warn(message string) {
	if r != nil && r.Warn != nil {
		r.Warn(message)
	}
}

func (r *Resolver) cacheDir() string {
	if r.DisableCache {
		return ""
	}
	if r.CacheDir != "" {
		return r.CacheDir
	}
	dir, err := DefaultCacheDir()
	if err != nil {
		return ""
	}
	return dir
}

func (r *Resolver) client() Doer {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{
		Timeout: FetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing non-HTTPS redirect to %s", req.URL.Redacted())
			}
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}

// Resolve returns a validated snapshot of the named preset.
func (r *Resolver) Resolve(ctx context.Context, name string) (Snapshot, error) {
	if !IsPreset(name) {
		return Snapshot{}, fmt.Errorf("unknown trusted proxy preset %q (supported: %s)", name, strings.Join(Names(), ", "))
	}
	now := r.now()
	dir := r.cacheDir()
	cached, cachedOK := Snapshot{}, false
	if dir != "" {
		cached, cachedOK = readCache(dir, name, now)
	}
	if cachedOK && now.Sub(cached.FetchedAt) < FreshFor {
		return cached, nil
	}

	snapshot, failedURL, fetchErr := r.fetch(ctx, name, now)
	if fetchErr == nil {
		if dir != "" {
			if err := writeCache(dir, snapshot); err != nil {
				r.warn(fmt.Sprintf("could not cache trusted proxy preset %s: %v", name, err))
			}
		}
		return snapshot, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Snapshot{}, ctxErr
	}
	if cachedOK && now.Sub(cached.FetchedAt) < StaleLimit {
		age := now.Sub(cached.FetchedAt)
		if age < 0 {
			age = 0
		}
		r.warn(fmt.Sprintf("trusted proxy preset %s: fetching %s failed (%v); using cached ranges fetched %s ago", name, failedURL, fetchErr, formatAge(age)))
		return cached, nil
	}
	return Snapshot{}, &ResolveError{Preset: name, URL: failedURL, Cause: fetchErr.Error()}
}

func (r *Resolver) fetch(ctx context.Context, name string, now time.Time) (Snapshot, string, error) {
	def := definitions[name]
	bodies := make([][]byte, 0, len(def.urls))
	hash := sha256.New()
	for _, rawURL := range def.urls {
		body, err := r.fetchURL(ctx, rawURL)
		if err != nil {
			return Snapshot{}, rawURL, err
		}
		bodies = append(bodies, body)
		fmt.Fprintf(hash, "%s\n%d\n", rawURL, len(body))
		hash.Write(body)
	}
	cidrs, syncToken, err := parseBodies(name, bodies)
	if err != nil {
		return Snapshot{}, strings.Join(def.urls, ", "), err
	}
	return Snapshot{
		Name:              name,
		FetchedAt:         now,
		SourceFingerprint: hex.EncodeToString(hash.Sum(nil)),
		SyncToken:         syncToken,
		CIDRs:             cidrs,
	}, "", nil
}

func (r *Resolver) fetchURL(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" {
		return nil, fmt.Errorf("range URL must use HTTPS")
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	userAgent := strings.TrimSpace(r.UserAgent)
	if userAgent == "" {
		userAgent = "tako-cli/unknown"
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", MaxBodyBytes)
	}
	return body, nil
}

// ReadCachedSnapshot returns the valid cached snapshot of a preset in dir.
func ReadCachedSnapshot(dir, name string, now time.Time) (Snapshot, bool) {
	return readCache(dir, name, now)
}

func readCache(dir, name string, now time.Time) (Snapshot, bool) {
	data, err := os.ReadFile(CachePath(dir, name))
	if err != nil || len(data) > MaxBodyBytes {
		return Snapshot{}, false
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Snapshot{}, false
	}
	if snapshot.Name != name {
		return Snapshot{}, false
	}
	if err := ValidateSnapshot(snapshot, now); err != nil {
		return Snapshot{}, false
	}
	snapshot.FetchedAt = snapshot.FetchedAt.UTC()
	return snapshot, true
}

func writeCache(dir string, snapshot Snapshot) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+snapshot.Name+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		return cleanup(err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, CachePath(dir, snapshot.Name)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func formatAge(age time.Duration) string {
	if age >= 48*time.Hour {
		return fmt.Sprintf("%d days", int(age/(24*time.Hour)))
	}
	if age >= time.Hour {
		return fmt.Sprintf("%d hours", int(age/time.Hour))
	}
	return fmt.Sprintf("%d minutes", int(age/time.Minute))
}

// FormatAge renders a cache age for diagnostics.
func FormatAge(age time.Duration) string { return formatAge(age) }
