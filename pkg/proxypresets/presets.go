// Package proxypresets resolves named CDN trusted-proxy presets (for example
// "cloudfront" and "cloudflare") to validated CIDR snapshots on the deploying
// machine, with a local cache and fail-closed fetch semantics.
package proxypresets

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
)

const (
	// CloudFront trusts AWS CloudFront origin-facing addresses.
	CloudFront = "cloudfront"
	// Cloudflare trusts Cloudflare's published edge addresses.
	Cloudflare = "cloudflare"

	// MaxPrefixes bounds a resolved preset so an empty or hijacked response
	// cannot silently widen trust.
	MaxPrefixes = 512
	// MaxBodyBytes caps each fetched range document.
	MaxBodyBytes = 16 << 20
	// FetchTimeout bounds each range URL request.
	FetchTimeout = 10 * time.Second
	// FreshFor is how long a cached snapshot is used without a network call.
	FreshFor = 24 * time.Hour
	// StaleLimit is the oldest cached snapshot used when a fetch fails.
	StaleLimit = 30 * 24 * time.Hour
	// MaxClockSkew is how far in the future a snapshot's fetch time may be.
	MaxClockSkew = 5 * time.Minute

	cloudFrontService = "CLOUDFRONT_ORIGIN_FACING"
)

type format int

const (
	formatAWSIPRanges format = iota + 1
	formatLines
)

type definition struct {
	urls   []string
	format format
}

var definitions = map[string]definition{
	CloudFront: {urls: []string{"https://ip-ranges.amazonaws.com/ip-ranges.json"}, format: formatAWSIPRanges},
	Cloudflare: {urls: []string{"https://www.cloudflare.com/ips-v4", "https://www.cloudflare.com/ips-v6"}, format: formatLines},
}

// Names returns the supported preset names in sorted order.
func Names() []string {
	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsPreset reports whether name is a supported preset name (case-sensitive,
// lowercase).
func IsPreset(name string) bool {
	_, ok := definitions[name]
	return ok
}

// URLs returns the range source URLs of a preset.
func URLs(name string) []string {
	return append([]string(nil), definitions[name].urls...)
}

// Snapshot is one resolved preset.
type Snapshot struct {
	Name              string    `json:"preset"`
	FetchedAt         time.Time `json:"fetchedAt"`
	SourceFingerprint string    `json:"sourceFingerprint"`
	SyncToken         string    `json:"syncToken,omitempty"`
	CIDRs             []string  `json:"cidrs"`
}

// PrefixTooBroad reports whether a trusted proxy prefix is broader than /8
// (IPv4) or /24 (IPv6).
func PrefixTooBroad(prefix netip.Prefix) bool {
	if prefix.Addr().Is4() {
		return prefix.Bits() < 8
	}
	return prefix.Bits() < 24
}

// NormalizePrefixes parses, masks, bounds, dedupes, and sorts preset CIDRs.
// A preset must resolve to between 1 and MaxPrefixes prefixes.
func NormalizePrefixes(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		prefix, err := netip.ParsePrefix(trimmed)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", trimmed)
		}
		prefix = prefix.Masked()
		if PrefixTooBroad(prefix) {
			return nil, fmt.Errorf("CIDR %q is too broad (minimum prefix is /8 for IPv4 and /24 for IPv6)", trimmed)
		}
		seen[prefix.String()] = struct{}{}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("no prefixes")
	}
	if len(seen) > MaxPrefixes {
		return nil, fmt.Errorf("%d prefixes exceeds the limit of %d", len(seen), MaxPrefixes)
	}
	out := make([]string, 0, len(seen))
	for prefix := range seen {
		out = append(out, prefix)
	}
	sort.Strings(out)
	return out, nil
}

type awsIPRanges struct {
	SyncToken string `json:"syncToken"`
	Prefixes  *[]struct {
		IPPrefix string `json:"ip_prefix"`
		Service  string `json:"service"`
	} `json:"prefixes"`
	IPv6Prefixes *[]struct {
		IPv6Prefix string `json:"ipv6_prefix"`
		Service    string `json:"service"`
	} `json:"ipv6_prefixes"`
}

// ParseAWSIPRanges extracts CLOUDFRONT_ORIGIN_FACING prefixes from AWS
// ip-ranges.json. The document must carry a syncToken and both prefix lists.
func ParseAWSIPRanges(body []byte) (cidrs []string, syncToken string, err error) {
	var doc awsIPRanges
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&doc); err != nil {
		return nil, "", fmt.Errorf("malformed ip-ranges.json: %w", err)
	}
	if decoder.More() {
		return nil, "", fmt.Errorf("malformed ip-ranges.json: trailing data")
	}
	if strings.TrimSpace(doc.SyncToken) == "" || doc.Prefixes == nil || doc.IPv6Prefixes == nil {
		return nil, "", fmt.Errorf("malformed ip-ranges.json: missing syncToken, prefixes, or ipv6_prefixes")
	}
	for _, entry := range *doc.Prefixes {
		if entry.Service == cloudFrontService {
			cidrs = append(cidrs, entry.IPPrefix)
		}
	}
	for _, entry := range *doc.IPv6Prefixes {
		if entry.Service == cloudFrontService {
			cidrs = append(cidrs, entry.IPv6Prefix)
		}
	}
	return cidrs, strings.TrimSpace(doc.SyncToken), nil
}

// ParseLines parses a list with one CIDR per line. Blank lines are ignored;
// any other content must be a CIDR.
func ParseLines(body []byte) ([]string, error) {
	var cidrs []string
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 4096), 4096)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if _, err := netip.ParsePrefix(line); err != nil {
			return nil, fmt.Errorf("malformed range list line %q", line)
		}
		cidrs = append(cidrs, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("malformed range list: %w", err)
	}
	return cidrs, nil
}

// parseBodies converts the raw source documents of a preset into normalized
// CIDRs. bodies are in the preset's URL order.
func parseBodies(name string, bodies [][]byte) ([]string, string, error) {
	def, ok := definitions[name]
	if !ok {
		return nil, "", fmt.Errorf("unknown trusted proxy preset %q", name)
	}
	if len(bodies) != len(def.urls) {
		return nil, "", fmt.Errorf("preset %s expects %d range documents, got %d", name, len(def.urls), len(bodies))
	}
	var raw []string
	syncToken := ""
	for _, body := range bodies {
		switch def.format {
		case formatAWSIPRanges:
			cidrs, token, err := ParseAWSIPRanges(body)
			if err != nil {
				return nil, "", err
			}
			raw = append(raw, cidrs...)
			syncToken = token
		case formatLines:
			cidrs, err := ParseLines(body)
			if err != nil {
				return nil, "", err
			}
			raw = append(raw, cidrs...)
		}
	}
	cidrs, err := NormalizePrefixes(raw)
	if err != nil {
		return nil, "", err
	}
	return cidrs, syncToken, nil
}

// ValidateSnapshot checks a snapshot the same way a fetched response is
// checked, and that its fetch time is not more than MaxClockSkew ahead of now.
func ValidateSnapshot(snapshot Snapshot, now time.Time) error {
	if !IsPreset(snapshot.Name) {
		return fmt.Errorf("unknown trusted proxy preset %q", snapshot.Name)
	}
	if snapshot.FetchedAt.IsZero() {
		return fmt.Errorf("preset %s snapshot has no fetch time", snapshot.Name)
	}
	if snapshot.FetchedAt.After(now.Add(MaxClockSkew)) {
		return fmt.Errorf("preset %s snapshot fetch time %s is in the future", snapshot.Name, snapshot.FetchedAt.UTC().Format(time.RFC3339))
	}
	if !isHexSHA256(snapshot.SourceFingerprint) {
		return fmt.Errorf("preset %s snapshot has an invalid source fingerprint", snapshot.Name)
	}
	normalized, err := NormalizePrefixes(snapshot.CIDRs)
	if err != nil {
		return fmt.Errorf("preset %s snapshot: %w", snapshot.Name, err)
	}
	if strings.Join(normalized, ",") != strings.Join(snapshot.CIDRs, ",") {
		return fmt.Errorf("preset %s snapshot CIDRs are not canonical", snapshot.Name)
	}
	return nil
}

func isHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
