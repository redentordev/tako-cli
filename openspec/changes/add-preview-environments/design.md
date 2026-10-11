# Design

## Context

See proposal.md for motivation and specs/preview-environments/spec.md for the behavior contract. Observed facts that shape the approach:

- **Environment resolution happens on the document tree.** `LoadConfig` (`pkg/config/types.go:1250-1338`) expands `${VAR}` textually (`expandEnvWithTrim`, `:1196-1224`, which fails on any unset variable), then `ParseConfigDocument` (`:1347`) calls `resolveConfigDocument` (`pkg/config/environment_extends.go:290`) to merge `extends` chains and drop `abstract` environments, then strict-decodes and runs `ValidateConfig`. `extends`/`abstract` are stripped and never inherited. Abstract environments are pre-decoded strictly per declaration (D5 of `add-environment-extends`) but do not survive into `Config.Environments`.
- **Everything runtime is keyed by `(project, environment)`.** Container, alias, network, volume, router and proxy-file names come from `pkg/runtimeid/runtimeid.go` (`ContainerName` `:23`, `NetworkName` `:62`, `VolumeName` `:74`, `ProxyConfigFileName` `:131`), all hashed and length-bounded. Docker resources carry exact `tako.project` / `tako.environment` labels (`pkg/takod/reconcile.go:1015-1026` for volumes). Replicated state lives under `desired/<project>/<environment>/` (`pkg/takod/state.go:260-268`). Operation leases are per environment (`AcquireRemoteOperationLeasesContext(…, envName, …)`, `pkg/engine/deploy.go:416`). So a new environment name is already a fully isolated runtime namespace.
- **One exception: network cleanup matches by prefix.** `cleanupNetworks` (`pkg/takod/cleanup.go:255-285`) removes every network starting with `NetworkEnvironmentPrefix(project, env)` = `tako_<project>_<env>_` (`runtimeid.go:70-72`). Because `sanitizePart` maps `-` to `_`, environment `prod`'s prefix also matches `prod-eu`'s networks. Container cleanup uses exact labels (`cleanup.go:218`, `:238`).
- **Volumes are never removed.** `cleanupUnusedProjectVolumes` ignores its arguments and returns 0 (`cleanup.go:431`); `CleanupRequest` (`cleanup.go:15-35`) has no volume-removal field besides `ExternalVolumes`. `tako destroy` today leaves named volumes behind on purpose.
- **Destroy is config-driven.** `DestroySession.decommissionApp` asks takod to remove the services listed in the current config (`pkg/engine/destroy.go:437-453`), then `purgeProjectRuntime` prunes leftovers (`:457-474`).
- **Secrets are file-per-environment.** `secrets.Manager.loadSecrets` reads `.tako/secrets` then `.tako/secrets.<environment>` (`pkg/secrets/manager.go:83-115`); `NewManagerForConfig(cfg, environment)` (`pkg/secrets/sources.go:84`) adds `secretSources` resolved for that environment.
- **Domain checks are post-deploy and non-fatal by default** (`--skip-domain-check`, `--strict-domains`, `cmd/deploy.go:74-75`). tako-proxy issues one certificate per host via HTTP-01, or DNS-01 when the environment declares `proxy.acme` (`EnvironmentACMEConfig`, `types.go:808-812`).
- **Capability gating** goes through `RequireTakodCapabilityContext` (`pkg/engine/takod.go:238-253`), which returns `takod does not support <feature> (missing capability <cap>); upgrade the node agent`. Capabilities are advertised in `pkg/takod/server.go:2636`. Every takod route is listed once in `registeredRoutes` (`pkg/takod/lifecycle_gate.go:25-41`) so the lifecycle gate covers it.
- **`-e` handling** is `getEnvironmentName` (`cmd/root.go:353`) over the persistent `--env` flag (`:282`); `GetEnvironment` rejects abstract names (`types.go:900`).
- **Hook env** is built in `pkg/hooks/env.go:63-94` (`TAKO_ENVIRONMENT`, `TAKO_SERVICES`, …).
- **Sibling `add-deploy-summary`** reserves `summary.environment.kind` (`standard`) and per-URL `kind` for this change.

## Goals / Non-Goals

**Goals:**
- A preview is an ordinary environment to every downstream component (engine, deployer, takod, proxy, state, leases, history), so it inherits their behavior and tests for free.
- Safe by default: a preview can never write to the base environment's data, schedules, backups, host ports or domains.
- Works from CI with only `tako deploy --preview "$NAME"` on PR open/update and `tako preview destroy "$NAME" --yes` on PR close, both idempotent.
- No new long-running node behavior.

**Non-Goals:**
- Data cloning, DNS automation, wildcard certificates, a takod expiry sweeper (see proposal Out of Scope).

## Decisions

### D1. A preview is a synthesized child environment, resolved by the existing extends machinery
`LoadConfig` gains a sibling `LoadConfigWithOptions(path string, opts LoadOptions)` with `LoadOptions{Preview *PreviewInstance}` where `PreviewInstance{Base, Name string}`. `resolveConfigDocument` receives the option and, before chain resolution, injects a declared environment `preview-<name>` whose body is `{extends: <base>}`. The existing resolver then produces a flattened concrete environment from any base, abstract or not, with the full merge rules, provenance and error annotation. `LoadConfig(path)` is `LoadConfigWithOptions(path, LoadOptions{})` and keeps its exact behavior (including the fast path that returns the original bytes).

After typed decode, `applyPreviewPolicy(cfg, instance, policy)` in new `pkg/config/preview.go` mutates only `cfg.Environments["preview-<name>"]` (D4), then `ValidateConfig` runs over the whole config as usual, so a template that resolves to something invalid fails with the normal located errors, annotated with the chain `preview-pr-42 extends preview -> staging`.

The base's `preview:` block is captured from the declared (pre-merge) node during resolution and stored on `Config` (`cfg.previewInstance{Name, Base, Identity, Policy}`), exposed through `cfg.PreviewInstance()`. `preview` is stripped from every resolved environment and is not inherited (same handling as `abstract`), so exactly the environments that declare it are preview bases.

- Alternative rejected: deep-copy the typed `EnvironmentConfig` after decode. Abstract bases do not survive decode, and typed copies cannot express template overrides like `services: {worker: null}`.
- Alternative rejected: a `preview.overrides` sub-block. An abstract template environment that `extends` the real base already *is* the override document, with `null` removal and provenance; a second override mechanism would duplicate it.
- Placeholders such as `{name}` are Tako-owned, single-brace and expanded only inside the `preview:` block after decode. They cannot be `${…}` because `expandEnvWithTrim` fails the whole load on an unset variable, which would break every non-preview command.

### D2. Identity and name normalization
The user-facing name is normalized: lowercase; every run of characters outside `[a-z0-9]` becomes `-`; leading/trailing `-` trimmed; if longer than 40 characters it is cut to 33 and suffixed with `-` + the first 6 hex characters of SHA-256 of the original input. An empty result is exit 2. The CLI prints `preview name "Feature/Login_v2" normalized to "feature-login-v2"` on stderr when they differ. CI can therefore pass `$GITHUB_HEAD_REF` directly; `pr-<number>` is the documented recommendation.

The environment identity is `preview-<name>` (max 48 characters, satisfies the environment-name rule in `pkg/config/validator.go:125`). Loading fails with exit 2 when a declared environment already has that identity. Identities do not depend on the base, so `tako preview destroy pr-42` needs no `-e`.

Why the fixed `preview-` prefix: operators recognize preview containers in `docker ps` (`tako_app_preview_pr_42_web_1_<hash>`) and the takod listing can filter on it cheaply. The prefix collides by string with a declared environment named `preview` only through network-name prefixes, which D8 fixes.

### D3. Base selection
`tako deploy --preview <name>` picks the base as: `-e <env>` when given (abstract allowed, but it must declare `preview:`); otherwise the only environment declaring `preview:`; otherwise exit 2 listing the candidates (none → `no environment declares a preview: block; see docs/PREVIEW-ENVIRONMENTS.md`). `GetEnvironment` keeps rejecting abstract names for normal use; only the preview path passes an abstract base to the resolver. A deploy of an existing preview under a different base is exit 2 (`preview pr-42 was created from staging; destroy it first`), detected from the record (D7).

### D4. Policy transforms, applied in this order to the resolved instance
1. **Services kept:** drop `kind: job` services unless `keepJobs: true`. `kind: run` services are kept (they run on deploy, not on a schedule).
2. **Isolation checks (exit 2, before any remote call), per remaining service:** host `ports` → `service web publishes host ports; remove it or set ports: null in the preview template`; `imports` → same pattern; any volume whose top-level definition has `name:` or `external: true` → `volume uploads is shared across environments (name/external); previews only get fresh volumes`.
3. **Strip:** `backup` and `monitoring` on every service (no backup schedule, backup storage credentials or alert webhook reaches a preview).
4. **Replicas:** `min(replicas, maxReplicas)` per service (`maxReplicas` default 1, minimum 1). Services with a `placement.strategy: global` keep `global` but the cap does not apply to them.
5. **Proxy routes (D5).**
6. **Env:** add `TAKO_PREVIEW=true`, `TAKO_PREVIEW_NAME`, and, for proxied services, `TAKO_PREVIEW_URL`; then merge `preview.env` into every service and `preview.services.<svc>.env` into that service, after placeholder expansion; preview env wins over template env.

Volumes need no transform: `VolumeName(project, "preview-pr-42", volume)` is already a distinct Docker volume, so persistent services (databases) start empty. This is the safe default; sharing the base's data is out of scope and blocked by the check in step 2.

`tako validate` (and the warnings on `deploy`) validates every declared `preview:` block statically: known keys, placeholders known and referencing existing proxied services, `ttl` parses, `max >= 1`, the default-host template renders a valid hostname for the sample name `pr-1`. Isolation checks (step 2) run only when instantiating, because a concrete base such as `staging` may legitimately publish ports; `tako validate` reports them as `warn` findings on the preview base so CI catches them before the first PR.

### D5. Domains, DNS and certificates
For each proxied service in the instance, Tako computes one host from `preview.domain` (default `{name}.{domain}`): `{name}` = normalized preview name, `{service}` = service name, `{domain}` = the service's resolved primary host (public `domain`, or internal `host`). The route's `domain` (or `host` for internal visibility) is replaced with it; `domains`, `redirectFrom`, `dynamicDomains`, `cdn` are cleared; `email`, `tls`, `basicAuth`, `allowIps`, `trustedProxies`, path policies, `maxRequestBody` and `responseHeaders` are kept. Validation: every label ≤ 63, total ≤ 253, hosts unique within the instance, and no host equal to a host served by any declared environment of the project (exit 2 otherwise).

DNS is the operator's job: a wildcard A/AAAA record (for example `*.preview.example.com`, or `*.app.example.com` with the default template) pointing at the environment's proxy servers. Domain checks behave exactly as for other environments (warn, or fail with `--strict-domains`). Certificates are per host through the existing HTTP-01 or the environment's DNS-01 provider; the default 168h TTL and name reuse (same PR → same host → Caddy reuses the stored certificate) keep Let's Encrypt usage low. Docs recommend `tls.staging: true` in the template while trying previews out.

### D6. Expiry: CLI-driven prune, not a takod sweeper
Each record stores `expiresAt = updatedAt + ttl` (or none). It is enforced by:
- `tako preview prune [--dry-run]`: destroys every preview with `expiresAt < now`, one at a time, each under its own lease; a preview whose lease is held is skipped and reported; exit 6 if any expired preview could not be destroyed, else 0.
- `tako deploy --preview` runs the same prune (best effort, failures are warnings, never change the deploy result) before counting against `max`, unless `--no-prune`.
- `tako preview list` shows `expired` status, and `tako doctor` warns about expired previews.

Why not takod: destroying an environment is a multi-node operation coordinated by the CLI (leases on every target server, proxy routes on proxy nodes, state documents on all servers, `pkg/engine/destroy.go:236`). Moving it into takod would need a new autonomous, cross-node, data-deleting authority in the agent, plus node-to-node coordination takod does not have today, and it would run without an operator or CI log. CI already has natural triggers (PR closed → `preview destroy`; a scheduled workflow → `preview prune`), and the opportunistic prune bounds leaks for teams without a schedule. A takod sweeper can be added later on top of the same records if needed.

### D7. Preview records and listing (takod, gated)
New state document kind `preview` (`stateDocumentPreview`, `pkg/takod/state.go:15-20`) stored at `desired/<project>/<environment>/preview.json`, written by the CLI through the existing `/v1/state` document API after a successful deploy (with the deployment's desired state) to every target server of the instance:

```json
{
  "apiVersion": "tako.redentor.dev/v1alpha1", "kind": "PreviewRecord",
  "project": "app", "environment": "preview-pr-42", "name": "pr-42", "base": "preview",
  "servers": ["app-1"], "hosts": {"web": "pr-42.preview.example.com"},
  "createdAt": "…", "updatedAt": "…", "expiresAt": "…", "ttl": "168h",
  "source": {"branch": "feature/login", "commit": "abc1234"}, "createdBy": "ci"
}
```

New read-only route `GET /v1/state/previews?project=<p>` returns every record under `desired/<project>/*/preview.json` on that node. `tako preview list` queries all servers of the project config (union of every environment's servers), merges records by identity keeping the newest `updatedAt`, and reports unreachable servers as warnings (exit 6 if any server failed, as `logs` does).

New capability `preview.lifecycle-v1` (`server.go`) means: the node stores `preview` documents, serves the listing route, honors `CleanupRequest.RemovePreviewVolumes`, and matches networks exactly (D8). `deploy --preview`, `preview destroy|prune` require it on every target server; `preview list` requires it on every queried server. Missing → the standard message from `RequireTakodCapabilityContext`, before any mutation.

### D8. Destroy removes everything the preview owns
`tako preview destroy <name>` resolves the instance the same way as deploy (base from the record, falling back to D3), then runs `PlanDestroy`/`Apply` with these differences:
- Services to decommission = current template services ∪ services in the preview's replicated desired state, so a service removed from the template since the last deploy is still removed.
- Target servers = record `servers` ∪ currently resolved servers.
- After decommission, `CleanupRequest{RemovePreviewVolumes: true, Environment: identity}` removes Docker volumes with exact labels `tako.project=<p>` and `tako.environment=<identity>`. takod refuses this flag unless `preview.json` exists for that environment and the environment name starts with `preview-`, so a bug or a hand-crafted request cannot delete a normal environment's data.
- The `preview` document and the environment's state directory are removed last; the record is the "it still exists" marker, so a failed destroy can be retried.
- No interactive production guard (identity is never `production`); `--yes` is still required in non-interactive and machine modes, consistent with `destroy`.

Network fix (unconditional in the new takod): `cleanupNetworks` with an environment removes only `NetworkName(project, env)` and names matching `ExportNetworkName(project, env, <service>)` for networks whose `tako.environment` label equals `env` — never a prefix match. Shipped in the same takod release; covered by `preview.lifecycle-v1` because only upgraded nodes can host previews.

### D9. Secrets for previews
The secrets manager is created with the **base** name for file lookup (`.tako/secrets` then `.tako/secrets.<base>`), and with the instance's resolved `secretSources` (inherited from the base chain). A dedicated abstract `preview` template therefore gets its own `.tako/secrets.preview` file, separate from `secrets.staging`/`secrets.production` — the recommended setup. Implemented by `cfg.SecretsEnvironment(env)` returning the base for the instance and `env` otherwise, used by `NewManagerForConfig` and the hook/redaction registration paths. Preview names, identities and hosts are registered as public values with the redactor (they are config names).

### D10. `--preview` on other commands
`--preview <name>` is a persistent root flag. Commands annotated `previewSupported` (`deploy`, `ps`, `logs`, `exec`, `history`, `rollback`, `config explain`) load the config with `LoadOptions{Preview}` and use the identity as the environment. Every other command fails with exit 2 `--preview is not supported by tako <cmd>`. `ps/logs/exec/history/rollback` with an unknown preview fail with exit 2 when no record exists (they read the record first to find the base). `rollback --preview` rolls back within the preview's own history; the summary's undo for the first deploy of a preview is `tako preview destroy <name>`.

### D11. Machine interface
- `DeployResult.preview` (additive): `{name, environment, base, created, expiresAt, ttl, hosts}`; `created` is true on the first deploy.
- `PreviewList` (`items[]` of records plus `status: active|expired`), `PreviewDestroyResult` (`status: destroyed|not_found|failed`, removed counts), `PreviewPruneResult` (`destroyed[]`, `skipped[]` with reason, `failed[]`).
- Events: `preview.destroyed`, `preview.pruned` (one per destroyed preview during prune, including the opportunistic prune in deploy), plus existing `destroy`/`cleanup` events.
- Exit codes: `0` success (including destroy `not_found`); `2` invalid name, unknown base, isolation check failure, `max` reached, base mismatch, unsupported `--preview`; `3` lease held (destroy/deploy); `6` prune or list partially failed.
- `add-deploy-summary` integration: the summary builder sets `environment.kind = "preview"` and adds `environment.preview = {name, base, expiresAt}`; URLs get `kind: "preview"`; human block adds `Preview pr-42 (from preview) · expires 2026-10-18 14:02 UTC`.
- `cmd/machine_coverage_test.go` and the coverage table list `preview list|destroy|prune` as full contract.

### D12. Limits and concurrency
`max` (default 5) is checked per base after the opportunistic prune, counting records of that base other than the one being deployed; reaching it is exit 2 listing the existing previews and their expiry. Each preview has its own lease scope, so preview deploys never block deploys of the base or of other previews; two CI jobs deploying the same preview serialize on that preview's lease (exit 3 for the loser, as today). The record write happens after the rollout under the same lease.

## Risks / Trade-offs

- **Small VPS exhaustion.** Each preview runs a full copy of the template, including a fresh database. Mitigated by `max`, `maxReplicas`, recommending a lean template (`services: {worker: null}`, `resources` limits), and placing previews on dedicated servers via the template's `servers`. Images for preview builds are cleaned only by the existing `cleanOldImages` retention.
- **Expired previews linger without CI.** Bounded by the opportunistic prune on every preview deploy and surfaced by `preview list`/`doctor`.
- **Let's Encrypt rate limits** (50 certificates per registered domain per week). A busy repo can hit it with per-PR hosts on the production apex. Docs recommend a separate preview domain and `tls.staging` while testing.
- **Secrets exposure.** A preview of a concrete base uses that base's secrets. Opt-in is explicit (`preview:` must be declared), and docs recommend an abstract `preview` template with its own `secrets.preview` and non-production credentials.
- **Proxy host clashes across projects.** Two projects on the same proxy generating the same host is not detected (no cross-project index today); generated hosts include project-specific template domains in practice.
- **Records on a subset of servers.** If a destroy fails midway, records may remain on some nodes. `preview list` merges by newest record and `destroy` is retryable and idempotent.

## Migration Plan

No config migration. Rollout order: ship takod with `preview.lifecycle-v1` and the network fix; operators run `tako upgrade servers`; then the CLI features work. A newer CLI against older nodes fails closed with the upgrade message; an older CLI ignores `preview` documents (unknown document kinds are never read).

## Open Questions

1. **Identity prefix `preview-` vs a shorter `pv-`.** Recommendation: `preview-`, for readability in `docker ps` and logs; the 48-character identity still fits every bounded runtime name.
2. **Default `ttl`.** Recommendation: `168h` (7 days) refreshed on every deploy — long enough for a slow review, short enough to bound leaks.
3. **Default `max`.** Recommendation: `5` per base; small VPSes are the primary audience, and the error message tells the operator how to raise it.
4. **Persistent services: fresh, skipped, or shared?** Recommendation: fresh preview-scoped volumes (isolated, empty). Skipping breaks apps that need their database; sharing risks corrupting real data and is blocked.
5. **Expiry enforcement: takod sweeper or CLI prune?** Recommendation: CLI prune plus opportunistic prune on deploy (D6); revisit a sweeper only if users run previews without CI.
6. **Should `--preview` imply `--strict-domains`?** Recommendation: no; keep the normal default so a missing wildcard record does not fail CI the first time, and document `--strict-domains` for teams that want it.
7. **Inject `TAKO_PREVIEW_*` into containers by default?** Recommendation: yes; they are non-secret, only present in previews, and let apps adjust absolute URLs without template work.
