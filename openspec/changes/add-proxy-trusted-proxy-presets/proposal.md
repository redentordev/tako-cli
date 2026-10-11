# Proposal

## Why

Behind CloudFront, the origin request carries the origin hostname in `Host` (CloudFront needs it for TLS to the origin), so the app sees `http://origin.example.com` as its own URL. Frameworks that compare `Origin` with the request URL (Astro `checkOrigin`, CSRF middleware) then reject the site's own POSTs with 403 (issue #116).

The usual fix is a CDN-sent `X-Forwarded-Host`, but two things stand in the way:
- Caddy only keeps incoming `X-Forwarded-*` from trusted proxies, and `proxy.trustedProxies` accepts CIDRs only (`pkg/config/validator.go:2269-2286`). CloudFront's origin-facing ranges are about 55 prefixes that change over time, so hard-coding them in `tako.yaml` is brittle.
- There is no way to set a fixed request header towards the upstream: `writeCaddyReverseProxy` (`pkg/takod/proxy_routes.go:1013`) emits no `header_up`.

## What Changes

The change ships as two PRs (design.md "Implementation Order").

- **PR1: `proxy.requestHeaders` (fixes #116 without network access).** A per-service map of literal request headers set on the upstream request (`header_up`), e.g. `requestHeaders: {X-Forwarded-Host: www.example.com}`.
  - Names and values are validated in the CLI and again in takod.
  - Hop-by-hop, framing (including `Http2-Settings`), `Host`, `X-Forwarded-For`, `Forwarded` and `X-Real-IP` cannot be set. The hop-by-hop and framing list is the one `responseHeaders` already uses.
  - Values are literal: Caddy placeholders and control characters are rejected.
  - The headers override Caddy's default `X-Forwarded-Host`/`X-Forwarded-Proto` for that route only.
  - Allowed on public and internal proxies, like `responseHeaders`.
  - Requires the takod capability `proxy.request-headers-v1`, only when used.
- **PR2: presets in `proxy.trustedProxies`.** Entries may be the names `cloudfront` or `cloudflare` as well as CIDRs, e.g. `trustedProxies: [cloudfront, 10.0.0.0/8]`.
  - The CLI resolves presets on the deploying machine during proxy preflight, so every command that writes route manifests (deploy, rollback, `run`, scale, placement apply) resolves them before mutation:
    - `cloudfront` uses `https://ip-ranges.amazonaws.com/ip-ranges.json`, with the prefixes whose `service` is `CLOUDFRONT_ORIGIN_FACING` (IPv4 and IPv6).
    - `cloudflare` uses `https://www.cloudflare.com/ips-v4` and `https://www.cloudflare.com/ips-v6`.
  - Fetched ranges are validated with the existing trusted-proxy rules and cached in the user cache directory for 24 hours.
  - When a fetch fails, a cached copy up to 30 days old is used, with a warning. With no usable cache, the operation fails before any mutation.
  - The route manifest sent to takod carries the resolved CIDRs plus preset provenance (name, source fingerprint, fetch time).
  - A service that trusts a preset without pinning `X-Forwarded-Host` gets a warning, because a preset trusts every customer of that CDN.
  - Requires the takod capability `proxy.trusted-proxy-presets-v1`, only when used.
- **Shared-proxy conflict rule for presets (PR2).** Today every route on a node must declare an identical trusted-proxy set (`proxyTrustedProxySet`, `proxy_routes.go:1223`). Under this change, routes whose declared sets differ only in preset snapshot age are reconciled: takod uses the newest snapshot of each preset. Routes with different declared sets (different preset names or explicit CIDRs) still conflict, as today.
- **Docs.** `docs/CONFIGURATION.md` gains a "Behind a CDN" recipe: CloudFront in PR1, Cloudflare and presets in PR2. `schema/tako.schema.json` is updated in each PR.
- No **BREAKING** changes. Configs that use neither feature render byte-identical Caddyfiles.

## Out of Scope

- Other CDN presets (Fastly, Akamai, Bunny) and user-defined preset URLs. The adapter table makes these easy to add later.
- Refreshing ranges on nodes between deploys (a takod timer). Ranges on a node change only when some project deploys there. `tako doctor` reports the age of the local preset cache.
- Reading route manifests back from nodes (no such endpoint exists).
- A `--refresh-trusted-proxies` flag. Deleting the cache file forces a fetch.
- Header removal (`-Header`), placeholders in values, and response-side `header_down` (`responseHeaders` already exists).
- Per-route trusted proxies. Caddy's `trusted_proxies` is a server-global option and stays global.
- Verifying that requests come from the operator's own CDN distribution (for example a secret origin header).

## Capabilities

### New Capabilities
- `proxy-routing`: per-service proxy trust and upstream request-header behavior, including preset resolution, caching, shared-node reconciliation and header safety rules.

### Modified Capabilities
- None. No existing main spec covers the proxy.

## Impact

- **PR1 (requestHeaders):**
  - Config: `pkg/config/types.go:554-613` (`ProxyConfig.RequestHeaders`) and `pkg/config/validator.go:2293-2358` (rules shared with `responseHeaders`); `schema/tako.schema.json:1121`.
  - Deployer: `pkg/deployer/takod_proxy.go:537-553` (route field) and `:609-684` (capability requirement).
  - takod: `pkg/takod/proxy_routes.go:81-100` (`ProxyRoute`), `:1013` (`header_up`), `:1072-1100` (header rules); `pkg/takod/server.go` (capability).
- **PR2 (presets):**
  - Config: `TrustedProxies` stays `[]string` and accepts preset names (`validator.go:2238-2290`); `schema/tako.schema.json:1078`.
  - Resolution: a new `pkg/proxypresets` package (fetch, parse, validate, cache) using `pkg/httputil/client.go` and `os.UserCacheDir()`.
  - Deployer: `pkg/deployer/takod_proxy.go:59-99` (resolve in preflight and memoize), `:537-553` (preset provenance) and `:669` (capability).
  - takod: `pkg/takod/proxy_routes.go:204-315` (snapshot validation), `:1223` (preset-aware merge); `server.go` (capability).
  - Engine and doctor: config warning for presets without a pinned host; `cmd/doctor.go:247` (suggest the preset; local cache age).
- **Tests and docs:** see tasks.md.
