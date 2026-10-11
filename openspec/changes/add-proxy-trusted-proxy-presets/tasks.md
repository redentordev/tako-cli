# Tasks

## 1. requestHeaders (no network)

- [ ] 1.1 Add `RequestHeaders map[string]string` to `ProxyConfig` (`pkg/config/types.go:554-613`) and validate per D4 in `pkg/config/validator.go` (reuse `proxyHeaderNamePattern`, `:2233`). Reject on internal proxies like `cdn` (`validateInternalProxy`, `:2405`). Verify with table tests in `pkg/config/proxy_access_test.go`: allowed `X-Forwarded-Host`, forbidden names (case variants), placeholder/quote/control-char values, duplicate after canonicalization, more than 32 entries.
- [ ] 1.2 Add `RequestHeaders` to `takod.ProxyRoute` (`pkg/takod/proxy_routes.go:81-100`), mirror validation in takod `validateProxyRoutePolicy` (`:1072`), and render sorted quoted `header_up` lines in `writeCaddyReverseProxy` (`:1013`). Verify with `proxy_file_test.go` contains/forbidden cases and a golden Caddyfile; confirm configs without the field render byte-identical.
- [ ] 1.3 Add `CapabilityProxyRequestHeadersV1` (`pkg/takod/server.go`), advertise it, and require it in `takodProxyCapabilityRequirements` (`pkg/deployer/takod_proxy.go:669`) only when used. Verify with capability preflight tests (`takod_proxy_test.go:941-973` pattern).
- [ ] 1.4 Emit the `X-Forwarded-Host` + `trustedProxies` warning. Verify with an engine/config warning test.

## 2. Preset resolution

- [ ] 2.1 Add `pkg/proxypresets` with the D1 table, strict parsers, prefix validation (shared with `validator.go:2269-2286`, `:2398`), and the 1-512 bound. Verify with parser tests on recorded fixtures of both sources, malformed JSON, empty lists, a too-broad prefix, and more than 512 prefixes.
- [ ] 2.2 Implement the D2 cache (atomic write, freshness 24h, stale limit 30d) with injectable clock, HTTP client and cache dir. Verify with tests: fresh cache with no network call, stale cache refetched, fetch failure with cache under 30d (warning), fetch failure with no cache (error), `--refresh-trusted-proxies`.
- [ ] 2.3 Accept preset names in `TrustedProxies` validation (config time: names only; CIDR resolution happens at deploy). Update `schema/tako.schema.json:1078` (items: CIDR pattern or enum). Verify with config and schema tests.

## 3. Shipping and node merge

- [ ] 3.1 Resolve presets in deploy preflight (before mutation) and populate `trustedProxyPresets` in the route manifest (`takod_proxy.go:537-553`). Explicit CIDRs remain in `trustedProxies`. Verify with deployer tests that the manifest carries provenance and that resolution failure aborts before any proxy-file write.
- [ ] 3.2 Add `CapabilityProxyTrustedProxyPresetsV1`, gate on non-empty presets. Verify with preflight tests.
- [ ] 3.3 Rewrite `proxyTrustedProxySet` (`proxy_routes.go:1223`) per D3: declared identity comparison, newest snapshot per preset, re-validation. Verify with tests in `proxy_access_test.go`: two projects same preset different snapshots (newest wins, no conflict), preset vs explicit CIDR list (conflict), CIDR-only routes unchanged (`:129` test still passes).

## 4. Doctor, docs, verification

- [ ] 4.1 `tako doctor`: report preset snapshot age per proxy node and warn after 30 days; suggest the matching preset in the existing trusted-proxy warning (`cmd/doctor.go:247`). Verify with doctor tests.
- [ ] 4.2 Docs: "Behind a CDN" recipe in `docs/CONFIGURATION.md` (~:1020-1094) covering CloudFront (`requestHeaders` X-Forwarded-Host and/or `trustedProxies: [cloudfront]`) and Cloudflare; update `docs/ORCHESTRATION-MODEL.md:770-782` for the shared-node rule. Verify examples with `tako config validate`.
- [ ] 4.3 Run `go build ./...` and `go test ./...`.
