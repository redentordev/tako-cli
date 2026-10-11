# Design

## Context

See proposal.md for motivation and specs/registry-auth/spec.md for the behavior contract. Observed current state (pre-change citations):

- **Config.**
  - `Config.Registries map[string]RegistryConfig` (`pkg/config/types.go:40`), with `RegistryConfig{Username, Password}` (`:184-187`).
  - `validateRawRegistryCredentials` (`pkg/config/registries.go:28`) runs on raw bytes before `${VAR}` expansion (`types.go:1298`) and rejects any non-empty password that is not a whole `${VAR}`.
  - `validateRegistries` (`registries.go:52`, called from `validator.go:73`) requires both username and password.
  - The schema (`schema/tako.schema.json:286-304`) marks both required and sets `additionalProperties: false`.
  - `tako config explain` (`cmd/config.go`) does not print registries at all.
- **Flow to nodes.**
  - `(*Deployer).registryAuths()` (`pkg/deployer/registry.go:31-53`) maps **every** complete registry entry to `[]takod.RegistryAuth`, sorted by host, whatever image is being pulled.
  - It is attached to reconcile requests only when `pullImage` is true (`takod_deploy.go:1685-1686`), to `RunDeployStepOnNodes` (`run.go:136`), and to the takod build preamble (`deployer.go:611-631`, parsed at `pkg/takod/server.go:2005-2030`) so `FROM` images in private registries resolve.
  - On the node, `writeEphemeralDockerConfig` (`pkg/takod/registry_auth.go:65`) writes a 0600 `config.json` in a 0700 temp dir, and `runDockerWithAuth` (`:101`) deletes it after each docker call.
  - takod rejects any auth field longer than 4096 bytes or containing control characters (`registry_auth.go:27-46`).
  - Reconcile requests document that auths are never persisted (`pkg/takod/reconcile.go:77-80`).
  - Auth failures become `RegistryAuthError` / `image.pull.auth_failed` (`pkg/deployer/registry.go:15`, `:58`).
  - `cmd/run.go:145` already has `runImageRegistryHost(imageRef)`, which maps an image reference to its registry host.
- **Redaction.**
  - `Engine.RegisterRegistrySecrets(cfg)` (`pkg/engine/engine.go:179`) registers static passwords. It is called from `deploy.go:248`, `rollback_project.go:298`, `run.go:116` and `cmd/hooks.go:162`.
  - `Engine.RegisterSecret` (`:106`) registers an arbitrary runtime value. The redactor ignores values shorter than 4 characters (`pkg/secrets/redactor.go`).
- **Provider CLI pattern.**
  - `pkg/secrets/provider.go` has a `providerAdapter` registry (`:66-174`) and a LookPath check with the message `"%s provider requires the %q CLI on PATH; install it from %s"` (`:200`).
  - `runProviderCommand` (`:335-361`) runs with a 2-minute timeout (`:30`), passes through `os.Environ()`, leaves stdin unset (empty input) and captures output.
  - On a non-zero exit, it puts stderr into the error, **falling back to stdout when stderr is empty** (`:350-353`). For a command whose stdout is a credential, that fallback can put the credential in an error message.
  - Test seams: `providerCommandContext`/`providerLookPath` (`:29-31`) and `SetProviderCommandForTesting` (`testing_hooks.go:13`).
  - `appendAWSFlags` (`provider_aws.go:196`) and `appendGCPFlags` (`provider_gcp.go:84`) already format `--profile/--region` and `--project`.
  - Tests use a re-exec fake (`provider_test.go:347-366`).
- **Exit codes** (`docs/MACHINE-INTERFACE.md:742-753`): 1 means the operation ran and failed, 2 an invalid request or config, 4 node connectivity.

## Goals / Non-Goals

**Goals:**
- Zero-wrapper deploys to ECR, GCR/Artifact Registry and GHCR using the operator's existing CLI login.
- A minted token exists only in memory on the deploying machine and in the ephemeral `DOCKER_CONFIG` that takod already uses.
- No takod change and no new takod capability.
- Reuse the secrets provider exec helper rather than a second copy.

**Non-Goals:**
- AWS/GCP SDK dependencies.
- Node-side credential helpers (`docker-credential-ecr-login`), which would need cloud credentials on the VPS.
- Changing how static `username`/`password` entries are attached.

## Decisions

### D1. Shell out to the provider CLIs, not the SDKs
Reasons:
- It matches `pkg/secrets`, which already shells out to the `aws` and `gcloud` CLIs.
- It honors every credential source the CLI supports (SSO, `credential_process`, assumed roles, `gcloud` impersonation) without Tako reimplementing them.
- It avoids adding the AWS SDK v2 ECR module and Google auth libraries (binary size, dependency churn).

Cost: the CLI must be installed. The LookPath error names the install URL, in the same shape as secret providers.

Implementation: move `runProviderCommand`, the seams and the LookPath message into `internal/providercli`, used by both `pkg/secrets` and the new `pkg/registryauth`, so `SetProviderCommandForTesting` fakes both. The helper takes an option controlling whether stdout may appear in errors:
- `pkg/secrets` keeps today's behavior.
- Registry minting never includes stdout in any error. It includes stderr only after passing it through the redactor, and truncates it to 2 KiB.

### D2. Provider table

| provider | command | username | host match | options |
|---|---|---|---|---|
| `ecr` | `aws ecr get-login-password --region R [--profile P]` | `AWS` | `^\d{12}\.dkr\.ecr(-fips)?\.([a-z0-9-]+)\.amazonaws\.com(\.cn)?$` | `region` (default: from host), `profile` |
| `gcr` / `artifact-registry` | `gcloud auth print-access-token [--account A]` | `oauth2accesstoken` | `gcr.io`, `*.gcr.io`, `*-docker.pkg.dev` | `account` |
| `ghcr` | env `GH_TOKEN`, then `GITHUB_TOKEN`, then `gh auth token` | `username`, else `GITHUB_ACTOR`, else `gh api user --jq .login` | `ghcr.io` | `username` |

- A provider whose host pattern does not match is a config error. This catches copy-paste mistakes such as `provider: ecr` on a GCR host. An explicit `region` that disagrees with the region in an ECR host is an error, not a silent override.
- Output is trimmed. Empty output, output with control characters, output shorter than 16 characters, or output longer than 4096 bytes is rejected with an error naming the provider and host, never the value. 4096 bytes is takod's existing field cap (`registry_auth.go:27`): a longer token would be rejected on the node with a less clear error, and raising the cap would need a takod capability. ECR tokens already pass through this cap today via the static `${ECR_PASSWORD}` workaround.
- `GH_TOKEN` comes before `GITHUB_TOKEN`, the precedence `gh` itself uses. A user who sets `GH_TOKEN` in Actions, for example a token with `read:packages` on another repository's package, means it to override the workflow's `GITHUB_TOKEN`.
- `ghcr` with `gh auth token`: the default `gh` OAuth scope may lack `read:packages`. A pull auth failure for a `ghcr` provider entry adds the hint "token from gh auth token may need `gh auth refresh -s read:packages`".

### D3. Which credentials a request carries
- **Static entries:** unchanged. Every complete static entry is attached to every request that carries auths, as today.
- **Provider entries on pulls (reconcile requests and `RunDeployStepOnNodes`):** only the entry whose host equals the image's registry host is minted and attached. `runImageRegistryHost` moves to `pkg/config` (or a small `pkg/imageref`) so the deployer and `cmd/run.go` share it. Minting for unrelated hosts would run CLIs, and fail on machines without them, for registries the pull cannot use.
- **Provider entries on takod builds:** every provider entry is minted and attached. A Dockerfile's `FROM` lines can reference any registry, possibly through build arguments, and parsing them is unreliable.

### D4. Preflight, lazy-by-need, cached and refreshed minting
`registryauth.Minter` holds `map[cacheKey]mintedToken{value, username, mintedAt}`. The cache key is host plus provider plus options.

- **Preflight.** Every operation that can attach registry credentials mints, before its first mutation, exactly the provider entries D3 says it will attach. These operations are deploy, single-service and project rollback, scale, promote, `run`, and placement apply. The set is computed from the inputs that decide `PullImage` and remote builds, so the same failure happens before any node changes, whichever command hit it. An operation that will attach no provider credential runs no CLI.
- **On use.** `registryAuths(ctx, imageRef)` calls `Minter.Auth(ctx, host)`, which returns the cached token or mints again when the token is older than its refresh age: ECR 6h (the token lasts 12h), GCR 45m (the token lasts about 60m), GHCR never. A re-mint during a long deploy is transparent.
- Concurrent callers for the same key share one in-flight mint (`golang.org/x/sync/singleflight`; `golang.org/x/sync` is already a direct dependency in `go.mod`).
- **Failure.** A missing CLI, a failing command or a rejected token fails the operation with exit 1, as a runtime failure on the deploying machine. Config validation has already passed, and the cure is an install or a login, not a config edit. The error names the CLI and suggests `aws sso login`, `gcloud auth login` or `gh auth login`. When the failure happens in preflight, nothing is mutated.

### D5. Redaction before use
The minter takes a `register func(string)` callback wired to `Engine.RegisterSecret`. The token is registered before `Auth` returns, so it is masked even if a docker error on a node echoes it. Mint errors never contain stdout (D1), and their stderr passes through the redactor. Because the redactor ignores values under 4 characters, tokens shorter than 16 characters are rejected as malformed (D2).

### D6. Validation
- Raw check (`validateRawRegistryCredentials`): unchanged for entries with `password`. Entries with `provider` and no `password` pass.
- `validateRegistries`, when `provider` is set:
  - `password` must be empty (`registry H: provider and password are mutually exclusive`);
  - `username` is allowed only for `ghcr`;
  - `region`/`profile` are allowed only for `ecr`, and `account` only for `gcr`;
  - the host pattern must match (D2).
- `validateRegistries`, when `provider` is unset: today's rules, and provider-only keys are errors.
- Schema: `required` becomes `oneOf: [{required: [username, password]}, {required: [provider]}]`, with a `provider` enum and the option keys added.

### D7. Nothing persisted
No token is written to `.tako/`, local state, history, desired state on takod, or events. takod's existing ephemeral config path is unchanged. A unit test pins that the reconcile desired state takod persists never includes `registryAuths`. The field is `json:"registryAuths,omitempty"` on the request type only, and desired state is a separate type.

## Risks / Trade-offs

- **Token expiry during a multi-hour deploy:** each use asks the minter, which re-mints after the D4 refresh age.
- **The CLI prompts interactively** (for example, an expired SSO login): commands run with empty stdin and the 2-minute timeout, and the error suggests the login command.
- **Token in output:** CLI stdout is captured, never streamed, and never placed in an error (D1). stderr appears only after redaction.
- **`GITHUB_TOKEN` present in CI but scoped to another repository:** standard GHCR behavior, and documented. `GH_TOKEN` overrides it (D2).
- **Background takod reconciles (self-heal) after the deploy cannot pull:** unchanged from static credentials. Images are already local after the deploy, and takod never pulls without a request-scoped auth.
- **Builds mint every provider entry (D3):** a project with an ECR entry used only for pulls still needs `aws` when it builds on takod. This is accepted, because skipping it could break a `FROM` image, and the error names the entry.

## Migration Plan

This is additive. Users replace

```yaml
registries:
  123456789012.dkr.ecr.us-west-1.amazonaws.com:
    username: AWS
    password: ${ECR_PASSWORD}
```

with `provider: ecr` and drop the wrapper. Older CLIs reject `provider` as an unknown field, which fails closed.

## Implementation Order

One CLI-only PR. Accepting `provider` before minting is wired would ship a key that silently pulls anonymously, so config, minting and wiring must land together. No takod change and no capability are involved. Suggested commit order inside the PR:
1. Extract `internal/providercli` with the no-stdout option. `pkg/secrets` tests pass unchanged.
2. Config fields, validation and schema.
3. `pkg/registryauth` (adapters, host parsing, minter).
4. Deployer and engine wiring (D3 host filter, D4 preflight, D5 redaction).
5. Docs, including the `docs/MACHINE-INTERFACE.md` redaction bullet.

## Resolved Questions

1. **CLI or SDK for ECR?** CLI (D1). Revisit only if users report a missing `aws` CLI on CI runners. An SDK path could later sit behind the same adapter interface.
2. **Should `ghcr` prefer `gh auth token` over the environment?** No: environment first, with `GH_TOKEN` before `GITHUB_TOKEN`, matching `gh` (D2). In CI the environment token is the intended identity, and an explicit `GH_TOKEN` is a deliberate override. Locally both are usually unset.
3. **Support `provider: exec` with a custom command now?** No. Arbitrary command strings in `tako.yaml` widen the trust surface. Add named adapters instead.
4. **Fail or fall back when the CLI is missing?** Fail with exit 1 and the install hint, in preflight before any mutation (D4). A silent anonymous pull would surface later as a less clear auth error on the node.
5. **Mint up front to fail fast?** Yes, for every operation that can attach credentials, not only deploy (D4). Rollback, scale and promote also mutate nodes before their first pull. Minting stays need-based: nothing is minted when no provider credential will be attached.
6. **Mint for every registry on every pull?** No. Pulls mint and attach only the matching provider host, builds mint all provider entries, and static entries are unchanged (D3). This keeps unrelated CLIs out of image-only deploys without risking `FROM` resolution.
7. **May mint errors include CLI output?** stderr only, redacted and truncated. Never stdout, because for these commands stdout is the credential (D1). The secrets helper's existing stdout fallback stays for secrets but is disabled for minting.
8. **Token size limits?** 16 characters to 4096 bytes. The minimum keeps the redactor effective; the maximum is takod's existing cap, checked locally for a clear error, with no takod change (D2).
9. **`config explain` output?** No change. It does not print registries today, so the planned placeholder task is dropped. Machine-facing guarantees go into the `docs/MACHINE-INTERFACE.md` "Redaction Guarantees" list instead.
10. **Exit code for mint failures?** 1 (operation failed), not 2, because the config is valid and the remedy is a login or install on the deploying machine (D4).
