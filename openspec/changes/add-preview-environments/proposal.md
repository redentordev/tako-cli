# Proposal

## Why

Teams moving to Tako from Vercel, Render or Heroku expect a throwaway copy of the app per branch or pull request, on its own URL, that disappears when the PR closes. Tako has no such feature. Today an operator has to:

- hand-write a new environment in `tako.yaml` per branch (environment names are static keys in `Config.Environments`, `pkg/config/types.go:53`);
- invent unique domains for every proxied service;
- remember to strip backup schedules, cron jobs, host ports and shared volumes so the copy cannot damage the real environment;
- remember to `tako destroy` it later — and even then its Docker volumes are never removed (`cleanupUnusedProjectVolumes` is a no-op, `pkg/takod/cleanup.go:431`).

Most building blocks already exist: `extends`/`abstract` resolve an environment from a template (`pkg/config/environment_extends.go`), runtime names, leases, desired state and proxy files are all scoped by `(project, environment)` (`pkg/runtimeid/runtimeid.go`, `pkg/takod/state.go:260`), and tako-proxy issues per-host certificates automatically. What is missing is a way to instantiate an environment on demand under a generated name, a safe default policy for that instance, a record of which instances exist, and a way to expire them.

## What Changes

- **`preview:` policy block on an environment.** Any environment (typically an `abstract` template that `extends` staging) can declare a `preview:` block. Only an environment with this block can be the base of a preview. Keys:
  - `domain`: hostname template, placeholders `{name}`, `{service}`, `{domain}`; default `{name}.{domain}` (the service's base primary host prefixed with the preview name).
  - `ttl`: lifetime refreshed on every deploy of the preview; default `168h`; `none` disables expiry.
  - `max`: concurrent previews of this base; default `5`.
  - `maxReplicas`: replica cap applied to every service; default `1`.
  - `keepJobs`: keep `kind: job` services; default `false`.
  - `env` and `services.<name>.env`: extra container env, with `{name}`, `{host:<service>}` and `{url:<service>}` placeholders.
  - `preview` is not inherited through `extends`, like `abstract`.
- **`tako deploy --preview <name>`.** Synthesizes a concrete environment named `preview-<normalized name>` that extends the base (`-e`, or the only environment declaring `preview:`), applies the preview policy, and deploys it with the normal engine. Re-running with the same name updates the preview and refreshes its TTL.
- **Safe defaults for every preview instance:**
  - Fresh, preview-scoped volumes: persistent services (databases, caches) start empty, isolated from the base. Volumes declared with `name:`, `external: true`, a non-`local` `driver` or `driver_opts`, and absolute host bind mounts are rejected unless the service is removed or overridden in the template.
  - `backup` and `monitoring` are stripped from every service; `kind: job` services are dropped unless `keepJobs`; `deploy.promotion` is forced to `automatic` (there is no `promote --preview`).
  - A preview deploy skips repository-wide old-image retention on servers a declared environment uses, so PR deploys cannot evict the base's rollback images.
  - Services with host `ports` or cross-project `imports` are rejected with a message telling the operator to drop or override them in the template.
  - Proxied services get exactly one generated host each; `domains`, `redirectFrom`, `dynamicDomains` and `cdn` are dropped; `basicAuth`, `allowIps` and the other route policies are kept.
- **`tako preview list | destroy <name> | prune`.** List previews with base, hosts, age and expiry; destroy one (idempotent: an unknown name is success with status `not_found`); prune every expired preview. `deploy --preview` prunes expired previews of the same project first (opt out with `--no-prune`).
- **Read/operate commands accept `--preview <name>`:** `ps`, `logs`, `exec`, `history`, `rollback`, `config explain` (which stays offline). Every other environment-scoped command rejects `--preview` with exit 2.
- **Preview records in replicated state.** A new per-environment state document `preview` (name, base, hosts, servers, created/updated/expires timestamps, git source) is written to every target server before the first mutation (so a failed first deploy is still listable and destroyable) and refreshed after a successful rollout. A new takod read endpoint lists records; the CLI queries every server in top-level `servers:`.
- **Destroy owns exactly the preview.** One ordered teardown request per node removes proxy routes, containers, networks, volumes, history and operator files, then state, deleting the record last so a failed destroy can be retried. It never removes images (their repositories are shared with the base) or project-wide paths. takod refuses it unless the environment is `preview-*` and has a record.
- **takod capability `preview.lifecycle-v1`** gates the record document, the listing endpoint, and preview data removal. A CLI talking to an older node fails before any mutation with the standard upgrade message.
- **Bug fix (also benefits non-preview users):** takod network cleanup for an environment matches exact runtime network names instead of the `tako_<project>_<env>_` prefix (`pkg/takod/cleanup.go:255-285`), which today also matches every environment whose name starts with `<env>-` (for example destroying `prod` touches `prod-eu` networks, and destroying a declared `preview` environment would touch `preview-pr-42`). Unlabeled legacy networks that only share the prefix are left alone.
- **Machine interface:** `DeployResult` gains an additive `preview` object; new result documents `PreviewList`, `PreviewDestroyResult`, `PreviewPruneResult`; new events `preview.pruned`, `preview.destroyed`. The end-of-deploy summary (sibling change `add-deploy-summary`) reports `environment.kind: preview` and the expiry; URLs keep `kind: public`/`internal`; every command it prints uses `--preview <name>`.
- **Hooks:** deploy hooks for a preview get `TAKO_PREVIEW_NAME`, `TAKO_PREVIEW_BASE` and `TAKO_PREVIEW_EXPIRES_AT` (useful for seeding a fresh database in `post-deploy`).
- **Containers** of a preview get `TAKO_PREVIEW=true`, `TAKO_PREVIEW_NAME`, and `TAKO_PREVIEW_URL` (their own URL when proxied).

No **BREAKING** change: configs without a `preview:` block load, validate and deploy exactly as today; all JSON additions are new optional fields or new documents; no existing flag changes meaning. The network-cleanup fix only narrows what cleanup removes.

## Out of Scope

- Creating DNS records. The operator must point a wildcard record (for example `*.preview.example.com`) or per-host records at the proxy servers. Tako never creates cloud infrastructure.
- Wildcard certificates. Each preview host gets its own certificate through the existing HTTP-01 or environment DNS-01 path.
- Cloning or snapshotting the base environment's data into a preview. Seeding is left to `post-deploy` hooks or application code.
- Sharing the base environment's databases or volumes with a preview.
- A takod-side expiry sweeper (see design D6); expiry is enforced by `tako preview prune` and by the opportunistic prune on `deploy --preview`.
- Posting preview URLs as PR comments, or a published GitHub Action. CI reads URLs from `--output json` or the GitHub step summary from `add-deploy-summary`.
- `--preview` on `promote`, `scale`, `start`, `stop`, `remove`, `backup`, `jobs`, `maintenance`, `config export|pull`, `drift` and `placement`.
- Per-preview resource quotas beyond `max` and `maxReplicas`; operators set `resources` in the template.
- Preview-specific notification routing; top-level `notifications` behave as for any environment.

## Capabilities

### New Capabilities
- `preview-environments`: on-demand environments instantiated from a `preview:` template — naming, policy transforms, domains, isolation guarantees, records, TTL and pruning, the `tako preview` commands, `--preview` on read/operate commands, machine output and capability gating.

### Modified Capabilities
- `environment-config`: abstract environments may be selected with `-e` as the base of `--preview`; the `preview:` block is a non-inherited key; an abstract environment with a `preview:` block is not reported as unused.
- `deploy-hooks`: preview context variables in the hook environment.

## Impact

- **Config (`pkg/config`):** new `PreviewConfig` type and `EnvironmentConfig.Preview` (`types.go:778-796`); preview synthesis in `resolveConfigDocument` (`environment_extends.go:290`) driven by a new `LoadOptions`; policy transforms and validation in a new `preview.go`; `ValidationWarnings` exemption; schema.
- **CLI (`cmd`):** `--preview` and `--no-prune`/`--ttl` on deploy (`cmd/deploy.go:69-86`); new `cmd/preview.go` (`list|destroy|prune`); `--preview` on `ps`, `logs`, `exec`, `history`, `rollback`, `config explain`; rejection elsewhere; machine coverage table.
- **Engine (`pkg/engine`):** preview record writes before mutation and after a successful rollout; list/destroy/prune built on `PlanDestroy`/`Apply` (`destroy.go:122`, `:236`) with a dedicated teardown request; post-deploy image retention skipped on shared servers; secrets file selection for previews (`pkg/secrets/sources.go:84`); hook env.
- **takod:** state document `preview` (`pkg/takod/state.go:15-20`, `:260`); `GET /v1/state/previews` listing route registered in `registeredRoutes` (`lifecycle_gate.go:25-41`); guarded, ordered `CleanupRequest.RemovePreviewData`; exact network matching; capability constant in `server.go`.
- **Docs:** new `docs/PREVIEW-ENVIRONMENTS.md`, `docs/CONFIGURATION.md`, `docs/CI-CD.md` (GitHub workflow for PR open/close), `docs/MACHINE-INTERFACE.md`; `schema/tako.schema.json`; regenerated `man/` and `completions/`; a new example `examples/23-preview-environments`.
- **Depends on:** `add-deploy-summary` for the summary rendering of preview URLs and expiry (the `kind` fields it reserves). Independent of `explain-secret-redaction`.
