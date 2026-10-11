# Tasks

## 1. PR1: requestHeaders (no network; fixes #116)

- [x] 1.1 Add `RequestHeaders map[string]string` to `ProxyConfig` (`pkg/config/types.go:554-613`). Validate it per D4 in `pkg/config/validator.go`, sharing the name pattern, the 128-character limit and the hop-by-hop/framing list with `responseHeaders` (`:2324-2351`), and adding the client-identity names. Allow it on internal proxies. Verify with table tests in `pkg/config/proxy_access_test.go`:
  - allowed `X-Forwarded-Host`, on both a public and an internal proxy;
  - forbidden names, including case variants and `Http2-Settings`;
  - values with placeholders, quotes, backslashes or control characters;
  - a duplicate after canonicalization;
  - more than 32 entries.
- [x] 1.2 Add `RequestHeaders` to `takod.ProxyRoute` (`pkg/takod/proxy_routes.go:81-100`). Mirror the validation in takod `validateProxyRoutePolicy` (`:1072`), using the same lists. Render sorted `header_up` lines quoted with `strconv.Quote` in `writeCaddyReverseProxy` (`:1013`). Verify with:
  - `proxy_file_test.go` contains and forbidden cases;
  - a golden Caddyfile;
  - a hand-crafted manifest containing `{`, `"` or a newline, which is rejected;
  - configs without the field rendering byte-identical.
- [x] 1.3 Add `CapabilityProxyRequestHeadersV1` (`pkg/takod/server.go`), advertise it, and require it in `takodProxyCapabilityRequirements` (`pkg/deployer/takod_proxy.go:669`) only when used. Do not bump the manifest version (D4). Verify with capability preflight tests (`takod_proxy_test.go:941-973` pattern), including an old-takod case that fails before mutation.
- [x] 1.4 Update `schema/tako.schema.json` (`:1121` area) for `requestHeaders`. Verify with the schema tests.
- [x] 1.5 Docs: add a "Behind a CDN" section to `docs/CONFIGURATION.md` (~:1020-1094) with the CloudFront recipe (`requestHeaders: {X-Forwarded-Host: <public host>}`) and a note that these headers are sent on every request. Verify the example with `tako config validate`.

## 2. PR2: Preset resolution

- [ ] 2.1 Add `pkg/proxypresets` with the D1 table, strict parsers, prefix validation shared with `validator.go:2269-2286` and `:2398`, the 1-512 bound, and a 16 MiB body cap. Verify with parser tests on recorded fixtures of both sources, plus:
  - malformed JSON;
  - empty lists;
  - a too-broad prefix;
  - more than 512 prefixes;
  - an oversize body.
- [ ] 2.2 Implement the D2 cache (atomic write, 24h freshness, 30-day stale limit) with an injectable clock, HTTP client and cache dir. Verify with tests:
  - a fresh cache makes no network call;
  - a stale cache is refetched;
  - a fetch failure with a cache under 30 days warns;
  - a fetch failure with no cache errors;
  - a corrupt cache entry, or one with `fetchedAt` more than 5 minutes ahead, is treated as missing;
  - an unavailable or unwritable cache dir still resolves through a fetch and warns on write failure;
  - a stale-cache snapshot keeps its original `fetchedAt`.
- [ ] 2.3 Accept preset names in `TrustedProxies` validation. At config time only names are checked; CIDRs are resolved in preflight. Update `schema/tako.schema.json:1078` (items: CIDR pattern or enum). Verify with config and schema tests, including the unknown-name error listing the supported presets.

## 3. PR2: Shipping and node merge

- [ ] 3.1 Resolve presets inside `PreflightTakodProxyCapabilities` (`pkg/deployer/takod_proxy.go:59-99`) before the capability checks, and memoize the snapshots on the `Deployer`. Populate `trustedProxyPresets` in the route manifest (`:537-553`), keeping explicit CIDRs in `trustedProxies`. Rendering a preset-declaring service without a memoized snapshot returns an error. Verify with deployer tests:
  - the manifest carries provenance;
  - a resolution failure aborts before any proxy-file write;
  - the deploy, `run`, project rollback, placement apply and `reconcileTakodProxyWithOptions` paths all resolve before writing;
  - rendering without preflight errors.
- [ ] 3.2 Add `CapabilityProxyTrustedProxyPresetsV1`, gated on non-empty presets, with no manifest version bump (D3). Verify with preflight tests.
- [ ] 3.3 Rewrite `proxyTrustedProxySet` (`proxy_routes.go:1223`) per D3: declared-identity comparison, newest snapshot per preset with the fingerprint tie-break, and write-time validation of snapshots (prefix safety, cap, known name, `fetchedAt` no more than 5 minutes ahead). Verify with tests in `proxy_access_test.go`:
  - two projects with the same preset and different snapshots: the newest wins and there is no conflict;
  - a preset versus an explicit CIDR list conflicts;
  - a future `fetchedAt` is rejected;
  - CIDR-only routes are unchanged (the `:129` test still passes).
- [ ] 3.4 Emit the D4 config warning for a service that trusts a preset without pinning `X-Forwarded-Host` in `requestHeaders`. Verify with config warning tests: a warning for a preset without a pin, none with a pin, and none for explicit CIDRs only.

## 4. PR2: Doctor and docs

- [ ] 4.1 `tako doctor`: report each configured preset's local cache age and warn after 30 days, and suggest the matching preset in the existing trusted-proxy warning (`cmd/doctor.go:247`). Verify with doctor tests.
- [ ] 4.2 Docs: extend "Behind a CDN" in `docs/CONFIGURATION.md` with Cloudflare, `trustedProxies: [cloudfront]`, the every-CDN-customer warning, the cache location, and deleting the cache file to force a refresh. Update `docs/ORCHESTRATION-MODEL.md:770-782` for the shared-node rule. Verify the examples with `tako config validate`.

## 5. Verification

- [ ] 5.1 For each PR, run `go build ./...` and `go test ./...`. Compare failures against master, because the Windows/WSL baseline has known failures.
- [ ] 5.2 Manual check after PR1: behind a CloudFront distribution, confirm the app receives the pinned `X-Forwarded-Host` and that a same-origin POST succeeds.
