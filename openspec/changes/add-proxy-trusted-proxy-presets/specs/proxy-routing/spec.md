# Spec Delta

## Purpose

Defines how Tako's per-service proxy decides which peers are trusted to supply forwarded headers, how CDN range presets are resolved and shared on a proxy node, and which fixed request headers may be set on upstream requests.

## ADDED Requirements

### Requirement: Trusted proxy presets
`proxy.trustedProxies` SHALL accept the preset names `cloudfront` and `cloudflare` in addition to CIDRs. At deploy time Tako SHALL resolve `cloudfront` to the `CLOUDFRONT_ORIGIN_FACING` prefixes from AWS `ip-ranges.json`, and `cloudflare` to Cloudflare's published `ips-v4` and `ips-v6` lists. Resolved prefixes SHALL pass the same validation as user CIDRs, and a preset SHALL resolve to between 1 and 512 prefixes.

#### Scenario: CloudFront preset resolved
- **WHEN** a service declares `proxy: {trustedProxies: [cloudfront]}` and `tako deploy` runs with network access
- **THEN** the proxy node's global `trusted_proxies` contains exactly the current CloudFront origin-facing prefixes

#### Scenario: Mixed preset and CIDR
- **WHEN** a service declares `trustedProxies: [cloudflare, 10.0.0.0/8]`
- **THEN** the global set is the Cloudflare ranges plus `10.0.0.0/8`, deduplicated and sorted

#### Scenario: Unknown preset name
- **WHEN** a service declares `trustedProxies: [fastly]`
- **THEN** config validation fails, naming the supported presets

#### Scenario: Suspicious range response
- **WHEN** the fetched list is empty, has more than 512 prefixes, or has a prefix broader than /8 (IPv4) or /24 (IPv6)
- **THEN** the fetch is treated as failed

### Requirement: Preset caching and failure behavior
Tako SHALL cache resolved presets in the user cache directory.
- A cache younger than 24 hours SHALL be used without a network call.
- When a fetch fails, a cache younger than 30 days SHALL be used and a warning SHALL be emitted.
- When a fetch fails and no such cache exists, the deploy SHALL fail before any mutation.
- Tako SHALL NOT fall back to ranges embedded in the binary.

#### Scenario: Offline with recent cache
- **WHEN** the range URL is unreachable and the cached `cloudfront` snapshot is 3 days old
- **THEN** the deploy proceeds using the cached ranges
- **AND** emits a warning naming the preset and the cache age

#### Scenario: Offline without cache
- **WHEN** the range URL is unreachable and no cache exists
- **THEN** the deploy fails before writing any proxy configuration, and the error names the URL

#### Scenario: Forced refresh
- **WHEN** `tako deploy --refresh-trusted-proxies` runs with a fresh cache
- **THEN** the ranges are fetched again

### Requirement: Shared proxy nodes reconcile preset snapshots
On a proxy node shared by several projects or environments, routes SHALL conflict only when their declared trusted-proxy identities differ. A route's identity is its explicit CIDRs plus its preset names. For each preset, takod SHALL use the newest snapshot present on the node.

#### Scenario: Two projects with different snapshot ages
- **WHEN** project A deployed `trustedProxies: [cloudfront]` last month, and project B deploys the same preset today with an updated range list on the same node
- **THEN** rendering succeeds, and the global set uses B's newer snapshot

#### Scenario: Different declared identities still conflict
- **WHEN** one route declares `[cloudfront]` and another route on the same node declares an explicit CIDR list
- **THEN** rendering fails with the existing conflicting trusted proxy error

#### Scenario: CIDR-only configs unchanged
- **WHEN** no route on a node uses presets or request headers
- **THEN** the rendered Caddyfile is byte-identical to the one produced before this change

### Requirement: Fixed upstream request headers
`proxy.requestHeaders` SHALL set literal headers on requests forwarded to the service, overriding Caddy's default `X-Forwarded-Host` and `X-Forwarded-Proto` for that service only. Tako SHALL validate these headers both in the CLI and in takod:
- at most 32 entries;
- valid header names;
- no duplicate names after canonicalization;
- values of 1 to 1024 visible ASCII characters, with no control characters, `{`, `}`, `"` or `\`.

#### Scenario: Public host for CloudFront origin
- **WHEN** a service declares `proxy: {requestHeaders: {X-Forwarded-Host: www.example.com}}`
- **THEN** the upstream request carries `X-Forwarded-Host: www.example.com`, whatever the client or CDN sent

#### Scenario: Placeholder rejected
- **WHEN** a value contains `{http.request.host}`
- **THEN** validation fails, stating that values must be literal

#### Scenario: Older takod
- **WHEN** a route uses `requestHeaders` and a proxy node's takod lacks `proxy.request-headers-v1`
- **THEN** the deploy fails in preflight with an upgrade hint, and nothing is mutated

### Requirement: Protected request headers cannot be overridden
`proxy.requestHeaders` SHALL reject the following names, matched case-insensitively:
- hop-by-hop headers: `Connection`, `Keep-Alive`, `Proxy-Connection`, `Proxy-Authenticate`, `Proxy-Authorization`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`;
- framing headers: `Content-Length`, `Host`;
- client-identity headers: `X-Forwarded-For`, `Forwarded`, `X-Real-IP`.

#### Scenario: Client IP header rejected
- **WHEN** a service declares `requestHeaders: {x-forwarded-for: 1.2.3.4}`
- **THEN** validation fails, explaining that client-identity headers are managed by the proxy

#### Scenario: Override combined with trusted proxies warns
- **WHEN** a service sets both `trustedProxies` and `requestHeaders.X-Forwarded-Host`
- **THEN** validation succeeds with a warning that the fixed value replaces the CDN-supplied header
