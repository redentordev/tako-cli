# Design

## Context

See proposal.md for motivation and specs/registry-auth/spec.md for the behavior contract. Observed current state (pre-change citations):

- **Config.** `Config.Registries map[string]RegistryConfig` (`pkg/config/types.go:40`), `RegistryConfig{Username, Password}` (`:184-187`). `validateRawRegistryCredentials` (`pkg/config/registries.go:28`) runs on raw bytes before `${VAR}` expansion (`types.go:1298`) and rejects any non-empty password that is not a whole `${VAR}`. `validateRegistries` (`registries.go:52`, called from `validator.go:73`) requires both username and password. The schema (`schema/tako.schema.json:286-304`) marks both required and sets `additionalProperties: false`.
- **Flow to nodes.** `(*Deployer).registryAuths()` (`pkg/deployer/registry.go:31`) maps config to `[]takod.RegistryAuth`, sorted by host. It is attached to reconcile requests only when `pullImage` is true (`takod_deploy.go:1685-1686`), to `tako run` (`run.go:136`), and to the takod build preamble (`deployer.go:611-634`, parsed at `pkg/takod/server.go:2005-2030`) so `FROM` images in private registries resolve. On the node, `writeEphemeralDockerConfig` (`pkg/takod/registry_auth.go:65`) writes a 0600 `config.json` in a 0700 temp dir and `runDockerWithAuth` (`:101`) deletes it after each docker call. Reconcile requests document that auths are never persisted (`pkg/takod/reconcile.go:77-80`). Auth failures become `RegistryAuthError` / `image.pull.auth_failed` (`pkg/deployer/registry.go:15`, `:58`).
- **Redaction.** `Engine.RegisterRegistrySecrets(cfg)` (`pkg/engine/engine.go:179`) registers static passwords; called from `deploy.go:248`, `rollback_project.go:298`, `run.go:116`. `Engine.RegisterSecret` (`:106`) registers an arbitrary runtime value; the redactor ignores values shorter than 4 characters (`pkg/secrets/redactor.go`).
- **Provider CLI pattern.** `pkg/secrets/provider.go`: `providerAdapter` registry (`:66-174`), LookPath check with `"%s provider requires the %q CLI on PATH; install it from %s"` (`:200`), `runProviderCommand` (`:335`) with a 2-minute timeout (`:30`), `os.Environ()` passthrough, stderr captured into errors, test seams `providerCommandContext`/`providerLookPath` (`:29-31`) and `SetProviderCommandForTesting` (`testing_hooks.go:13`). `appendAWSFlags` (`provider_aws.go:196`) and `appendGCPFlags` (`provider_gcp.go:84`) already format `--profile/--region` and `--project`. Tests use a re-exec fake (`provider_test.go:347-366`).

## Goals / Non-Goals

**Goals:**
- Zero-wrapper deploys to ECR, GCR/Artifact Registry and GHCR using the operator's existing CLI login.
- A minted token is only ever in memory on the deploying machine and in the ephemeral `DOCKER_CONFIG` takod already uses.
- No takod change and no new takod capability.
- Reuse the secrets provider exec helper rather than a second copy.

**Non-Goals:**
- AWS/GCP SDK dependencies.
- Node-side credential helpers (`docker-credential-ecr-login`) — they would need cloud credentials on the VPS.

## Decisions

### D1. Shell out to the provider CLIs, not the SDKs
Reasons: matches `pkg/secrets` (which already depends on `aws`, `gcloud`, `gh`-style CLIs), honors every credential source the CLI supports (SSO, `credential_process`, assumed roles, `gcloud` impersonation) without Tako reimplementing them, and avoids adding the AWS SDK v2 ECR module and Google auth libraries (binary size, dependency churn). Cost: the CLI must be installed; the LookPath error names the install URL, same text shape as secret providers.

Implementation: move `runProviderCommand`, the seams and the LookPath message into an internal helper `internal/clicmd` (or export them from `pkg/secrets` as `secrets.RunCLI`). The new `pkg/registryauth` package uses that helper so `SetProviderCommandForTesting` fakes both features.

### D2. Provider table

| provider | command | username | host match | options |
|---|---|---|---|---|
| `ecr` | `aws ecr get-login-password --region R [--profile P]` | `AWS` | `^\d{12}\.dkr\.ecr(-fips)?\.([a-z0-9-]+)\.amazonaws\.com(\.cn)?$` | `region` (default: from host), `profile` |
| `gcr` / `artifact-registry` | `gcloud auth print-access-token [--account A]` | `oauth2accesstoken` | `gcr.io`, `*.gcr.io`, `*-docker.pkg.dev` | `account` |
| `ghcr` | env `GITHUB_TOKEN`, then `GH_TOKEN`, then `gh auth token` | `username`, else `GITHUB_ACTOR`, else `gh api user --jq .login` | `ghcr.io` | `username` |

- A provider whose host pattern does not match is a config error (catches copy-paste mistakes such as `provider: ecr` on a GCR host). An explicit `region` that disagrees with the region in an ECR host is an error, not a silent override.
- Output is trimmed; empty output, output containing control characters, or output longer than 16 KiB is rejected (takod caps fields at 4 KB — `registry_auth.go:29` — so the minter checks the same 4 KB cap and reports a clear error).
- `ghcr` with `gh auth token`: the default `gh` OAuth scope may lack `read:packages`; a pull auth failure for a `ghcr` provider entry adds the hint "token from gh auth token may need `gh auth refresh -s read:packages`".

### D3. Lazy, cached, refreshed minting
`registryauth.Minter` holds `map[cacheKey]mintedToken{value, username, mintedAt}`; cacheKey is host + provider + options. `registryAuths(ctx)` calls `Minter.Auth(ctx, host)` for each provider entry. Called only on paths that already attach auths (pull, run, build preamble), so a deploy that only transfers images never runs a CLI. Refresh ages: ECR 6h (token lasts 12h), GCR 45m (token ~60m), GHCR never. Concurrent callers for the same key share one in-flight mint (`singleflight`).

### D4. Redaction before use
The minter takes a `register func(string)` callback wired to `Engine.RegisterSecret` (and to the CLI-level redactor for `tako run`). The token is registered before `Auth` returns, so it is masked even if the CLI's own stderr or a docker error echoes it. CLI stderr in mint errors is passed through the redactor too. Because the redactor ignores values under 4 characters, tokens shorter than 16 characters are rejected as malformed.

### D5. Validation
- Raw check (`validateRawRegistryCredentials`): unchanged for entries with `password`; entries with `provider` and no `password` pass.
- `validateRegistries`: `provider` set → `password` must be empty (`registry H: provider and password are mutually exclusive`); `username` allowed only for `ghcr`; `region`/`profile` only for `ecr`; `account` only for `gcr`; host pattern must match (D2). `provider` unset → today's rules, and provider-only keys are errors.
- Schema: `required` becomes `oneOf: [{required: [username, password]}, {required: [provider]}]`, `provider` enum, the option keys added.

### D6. Nothing persisted
No token is written to `.tako/`, local state, history, desired state on takod, events, or `config explain` output (which shows `provider: ecr` and `password: <minted at deploy>`). takod's existing ephemeral config path is unchanged. A unit test asserts the reconcile request JSON written to takod desired state never includes `registryAuths` (already `json:"registryAuths,omitempty"` in request only; desired state is a separate type — the test pins that).

## Risks / Trade-offs

- [Token expiry during a long multi-hour deploy] → refresh ages in D3; each pull asks the minter, so a re-mint happens transparently.
- [CLI prompts interactively (e.g. SSO login expired)] → commands run with stdin closed and the 2-minute timeout; the error suggests `aws sso login` / `gcloud auth login` / `gh auth login`.
- [Machine output contamination] → CLI stdout is captured, never streamed; stderr only appears inside redacted errors.
- [`GITHUB_TOKEN` present in CI but scoped to another repo] → standard GHCR behavior; documented.
- [Background takod reconciles (self-heal) after the deploy cannot pull] → unchanged from static credentials today: images are already local after deploy; takod never pulls without a request-scoped auth.

## Migration Plan

Additive. Users replace

```yaml
registries:
  123456789012.dkr.ecr.us-west-1.amazonaws.com:
    username: AWS
    password: ${ECR_PASSWORD}
```

with `provider: ecr` and drop the wrapper. Older CLIs reject `provider` as an unknown field (fail closed).

## Open Questions

1. **CLI vs SDK for ECR?** Recommended: CLI (D1). Revisit only if users report missing `aws` CLI on CI runners; an SDK path could later sit behind the same adapter interface.
2. **Should `ghcr` prefer `gh auth token` over `GITHUB_TOKEN`?** Recommended: environment first. In CI `GITHUB_TOKEN` is the intended identity; locally it is usually unset.
3. **Support `provider: exec` with a custom command now?** Recommended: no; arbitrary command strings in `tako.yaml` widen the trust surface. Add named adapters instead.
4. **Fail or fall back when the CLI is missing?** Recommended: fail the operation at first credential use with the install hint; no silent anonymous pull (it would surface as a less clear auth error on the node).
5. **Mint once up front during plan/preflight to fail fast?** Recommended: yes for `tako deploy` when the plan contains any pull or build-with-auth — mint in preflight (still lazy otherwise), so failures happen before any mutation.
