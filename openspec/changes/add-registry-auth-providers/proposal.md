# Proposal

## Why

`registries.<host>` accepts only a static `username`/`password` pair, and the password must be a whole `${ENV_VAR}` reference (`pkg/config/registries.go:28-49`, `:52-72`). That does not fit how the main cloud registries issue credentials:
- Amazon ECR passwords are 12-hour tokens.
- Google Artifact Registry / GCR access tokens last about an hour.
- GHCR tokens normally come from `gh auth token` or the CI-provided `GITHUB_TOKEN`.

Every deploy against those registries therefore needs a wrapper script that runs `aws ecr get-login-password`, exports the result, and only then runs Tako (issue #117). Forgetting the wrapper, or a token expiring between export and pull, gives a confusing `image.pull.auth_failed` on the nodes.

Tako already shells out to provider CLIs for secrets (`pkg/secrets/provider.go`, `provider_aws.go`, `provider_gcp.go`: LookPath check, 2-minute timeout, stderr captured, test seams). Minting a registry token is the same shape: run the CLI on the deploying machine, use the value, never persist it.

## What Changes

- `registries.<host>` gains an optional `provider` key with the values `ecr`, `gcr` (alias `artifact-registry`) and `ghcr`. With `provider` set, Tako mints a short-lived credential on the deploying machine and passes it to takod exactly like a static credential today (request-scoped `RegistryAuths`, ephemeral `DOCKER_CONFIG`, deleted after the docker call).
  - `ecr`: `aws ecr get-login-password --region <region> [--profile <profile>]`; username `AWS`. The region is parsed from the host (`<account>.dkr.ecr[-fips].<region>.amazonaws.com[.cn]`) unless `region` is set.
  - `gcr`: `gcloud auth print-access-token [--account <account>]`; username `oauth2accesstoken`. Hosts `gcr.io`, `*.gcr.io` and `*-docker.pkg.dev`.
  - `ghcr`: `GH_TOKEN`, then `GITHUB_TOKEN`, from the environment when set (the precedence `gh` uses), else `gh auth token`. The username is `username` when given, else `GITHUB_ACTOR`, else `gh api user --jq .login`.
- Provider-specific optional keys: `region`, `profile` (ecr) and `account` (gcr). These are config errors:
  - unknown provider names;
  - provider/host mismatches (e.g. `provider: ecr` on `ghcr.io`);
  - `provider` together with `password`;
  - `username` together with a provider other than `ghcr`.
- **Which requests carry a minted credential.** An image pull carries only the provider entry for the image's own registry host. A takod build carries every provider entry, because `FROM` lines can name any registry. Static entries are attached exactly as today.
- **When minting happens.** Every operation that can attach registry credentials mints the provider credentials it will attach before its first mutation: deploy, rollback (single-service and project), scale, promote, `run` and placement apply. An operation that attaches none runs no provider CLI. A missing CLI or a failed mint exits 1 with the install or login hint.
- **Protecting the token:**
  - Every minted token is registered with the output redactor before it can reach any event, error or log line.
  - Mint errors never include the CLI's stdout, which is the credential; stderr is shown only after redaction.
  - Tokens are never written to disk on the deploying machine or on a node.
  - Tokens shorter than 16 characters or longer than 4096 bytes (takod's existing field cap) are rejected with a clear error.
- **Caching.** Tokens are cached in memory for the life of one CLI process, keyed by host and provider options. They are re-minted when older than a provider-specific refresh age (ECR 6h, GCR 45m; GHCR is not refreshed). Concurrent requests share one mint.
- **Docs.** `schema/tako.schema.json`, `docs/CONFIGURATION.md`, `docs/MACHINE-INTERFACE.md` (redaction guarantees), `examples/22-private-registry/README.md` and `README.md` document the new keys.
- No **BREAKING** changes: existing `username`/`password` entries are unchanged.

## Out of Scope

- `public.ecr.aws`. ECR Public uses `aws ecr-public` and needs auth only for higher rate limits. It can be added later as `provider: ecr-public`.
- Azure Container Registry (`az acr login --expose-token`), Docker Hub access tokens, and a generic `provider: exec` with an arbitrary command. They can be added later as further adapters.
- Minting tokens on nodes (instance roles or workload identity on the VPS). Tako's model keeps credentials on the deploying machine.
- Reading registry passwords from `secretSources` (a separate gap noted in research).
- Pushing images to registries. Tako builds through takod and transfers images.
- Filtering static registry entries by host.

## Capabilities

### New Capabilities
- `registry-auth`: how `registries.<host>` credentials are declared, validated, minted by providers, cached, attached to requests and passed to nodes, and kept out of persistent storage.

### Modified Capabilities
- `secret-redaction`: adds a requirement that minted registry tokens are registered for redaction and never echoed through provider CLI output.

## Impact

One CLI-only PR (design.md "Implementation Order").

- **Shared exec helper:** `pkg/secrets/provider.go:29-31` and `:324-361` move to `internal/providercli`, with an option to keep stdout out of errors.
- **Config:**
  - `pkg/config/types.go:184-187`: `RegistryConfig` gains `Provider`, `Region`, `Profile` and `Account`.
  - `pkg/config/registries.go`: the raw check skips provider entries without a password; `validateRegistries` gains the provider rules.
  - `schema/tako.schema.json:286-304`.
  - The image-host helper `runImageRegistryHost` (`cmd/run.go:145`) moves to a shared package.
- **Minting:** a new `pkg/registryauth/` package (provider adapters, host parsing, in-memory cache, singleflight).
- **Deployer:** `pkg/deployer/registry.go:31` `registryAuths()` becomes `registryAuths(ctx, imageRef) ([]takod.RegistryAuth, error)`. An empty `imageRef` means a build, and the host filter and minter apply. Callers: `pkg/deployer/takod_deploy.go:1686`, `pkg/deployer/run.go:136`, `pkg/deployer/deployer.go:611-631` (build preamble).
- **Engine:**
  - The minter is created per operation with `Engine.RegisterSecretAs` and the label `registries.<host>.token` as its redaction callback.
  - A preflight mint runs before mutation in deploy, rollback, project rollback, scale, promote, run and placement apply.
- **takod:** no change. The request shape (`RegistryAuth{Registry, Username, Password}`) is unchanged, so no new capability is needed.
- **Docs and tests:** see tasks.md.
