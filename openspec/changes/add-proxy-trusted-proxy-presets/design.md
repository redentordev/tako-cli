# Design

## Context

See proposal.md for motivation and specs/proxy-routing/spec.md for the behavior contract. Observed current state (pre-change citations):

- **Config.** `ProxyConfig` is per service (`pkg/config/types.go:378`, `:554-613`), with `CDN` (`:572`, values `cloudflare`/`generic`, `:80-81`), `AllowIps` (`:593`), `TrustedProxies []string` (`:597`), `ResponseHeaders map[string]string` (`:612`). `validateProxyAccessControls` (`validator.go:2238`) parses trusted proxies with `netip.ParsePrefix`, masks, dedupes, sorts (`:2269-2286`), and rejects prefixes broader than /8 (IPv4) or /24 (IPv6) via `trustedProxyPrefixTooBroad` (`:2398`). `validateProxyRoutePolicy` (`:2293`) validates `responseHeaders`: at most 64, name pattern `proxyHeaderNamePattern` (`:2233`), no surrounding whitespace, forbidden hop-by-hop/framing list (`:2327`).
- **Shipping.** The CLI renders a JSON route manifest per project/env (`renderTakodProxyDynamicConfigForNodeWithOptions`, `pkg/deployer/takod_proxy.go:460`, fields copied at `:537-553`, marshalled at `:591`) and writes it with `PUT /v1/proxy-file` (`:1016`) as `runtimeid.ProxyConfigFileName(project, env)`. Capability gating: `proxyServicesUseTrustedProxies` (`:609`), `proxyServicesUseRoutePolicy` (`:618`), `takodProxyCapabilityRequirements` (`:669`), preflight (`:829`); constants `CapabilityProxyRoutePolicyV1` (`pkg/takod/server.go:149`), `CapabilityProxyTrustedProxiesV1` (`:190`).
- **Rendering on the node.** takod merges every manifest in `/etc/tako/proxy/routes` (`pkg/takod/proxy_file.go:15`) into one **Caddyfile** (`renderCaddyfileWithCertificatesAndOwners`, `proxy_routes.go:468`). `trusted_proxies static <cidrs>` plus `trusted_proxies_strict` is emitted once in the global `servers {}` block (`:503-512`). `proxyTrustedProxySet` (`:1223`) requires every route that sets trusted proxies to have an identical sorted set, otherwise rendering fails with "conflicting trusted proxy CIDR sets" (documented at `docs/CONFIGURATION.md:1038`). takod re-validates with `isSafeTrustedProxyPrefix` (`:1209`, used at `:315`) and its own header rules (`:1072-1095`). `writeCaddyReverseProxy` (`:1013`) emits upstreams, health checks, `health_headers { Host }`, `lb_policy cookie` — no `header_up`.
- **X-Forwarded-\*.** Tako does not touch them. Caddy's `reverse_proxy` sets `X-Forwarded-For/Proto/Host`; from untrusted peers it overwrites incoming values, from trusted peers it keeps/appends them. `header_up` directives are applied after these defaults, so they override them.
- **Fetch/caching precedent.** None for CIDRs. `pkg/updater/updater.go:37` uses a 10s HTTP client and a timestamp file; `pkg/httputil/client.go` has `NewClientWithTimeout`.

## Goals / Non-Goals

**Goals:**
- `trustedProxies: [cloudfront]` works on a shared proxy node next to other projects using the same preset, without "conflicting CIDR sets" after a range update.
- `requestHeaders` gives a fixed, literal `X-Forwarded-Host` (or any safe custom header) per service.
- Configs without the new features produce byte-identical Caddyfiles and need no new capability.

**Non-Goals:**
- Making trusted proxies per route (Caddy limitation).
- Fetching ranges on nodes.

## Decisions

### D1. Resolve presets on the deploying machine at deploy time
The CLI already has network access to the internet during deploys (builds, updater), whereas nodes may be egress-restricted. Resolution happens during plan/preflight, before mutation.

Preset table (constants in `pkg/proxypresets`):

| preset | URLs | selection | expected size |
|---|---|---|---|
| `cloudfront` | `https://ip-ranges.amazonaws.com/ip-ranges.json` | `prefixes[].ip_prefix` and `ipv6_prefixes[].ipv6_prefix` where `service == "CLOUDFRONT_ORIGIN_FACING"` | ~55 IPv4 |
| `cloudflare` | `https://www.cloudflare.com/ips-v4`, `https://www.cloudflare.com/ips-v6` | one CIDR per line | ~15 IPv4 + ~7 IPv6 |

Fetch rules: HTTPS only, 10s timeout per URL, response body capped at 4 MiB, `User-Agent: tako-cli/<version>`. Parsed CIDRs go through the same `netip.ParsePrefix` + mask + `trustedProxyPrefixTooBroad` checks as user CIDRs. A preset must yield between 1 and 512 prefixes; anything else is treated as a failed fetch (protects against an empty or hijacked response widening trust). `CLOUDFRONT_ORIGIN_FACING` is used, not `CLOUDFRONT`, because only origin-facing addresses connect to origins.

### D2. Cache and failure modes
Cache file: `<os.UserCacheDir()>/tako/trusted-proxies/<preset>.json` with `{preset, fetchedAt, sourceFingerprint (sha256 of raw bodies), syncToken (AWS only), cidrs}`, written atomically (temp + rename, 0644).
- Fresh cache (< 24h): used without a network call.
- Stale or missing: fetch. On success, write the cache.
- Fetch fails and the cache is younger than 30 days: use the cache and emit a `warning` event naming the preset and its age.
- Fetch fails and there is no cache, or the cache is older than 30 days: **fail the deploy** before mutation (`ConnectivityError`, exit 1), with the URL and a hint to retry or to list CIDRs explicitly.
- `--refresh-trusted-proxies` forces a fetch, ignoring the 24h freshness.
- No ranges are embedded in the binary: a stale embedded list would silently drop new CDN edges, which is the failure the feature exists to avoid.

Failing closed is safe here. Without trusted proxies Caddy overwrites `X-Forwarded-*`, and `allowIps` with `client_ip` would see CDN addresses, so the user would get a broken app rather than a security hole. The deploy is stopped so the user notices instead of shipping a degraded route.

### D3. Manifest shape and the shared-node rule
`ProxyRoute` gains `trustedProxyPresets []{name, fetchedAt, sourceFingerprint, cidrs}`. `trustedProxies` stays the explicit CIDR list only, so older takod versions never see preset names. The new capability `proxy.trusted-proxy-presets-v1` is required whenever `trustedProxyPresets` is non-empty.

The new `proxyTrustedProxySet` rule:
1. Each route's *declared identity* is its sorted explicit CIDRs plus its sorted preset names. Routes with different identities conflict, exactly as today.
2. For each preset name, takod uses the snapshot with the newest `fetchedAt` among all routes on the node. A tie is broken by `sourceFingerprint`.
3. The global set is the explicit CIDRs plus the chosen snapshots, deduped and sorted.

The result: a newer deploy of project B refreshes CloudFront ranges for project A on the same node, and A's next render does not regress them, because the newest snapshot always wins. takod re-validates every snapshot CIDR (`isSafeTrustedProxyPrefix`) and the 512-prefix cap.

### D4. requestHeaders
- Config: `proxy.requestHeaders map[string]string`, at most 32 entries.
- Names must match `proxyHeaderNamePattern` and are canonicalized with `textproto.CanonicalMIMEHeaderKey`. Duplicates after canonicalization are an error.
- Forbidden names, case-insensitive:
  - hop-by-hop: `Connection`, `Keep-Alive`, `Proxy-Connection`, `Proxy-Authenticate`, `Proxy-Authorization`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`;
  - framing: `Content-Length`, `Host`;
  - client identity: `X-Forwarded-For`, `Forwarded`, `X-Real-IP`.

  `Host` is excluded because it changes upstream routing and health semantics. Client-identity headers are excluded because a fixed value would mask the real client IP from the app and from logs.
- Values: 1-1024 bytes of visible ASCII and spaces; no control characters, no surrounding whitespace, no `{` or `}` (Caddy placeholders), no `"` or `\`. Values are emitted quoted, e.g. `header_up X-Forwarded-Host "www.example.com"`.
- Rendering: inside `reverse_proxy` in `writeCaddyReverseProxy`, sorted by name. An explicit `X-Forwarded-Host` or `X-Forwarded-Proto` overrides Caddy's default or the trusted value for this route only.
- Validation is mirrored in takod (`validateProxyRoutePolicy`) so a hand-crafted manifest cannot inject Caddyfile syntax.
- Capability: `proxy.request-headers-v1`, required only when some route sets `requestHeaders`.
- Warning: setting `X-Forwarded-Host` together with `trustedProxies` emits a config warning. With both set, the fixed header wins and the CDN-sent value is ignored. This is legitimate, but users should know.

### D5. `cdn: cloudflare` stays independent
`cdn` keeps its current domain/DNS policy meaning (`pkg/engine/domains.go:195-222`). It does not imply `trustedProxies: [cloudflare]`, because auto-trusting would silently change header semantics for existing configs. The existing doctor warning about `basicAuth`/`allowIps` without trusted proxies (`domains.go:102,222`, `cmd/doctor.go:247`) suggests the matching preset.

## Risks / Trade-offs

- **A provider changes its JSON or URL.** Strict parsing plus the 1-512 prefix bounds turn the change into a failed fetch. The cache keeps deploys working for 30 days while a CLI fix ships.
- **A compromised or MITM'd range source widens trust.** Mitigated by HTTPS with system roots, the /8 and /24 breadth limits, and the 512-prefix cap. Residual risk: an attacker in the range list can spoof `X-Forwarded-*`. This is inherent to trusting a CDN.
- **Ranges drift between deploys.** Accepted. `tako doctor` reports node snapshot age (manifests carry `fetchedAt`).
- **Mixed fleet: an older takod receives preset fields.** The capability preflight fails the deploy with an upgrade hint. Explicit CIDRs keep working on old takod.
- **`requestHeaders` used to impersonate auth headers to the app**, e.g. `X-User: admin`. The operator controls the config, so this is a deliberate setting, not an injection. The docs warn that these headers are sent on every request.

## Migration Plan

This is additive, and no state migration is needed. Users who hard-coded CloudFront CIDRs can replace them with `cloudfront`. On a shared node, every route must switch at the same time, or project the explicit list consistently, because rule D3.1 treats the preset and its CIDRs as different declared identities. The docs call this out. Rollback: an older CLI rejects preset names as invalid CIDRs, which fails closed.

## Open Questions

1. **Ship presets, `requestHeaders`, or both?**
   - Recommended: both, in order. `requestHeaders` first (no network, small), then presets.
   - `requestHeaders` alone already fixes #116 for CloudFront: set `X-Forwarded-Host` to the public host.
   - Presets fix the general case where the app needs the real client IP.
2. **Fail the deploy or use the last-known ranges?** Recommended: last-known for up to 30 days with a warning, then fail (D2). Never fall back to an embedded list.
3. **Should a preset on one route be merged with CIDR-only routes on the same node?** Recommended: no. Keep today's identical-identity rule (D3.1) and relax only snapshot age. A union would let one project widen trust for another project's routes.
4. **Allow `{http.request.host}`-style placeholders in values?** Recommended: not in v1. Literal values cover the reported case and keep injection analysis trivial.
5. **Cache location: user cache dir or project `.tako/`?** Recommended: the user cache dir. Ranges are global, and `.tako/` is project state that CI often discards.
