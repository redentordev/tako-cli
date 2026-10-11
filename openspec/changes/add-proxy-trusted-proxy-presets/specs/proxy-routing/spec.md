# Spec Delta

## Purpose

Defines how Tako's per-service proxy decides which peers are trusted to supply forwarded headers, how CDN range presets are resolved and shared on a proxy node, and which fixed request headers may be set on upstream requests.

## ADDED Requirements

### Requirement: Trusted proxy presets
`proxy.trustedProxies` SHALL accept the preset names `cloudfront` and `cloudflare` in addition to CIDRs. Tako SHALL resolve:
- `cloudfront` to the `CLOUDFRONT_ORIGIN_FACING` prefixes from AWS `ip-ranges.json`;
- `cloudflare` to Cloudflare's published `ips-v4` and `ips-v6` lists.

Resolved prefixes SHALL pass the same validation as user CIDRs. A preset SHALL resolve to between 1 and 512 prefixes.

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
- **WHEN** the fetched list is empty, has more than 512 prefixes, has a prefix broader than /8 (IPv4) or /24 (IPv6), or exceeds 16 MiB
- **THEN** the fetch is treated as failed

### Requirement: Every route-manifest writer resolves presets before mutation
Every operation that writes a proxy route manifest SHALL resolve the trusted-proxy presets of its services before mutating anything. Tako SHALL NOT write a manifest for a preset-declaring service without that service's resolved snapshot.

#### Scenario: Rollback resolves presets
- **WHEN** a service declares `trustedProxies: [cloudfront]` and the operator runs `tako rollback`
- **THEN** the rewritten manifest carries a `cloudfront` snapshot, resolved before any mutation

#### Scenario: Resolution fails outside deploy
- **WHEN** preset resolution fails during `tako run` or a placement apply that would rewrite the proxy route manifest, and no usable cache exists
- **THEN** the operation fails before any mutation

### Requirement: Preset cache
Tako SHALL cache resolved presets in the user cache directory, with these rules:
- A valid cache younger than 24 hours SHALL be used without a network call.
- A cache entry that fails validation, or whose fetch time is more than 5 minutes in the future, SHALL be treated as missing.
- A snapshot taken from the cache SHALL keep its original fetch time.
- When the user cache directory is unavailable, Tako SHALL resolve without a cache.

#### Scenario: Fresh cache
- **WHEN** the cached `cloudflare` snapshot is 2 hours old
- **THEN** the deploy uses it without contacting the range URLs

#### Scenario: No cache directory
- **WHEN** the user cache directory cannot be determined and the range URL is reachable
- **THEN** the deploy resolves the preset from the network and proceeds

### Requirement: Preset fetch failures fail closed
When a fetch fails, a valid cache younger than 30 days SHALL be used, and a warning SHALL be emitted. When a fetch fails and no such cache exists, the operation SHALL fail before any mutation. Tako SHALL NOT fall back to ranges embedded in the binary.

#### Scenario: Offline with recent cache
- **WHEN** the range URL is unreachable and the cached `cloudfront` snapshot is 3 days old
- **THEN** the deploy proceeds using the cached ranges
- **AND** emits a warning naming the preset and the cache age

#### Scenario: Offline without cache
- **WHEN** the range URL is unreachable and no cache exists
- **THEN** the deploy fails before writing any proxy configuration, and the error names the URL

### Requirement: Shared proxy nodes reconcile preset snapshots
On a proxy node shared by several projects or environments, routes SHALL conflict only when their declared trusted-proxy identities differ. A route's identity is its explicit CIDRs plus its preset names. For each preset, takod SHALL use the snapshot with the newest fetch time present on the node. takod SHALL reject a snapshot whose fetch time is more than 5 minutes ahead of its own clock.

#### Scenario: Two projects with different snapshot ages
- **WHEN** project A deployed `trustedProxies: [cloudfront]` last month, and project B deploys the same preset today with an updated range list on the same node
- **THEN** rendering succeeds, and the global set uses B's newer snapshot

#### Scenario: Different declared identities still conflict
- **WHEN** one route declares `[cloudfront]` and another route on the same node declares an explicit CIDR list
- **THEN** rendering fails with the existing conflicting trusted proxy error

#### Scenario: Snapshot from the future
- **WHEN** a route manifest carries a preset snapshot fetched 1 hour after the node's current time
- **THEN** takod rejects the manifest, and the node's proxy configuration is unchanged

#### Scenario: Older takod
- **WHEN** a route uses a preset and a proxy node's takod lacks `proxy.trusted-proxy-presets-v1`
- **THEN** the operation fails in preflight with an upgrade hint, and nothing is mutated

#### Scenario: Unchanged configs render identically
- **WHEN** no route on a node uses presets or request headers
- **THEN** the rendered Caddyfile is byte-identical to the one produced before this change

### Requirement: Presets warn when the forwarded host is not pinned
When a service trusts a preset and does not set `X-Forwarded-Host` in `proxy.requestHeaders`, Tako SHALL emit a config warning. The warning SHALL state that every customer of that CDN can supply forwarded headers.

#### Scenario: Preset without a pinned host
- **WHEN** a service declares `trustedProxies: [cloudfront]` and no `requestHeaders`
- **THEN** validation succeeds with a warning recommending a pinned `X-Forwarded-Host`

#### Scenario: Preset with a pinned host
- **WHEN** a service declares `trustedProxies: [cloudfront]` and `requestHeaders: {X-Forwarded-Host: www.example.com}`
- **THEN** validation succeeds without that warning

### Requirement: Fixed upstream request headers
`proxy.requestHeaders` SHALL set literal headers on requests forwarded to the service, on public and internal proxies. These headers SHALL override Caddy's default `X-Forwarded-Host` and `X-Forwarded-Proto` for that service only. Tako SHALL validate them both in the CLI and in takod:
- at most 32 entries;
- header names of at most 128 characters;
- no duplicate names after canonicalization;
- values of 1 to 1024 visible ASCII characters, with no control characters, `{`, `}`, `"` or `\`.

#### Scenario: Public host for CloudFront origin
- **WHEN** a service declares `proxy: {requestHeaders: {X-Forwarded-Host: www.example.com}}`
- **THEN** the upstream request carries `X-Forwarded-Host: www.example.com`, whatever the client or CDN sent

#### Scenario: Placeholder rejected
- **WHEN** a value contains `{http.request.host}`
- **THEN** validation fails, stating that values must be literal

#### Scenario: Hand-crafted manifest rejected by takod
- **WHEN** a route manifest written directly to takod carries a request header value containing a newline or `"`
- **THEN** takod rejects the manifest, and the node's proxy configuration is unchanged

#### Scenario: Older takod
- **WHEN** a route uses `requestHeaders` and a proxy node's takod lacks `proxy.request-headers-v1`
- **THEN** the deploy fails in preflight with an upgrade hint, and nothing is mutated

### Requirement: Protected request headers cannot be overridden
`proxy.requestHeaders` SHALL reject these names, matched case-insensitively:
- hop-by-hop: `Connection`, `Keep-Alive`, `Proxy-Connection`, `Proxy-Authenticate`, `Proxy-Authorization`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`, `Http2-Settings`;
- framing: `Content-Length`, `Host`;
- client identity: `X-Forwarded-For`, `Forwarded`, `X-Real-IP`.

#### Scenario: Client IP header rejected
- **WHEN** a service declares `requestHeaders: {x-forwarded-for: 1.2.3.4}`
- **THEN** validation fails, explaining that client-identity headers are managed by the proxy

#### Scenario: Hop-by-hop header rejected
- **WHEN** a service declares `requestHeaders: {http2-settings: AAMAAABkAAQAAP__}`
- **THEN** validation fails, naming the header as hop-by-hop
