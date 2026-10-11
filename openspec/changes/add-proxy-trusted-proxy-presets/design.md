# Design

## Context

See proposal.md for motivation and specs/proxy-routing/spec.md for the behavior contract. Observed current state (pre-change citations):

- **Config.** `ProxyConfig` is per service (`pkg/config/types.go:378`, `:554-613`), with `CDN` (`:572`, values `cloudflare`/`generic`, `:80-81`), `AllowIps` (`:593`), `TrustedProxies []string` (`:597`), `ResponseHeaders map[string]string` (`:612`). `validateProxyAccessControls` (`validator.go:2238`) parses trusted proxies with `netip.ParsePrefix`, masks, dedupes, sorts (`:2269-2286`), and rejects prefixes broader than /8 (IPv4) or /24 (IPv6) via `trustedProxyPrefixTooBroad` (`:2398`). `validateProxyRoutePolicy` (`:2293`) validates `responseHeaders`: at most 64, names up to 128 characters matching `proxyHeaderNamePattern` (`:2233`), no surrounding whitespace, forbidden hop-by-hop/framing list including `Http2-Settings` (`:2327-2331`). `validateInternalProxy` (`:2405`) rejects public-only fields (`cdn`, domains, TLS) but allows route policy such as `responseHeaders`.
- **Shipping.** The CLI renders a JSON route manifest per project/env (`renderTakodProxyDynamicConfigForNodeWithOptions`, `pkg/deployer/takod_proxy.go:460`, fields copied at `:537-553`, marshalled at `:591`) and writes it with `PUT /v1/proxy-file` as `runtimeid.ProxyConfigFileName(project, env)`. Capability gating: `proxyServicesUseTrustedProxies` (`:609`), `proxyServicesUseRoutePolicy` (`:618`), `takodProxyCapabilityRequirements` (`:669`). `PreflightTakodProxyCapabilities` (`:59-99`) runs before mutation in every route-manifest writer: deploy (`pkg/engine/deploy.go:639`), `run` (`run.go:248`), project rollback (`rollback_project.go:449`), placement apply (`placement_apply.go:139`), `preflightAndSetupTakodRuntime` (`takod.go:196`) and `reconcileTakodProxyWithOptions` (`takod_proxy.go:187`). Manifest versions (`proxyManifestVersionForFeatures`, `:598-607`; takod `proxy_routes.go:25-27`) were bumped only for `allowPathPrefixes`, whose omission by an old takod would widen access; `trustedProxies` and `responseHeaders` were gated by capability alone.
- **Rendering on the node.** takod merges every manifest in `/etc/tako/proxy/routes` (`pkg/takod/proxy_file.go:15`) into one **Caddyfile** (`renderCaddyfileWithCertificatesAndOwners`, `proxy_routes.go:468`). `trusted_proxies static <cidrs>` plus `trusted_proxies_strict` is emitted once in the global `servers {}` block (`:503-512`). `proxyTrustedProxySet` (`:1223`) requires every route that sets trusted proxies to have an identical sorted set, otherwise rendering fails with "conflicting trusted proxy CIDR sets" (documented at `docs/CONFIGURATION.md:1038`). takod re-validates with `isSafeTrustedProxyPrefix` (`:1209`, used at `:315`) and its own header rules (`:1072-1100`). `writeCaddyReverseProxy` (`:1013`) emits upstreams, health checks, `health_headers { Host }`, `lb_policy cookie` — no `header_up`. There is no endpoint that reads route manifests back from a node.
- **X-Forwarded-\*.** Tako does not touch them. Caddy's `reverse_proxy` sets `X-Forwarded-For/Proto/Host`; from untrusted peers it overwrites incoming values, from trusted peers it keeps/appends them. `header_up` directives are applied after these defaults, so they override them.
- **Fetch/caching precedent.** None for CIDRs. `pkg/updater/updater.go:37` uses a 10s HTTP client and a timestamp file; `pkg/httputil/client.go` has `NewClientWithTimeout`.

## Goals / Non-Goals

**Goals:**
- `requestHeaders: {X-Forwarded-Host: www.example.com}` fixes #116 for CloudFront without any network dependency.
- `trustedProxies: [cloudfront]` works on a shared proxy node next to other projects using the same preset, without "conflicting CIDR sets" after a range update.
- Configs without the new features produce byte-identical Caddyfiles and need no new capability.

**Non-Goals:**
- Making trusted proxies per route (Caddy limitation).
- Fetching ranges on nodes.
- Verifying that a request came through the operator's own CDN distribution (for example a secret origin header). Users are warned instead (D4).

## Decisions

### D1. Resolve presets on the deploying machine, in proxy preflight
The CLI already has internet access during deploys (builds, updater), whereas nodes may be egress-restricted.

Resolution runs inside `PreflightTakodProxyCapabilities`, before the capability checks, so every route-manifest writer resolves presets before mutation. The resolved snapshots are memoized on the `Deployer` for the operation. Rendering a route whose service declares a preset without a memoized snapshot is an internal error, never a manifest without presets. Without this, a rollback, `run`, scale or placement apply could write a manifest that silently drops the CDN ranges, or that conflicts on the node under D3.1.

Preset table (constants in `pkg/proxypresets`):

| preset | URLs | selection | expected size |
|---|---|---|---|
| `cloudfront` | `https://ip-ranges.amazonaws.com/ip-ranges.json` | `prefixes[].ip_prefix` and `ipv6_prefixes[].ipv6_prefix` where `service == "CLOUDFRONT_ORIGIN_FACING"` | ~55 IPv4 |
| `cloudflare` | `https://www.cloudflare.com/ips-v4`, `https://www.cloudflare.com/ips-v6` | one CIDR per line | ~15 IPv4 + ~7 IPv6 |

Fetch rules:
- HTTPS only, with a 10s timeout per URL and `User-Agent: tako-cli/<version>`.
- Response bodies are capped at 16 MiB. AWS `ip-ranges.json` is already megabytes in size and grows with every region, so a 4 MiB cap would eventually turn routine growth into a failed fetch.
- Parsed CIDRs go through the same `netip.ParsePrefix`, mask and `trustedProxyPrefixTooBroad` checks as user CIDRs.
- A preset must yield between 1 and 512 prefixes. Anything else is treated as a failed fetch, which protects against an empty or hijacked response widening trust.
- `CLOUDFRONT_ORIGIN_FACING` is used, not `CLOUDFRONT`, because only origin-facing addresses connect to origins.

### D2. Cache and failure modes
Cache file: `<os.UserCacheDir()>/tako/trusted-proxies/<preset>.json` containing `{preset, fetchedAt, sourceFingerprint (sha256 of the raw bodies), syncToken (AWS only), cidrs}`. It is written atomically (temp file plus rename, mode 0644).

| Situation | Behavior |
|---|---|
| Cache younger than 24h | Used without a network call |
| Cache stale or missing | Fetch; on success, write the cache |
| Fetch fails, cache younger than 30 days | Use the cache and emit a `warning` event naming the preset and its age |
| Fetch fails, no cache or cache older than 30 days | Fail before mutation (`ConnectivityError`, exit 1), with the URL and a hint to retry or to list CIDRs explicitly |

More rules:
- A cache entry is read with the same validation as a fetched response. An entry that fails it, or whose `fetchedAt` is more than 5 minutes in the future, is treated as missing.
- If `os.UserCacheDir()` fails, or the cache cannot be written, Tako works without a cache: every resolution fetches, and a write failure is a warning. This is common in CI containers without `HOME`.
- A snapshot taken from the cache keeps its original `fetchedAt`, so an old cached snapshot never outranks a newer one on a shared node (D3).
- There is no `--refresh-trusted-proxies` flag. It would have to exist on every manifest-writing command, and the 24h freshness already bounds staleness. Deleting the cache file forces a fetch, as the docs explain.
- No ranges are embedded in the binary. A stale embedded list would silently drop new CDN edges, which is the failure this feature exists to avoid.

Failing closed is safe here. Without trusted proxies, Caddy overwrites `X-Forwarded-*`, and `allowIps` with `client_ip` sees CDN addresses, so the user gets a broken app rather than a security hole. The deploy is stopped so the user notices, instead of shipping a degraded route.

### D3. Manifest shape and the shared-node rule
`ProxyRoute` gains `trustedProxyPresets []{name, fetchedAt, sourceFingerprint, cidrs}`. `trustedProxies` stays the explicit CIDR list only, so older takod versions never see preset names. The new capability `proxy.trusted-proxy-presets-v1` is required whenever `trustedProxyPresets` is non-empty. The manifest version is not bumped: a takod that ignored the field would trust fewer peers, not more. This matches the `trustedProxies` and `responseHeaders` precedent; only `allowPathPrefixes`, whose omission widens access, needed a version.

The new `proxyTrustedProxySet` rule:
1. Each route's *declared identity* is its sorted explicit CIDRs plus its sorted preset names. Routes with different identities conflict, exactly as today.
2. For each preset name, takod uses the snapshot with the newest `fetchedAt` among all routes on the node. A tie is broken by the lexically greatest `sourceFingerprint`.
3. The global set is the explicit CIDRs plus the chosen snapshots, deduped and sorted.

takod validates each snapshot when the manifest is written:
- every CIDR passes `isSafeTrustedProxyPrefix`;
- the 512-prefix cap holds;
- the name is a known preset;
- `fetchedAt` is no more than 5 minutes ahead of the node's clock.

Without the last check, a deploying machine with a fast clock would pin its snapshot as "newest" on every shared node until real time caught up.

The result: a newer deploy of project B refreshes CloudFront ranges for project A on the same node, and A's next render does not regress them, because the newest snapshot always wins. Both routes declared the same preset, so A's trust semantics are unchanged.

### D4. requestHeaders
- Config: `proxy.requestHeaders map[string]string`, at most 32 entries. Allowed on public and internal proxies, like `responseHeaders`.
- Names: at most 128 characters, must match `proxyHeaderNamePattern`, and are canonicalized with `textproto.CanonicalMIMEHeaderKey`. Duplicates after canonicalization are an error.
- Forbidden names, case-insensitive:
  - hop-by-hop: `Connection`, `Keep-Alive`, `Proxy-Connection`, `Proxy-Authenticate`, `Proxy-Authorization`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`, `Http2-Settings`;
  - framing: `Content-Length`, `Host`;
  - client identity: `X-Forwarded-For`, `Forwarded`, `X-Real-IP`.

  The hop-by-hop and framing entries match the existing `responseHeaders` list. `Host` is excluded because it changes upstream routing and health semantics. Client-identity headers are excluded because a fixed value would mask the real client IP from the app and from logs.
- Values: 1-1024 bytes of visible ASCII and spaces; no control characters, no surrounding whitespace, no `{` or `}` (Caddy placeholders), no `"` or `\`. Values are emitted quoted with `strconv.Quote`, as response headers are, e.g. `header_up X-Forwarded-Host "www.example.com"`.
- Rendering: inside `reverse_proxy` in `writeCaddyReverseProxy`, sorted by name. An explicit `X-Forwarded-Host` or `X-Forwarded-Proto` overrides Caddy's default or the trusted value for this route only.
- Validation is mirrored in takod (`validateProxyRoutePolicy`) so a hand-crafted manifest cannot inject Caddyfile syntax.
- Capability: `proxy.request-headers-v1`, required only when some route sets `requestHeaders`. No manifest version bump: a takod that ignored the field would fall back to Caddy's defaults for that route, which is the behavior without the setting.
- Warning: a service that trusts a preset and does not pin `X-Forwarded-Host` in `requestHeaders` gets a config warning. Trusting `cloudfront` or `cloudflare` trusts every customer of that CDN, so anyone can point their own distribution at the origin and supply `X-Forwarded-Host` and `X-Forwarded-For`. The warning recommends pinning the host and, where client IP matters, restricting origin access. Pinning `X-Forwarded-Host` together with `trustedProxies` does not warn: it is the recommended combination.

### D5. `cdn: cloudflare` stays independent
`cdn` keeps its current domain/DNS policy meaning (`pkg/engine/domains.go:195-222`). It does not imply `trustedProxies: [cloudflare]`, because auto-trusting would silently change header semantics for existing configs. The existing doctor warning about `basicAuth`/`allowIps` without trusted proxies (`domains.go:102,222`, `cmd/doctor.go:247`) suggests the matching preset.

### D6. Doctor reports the local cache only
There is no takod endpoint that returns route manifests. Adding one only so that doctor can report snapshot age is not worth a new capability. `tako doctor` reports each configured preset's local cache age and warns after 30 days. The age of a node's snapshot is bounded by the last deploy, which used a snapshot no older than 30 days.

## Risks / Trade-offs

- **A provider changes its JSON or URL.** Strict parsing plus the 1-512 prefix bounds turn the change into a failed fetch. The cache keeps deploys working for 30 days while a CLI fix ships.
- **A compromised or MITM'd range source widens trust.** Mitigated by HTTPS with system roots, the /8 and /24 breadth limits, and the 512-prefix cap. Residual risk: an attacker in the range list can spoof `X-Forwarded-*`. This is inherent to trusting a CDN.
- **Any CDN customer is trusted.** A preset trusts the whole CDN, not the operator's distribution. Pinned `X-Forwarded-Host` closes the reported host problem; client IP remains spoofable by another customer of the same CDN. Mitigated by the D4 warning and the docs.
- **Ranges drift between deploys.** Accepted. Ranges on a node change only when some project deploys there.
- **Clock skew on the deploying machine.** A clock more than 5 minutes fast fails the deploy with a message naming the clock; a slow clock only makes a snapshot lose ties.
- **Mixed fleet: an older takod receives preset fields.** The capability preflight fails the deploy with an upgrade hint. Explicit CIDRs keep working on old takod.
- **Older CLI on a node where new routes use presets.** An older CLI writes manifests with explicit CIDRs only, and D3.1 still compares declared identities, so mixed identities conflict exactly as they do today.
- **`requestHeaders` used to impersonate auth headers to the app**, e.g. `X-User: admin`. The operator controls the config, so this is a deliberate setting, not an injection. The docs warn that these headers are sent on every request.

## Migration Plan

This is additive, and no state migration is needed. Users who hard-coded CloudFront CIDRs can replace them with `cloudfront`. On a shared node, every route must switch at the same time, or keep the explicit list consistently, because rule D3.1 treats the preset and its CIDRs as different declared identities. The docs call this out. Rollback: an older CLI rejects preset names as invalid CIDRs, which fails closed.

## Implementation Order

1. **PR1: `requestHeaders`** (config, schema, takod rendering and validation, capability `proxy.request-headers-v1`, CloudFront recipe using a pinned `X-Forwarded-Host`). This fixes #116 with no network dependency and can ship in a patch release.
2. **PR2: trusted-proxy presets** (`pkg/proxypresets`, cache, preflight resolution, manifest provenance, takod merge rule, capability `proxy.trusted-proxy-presets-v1`, D4 preset warning, doctor, docs for Cloudflare and client IP).

Each PR carries its own tests, schema and docs. PR2 depends on PR1 only for the warning, which checks `requestHeaders`.

## Resolved Questions

1. **Ship presets, `requestHeaders`, or both?** Both, as two PRs, with `requestHeaders` first. It needs no network, is small, and alone fixes #116 for CloudFront by setting `X-Forwarded-Host` to the public host. Presets fix the general client-IP case and carry more risk (network, cache, shared-node merge), so they ship separately.
2. **Fail the deploy or use the last-known ranges?** Last-known ranges for up to 30 days with a warning, then fail (D2). Never an embedded list. Failing closed yields a degraded app, not a trust widening. An embedded list would silently miss new edges.
3. **Should a preset on one route be merged with CIDR-only routes on the same node?** No. Keep today's identical-identity rule (D3.1) and relax only snapshot age. A union would let one project widen trust for another project's routes.
4. **Allow `{http.request.host}`-style placeholders in values?** No, not in v1. Literal values cover the reported case and keep injection analysis trivial. Placeholders can be added later behind a new capability.
5. **Cache location: user cache dir or project `.tako/`?** The user cache dir. Ranges are global, and `.tako/` is project state that CI often discards. When the user cache dir is unavailable, Tako runs without a cache instead of falling back to `.tako/` (D2).
6. **Where is resolution triggered?** In `PreflightTakodProxyCapabilities`, which every manifest writer already calls before mutation, with a hard error if rendering finds an unresolved preset (D1). Resolving only in deploy would let rollback, `run`, scale or placement apply write manifests without presets.
7. **Keep `--refresh-trusted-proxies`?** No. It would have to be added to every manifest-writing command, and the 24h freshness window already limits staleness. Deleting the cache file is the documented manual refresh.
8. **Response body cap?** 16 MiB instead of 4 MiB. `ip-ranges.json` is already megabytes in size and keeps growing, and hitting the cap means a failed fetch.
9. **Warn on `X-Forwarded-Host` plus `trustedProxies`?** No. That warning is removed: pinning the host behind a trusted CDN is the safe configuration. The warning moves to the risky case, a preset without a pinned host (D4), because a preset trusts every customer of that CDN.
10. **`requestHeaders` on internal proxies?** Allowed, consistent with `responseHeaders`. Internal routes have no CDN, but fixed upstream headers are still useful, for example for a tenant marker, and the same safety rules apply.
11. **Forbidden-name list.** It reuses the `responseHeaders` hop-by-hop and framing list, which adds `Http2-Settings`, and the same 128-character name limit, plus the client-identity names. One list per direction keeps the CLI and takod mirrors easy to keep identical.
12. **Bump the route manifest version?** No. Both new fields fail safe when an old takod ignores them. Capability preflight is the gate, as for `trustedProxies` and `responseHeaders` (D3, D4).
13. **Can a snapshot carry a future `fetchedAt`?** takod rejects snapshots more than 5 minutes ahead of its clock, and the CLI ignores such cache entries. Under newest-wins, a future timestamp would freeze a shared node's ranges.
14. **What does doctor report?** The local cache age only (D6). Reading node snapshots would need a new endpoint and capability for an informational check.

## Implementation Notes

Deviations and clarifications made while implementing, each chosen as the safest equivalent of the plan:

1. **Exit code for a failed preset resolution.** D2 says "`ConnectivityError`, exit 1", but `engine.ConnectivityError` classifies as exit 4 (node unreachable) and lives in `pkg/engine`, which `pkg/deployer` cannot import. Resolution failures return `proxypresets.ResolveError` (wrapped as `proxy preflight: ...`), which classifies as a failed operation (exit 1) as specified. The fetch cause is flattened into text so an HTTP client timeout is not misclassified as a cancelled operation.
2. **requestHeaders in replicated state.** Like `responseHeaders`, request-header values are not stored in the replicated desired state (`takodstate` keeps `proxyRequestHeaderNames` only). `tako config export` writes `REDACTED` values and emits the `proxy_request_header_values_redacted` warning.
3. **Validation command.** There is no `tako config validate`; the docs examples were verified with `tako validate`, and the snippets are covered by `docs/docs_snippets_test.go`.
4. **Capabilities for presets.** A preset-only route still trusts proxies, so it requires `proxy.trusted-proxies-v1` in addition to `proxy.trusted-proxy-presets-v1`. takod also switches the `allowIps` matcher to `client_ip` for routes that trust only presets.
5. **Preset name normalization.** Preset names are accepted case-insensitively, canonicalized to lowercase, and sorted together with the canonical CIDRs in `proxy.trustedProxies`. Names that look like words but are not supported presets get an "unknown preset" error listing `cloudflare, cloudfront`.
6. **Source fingerprint.** SHA-256 over each source URL, its body length, and its raw body, in the preset's URL order.
7. **No cache directory.** When `os.UserCacheDir()` fails, the resolver silently resolves without a cache; only cache write failures warn. `tako doctor` reports the missing cache directory in its new local "Trusted Proxy Presets" section.
8. **Coverage of manifest writers.** deploy, `run`, project rollback, placement apply, `scale` and `preflightAndSetupTakodRuntime` all call `PreflightTakodProxyCapabilities`, and `reconcileTakodProxyWithOptions` preflights before rendering, so resolution is tested at that shared entry point (deployer tests for preflight, reconcile and render-without-preflight) rather than per engine command.
9. **Doctor suggestion.** The existing trusted-proxy warning (doctor and domain checks) suggests `trustedProxies: [cloudflare]` for `cdn: cloudflare`, and a CIDR list or a preset otherwise.
