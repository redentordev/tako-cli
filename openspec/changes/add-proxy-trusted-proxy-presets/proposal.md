# Proposal

## Why

Behind CloudFront, the origin request carries the origin hostname in `Host` (CloudFront needs it for TLS to the origin), so the app sees `http://origin.example.com` as its own URL. Frameworks that compare `Origin` with the request URL (Astro `checkOrigin`, CSRF middleware) then reject the site's own POSTs with 403 (issue #116). The usual fix is a CDN-sent `X-Forwarded-Host`. Caddy only keeps incoming `X-Forwarded-*` from trusted proxies, and `proxy.trustedProxies` accepts CIDRs only (`pkg/config/validator.go:2269-2286`). CloudFront's origin-facing ranges are about 55 prefixes that change over time, so hard-coding them in `tako.yaml` is brittle. There is also no way to set a fixed request header towards the upstream: `writeCaddyReverseProxy` (`pkg/takod/proxy_routes.go:1013`) emits no `header_up`.

## What Changes

- **Presets in `proxy.trustedProxies`.** Entries may be the names `cloudfront` or `cloudflare` as well as CIDRs, e.g. `trustedProxies: [cloudfront, 10.0.0.0/8]`.
  - The CLI resolves presets on the deploying machine at deploy time:
    - `cloudfront` uses `https://ip-ranges.amazonaws.com/ip-ranges.json`, with the prefixes whose `service` is `CLOUDFRONT_ORIGIN_FACING` (IPv4 and IPv6).
    - `cloudflare` uses `https://www.cloudflare.com/ips-v4` and `https://www.cloudflare.com/ips-v6`.
  - Fetched ranges are validated with the existing trusted-proxy rules and cached in the user cache directory.
  - When a fetch fails, a cached copy up to 30 days old is used, with a warning. With no usable cache the deploy fails before any mutation.
  - The route manifest sent to takod carries the resolved CIDRs plus preset provenance (name, source fingerprint, fetch time).
- **Shared-proxy conflict rule for presets.** Today every route on a node must declare an identical trusted-proxy set (`proxyTrustedProxySet`, `proxy_routes.go:1223`). Under this change, routes whose declared sets differ only in preset snapshot age are reconciled: takod uses the newest snapshot of each preset. Routes with different declared sets (different preset names or explicit CIDRs) still conflict, as today.
- **`proxy.requestHeaders`.** A per-service map of literal request headers set on the upstream request (`header_up`), e.g. `requestHeaders: {X-Forwarded-Host: www.example.com}`.
  - Names and values are validated in the CLI and again in takod.
  - Hop-by-hop, framing, `Host`, `X-Forwarded-For`, `Forwarded` and `X-Real-IP` cannot be set.
  - Values are literal: Caddy placeholders and control characters are rejected.
  - The headers override Caddy's default `X-Forwarded-Host`/`X-Forwarded-Proto` for that route only.
- **Capabilities.** New takod capabilities `proxy.request-headers-v1` and `proxy.trusted-proxy-presets-v1` are required only when a config uses the new features, through the existing capability preflight.
- **Docs.** `docs/CONFIGURATION.md` gains a "Behind a CDN" recipe covering CloudFront and Cloudflare, and `schema/tako.schema.json` is updated.
- No **BREAKING** changes. CIDR-only configs render byte-identical Caddyfiles.

## Out of Scope

- Other CDN presets (Fastly, Akamai, Bunny) and user-defined preset URLs. The adapter table makes these easy to add later.
- Refreshing ranges on nodes between deploys (a takod timer). Ranges change only when someone deploys; `tako doctor` warns when a node's snapshot is older than 30 days.
- Header removal (`-Header`), placeholders in values, and response-side `header_down` (`responseHeaders` already exists).
- Per-route trusted proxies. Caddy's `trusted_proxies` is a server-global option and stays global.

## Capabilities

### New Capabilities
- `proxy-routing`: per-service proxy trust and upstream request-header behavior, including preset resolution, caching, shared-node reconciliation and header safety rules.

### Modified Capabilities
- None. No existing main spec covers the proxy.

## Impact

- **Config:**
  - `pkg/config/types.go:554-613`: `ProxyConfig.RequestHeaders`. `TrustedProxies` stays `[]string` and now accepts preset names.
  - `pkg/config/validator.go:2238-2330`: preset names and requestHeaders rules, reusing `proxyHeaderNamePattern` (`:2233`).
  - `schema/tako.schema.json:1078`, `:1121`.
- **Resolution:** a new `pkg/proxypresets` package (fetch, parse, validate, cache). It uses `pkg/httputil/client.go` and `os.UserCacheDir()`.
- **Deployer:** `pkg/deployer/takod_proxy.go:460-591` (route fields, preset provenance), `:609-669` (capability requirements).
- **takod:**
  - `pkg/takod/proxy_routes.go:81-100` (`ProxyRoute` fields), `:204-315` (validation), `:1013` (`header_up`), `:1072-1095` (header rules), `:1223` (preset-aware merge).
  - `pkg/takod/server.go:149,190,~2636`: new capabilities.
- **Engine/doctor:** `pkg/engine/domains.go:195-222`; `cmd/doctor.go:247` (snapshot age warning).
- **Tests and docs:** see tasks.md.
