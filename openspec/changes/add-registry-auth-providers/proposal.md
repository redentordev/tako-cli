# Proposal

## Why

`registries.<host>` accepts only a static `username`/`password` pair, and the password must be a whole `${ENV_VAR}` reference (`pkg/config/registries.go:28-49`, `:52-72`). Amazon ECR passwords are 12-hour tokens, Google Artifact Registry / GCR access tokens last about an hour, and GHCR tokens normally come from `gh auth token` or the CI-provided `GITHUB_TOKEN`. Every deploy against those registries therefore needs a wrapper script that runs `aws ecr get-login-password`, exports the result, and only then runs Tako (issue #117). Forgetting the wrapper, or a token expiring between export and pull, gives a confusing `image.pull.auth_failed` on the nodes.

Tako already shells out to provider CLIs for secrets (`pkg/secrets/provider.go`, `provider_aws.go`, `provider_gcp.go`: LookPath check, 2-minute timeout, stderr captured, test seams). Minting a registry token is the same shape: run the CLI on the deploying machine, use the value, never persist it.

## What Changes

- `registries.<host>` gains an optional `provider` key with the values `ecr`, `gcr` (alias `artifact-registry`) and `ghcr`. With `provider` set, Tako mints a short-lived credential on the deploying machine at the moment a registry credential is first needed in the operation, and passes it to takod exactly like a static credential today (request-scoped `RegistryAuths`, ephemeral `DOCKER_CONFIG`, deleted after the docker call).
  - `ecr`: `aws ecr get-login-password --region <region> [--profile <profile>]`; username `AWS`. The region is parsed from the host (`<account>.dkr.ecr[-fips].<region>.amazonaws.com[.cn]`) unless `region` is set.
  - `gcr`: `gcloud auth print-access-token [--account <account>]`; username `oauth2accesstoken`. Hosts `gcr.io`, `*.gcr.io` and `*-docker.pkg.dev`.
  - `ghcr`: `GITHUB_TOKEN` (or `GH_TOKEN`) from the environment when set, else `gh auth token`; username from `username` when given, else `GITHUB_ACTOR`, else `gh api user --jq .login`.
- Provider-specific optional keys: `region`, `profile` (ecr), `account` (gcr). Unknown provider names, provider/host mismatches (e.g. `provider: ecr` on `ghcr.io`), and `provider` together with `password` are config errors. `username` together with `provider` is allowed only for `ghcr`.
- Every minted token is registered with the output redactor before it can reach any event, error, or log line, and is never written to disk on the deploying machine or on a node.
- Tokens are cached in memory for the life of one CLI process, keyed by host and provider options, and re-minted when older than a provider-specific refresh age (ECR 6h, GCR 45m, GHCR not refreshed).
- Minting is lazy: a deploy that never pulls or builds with a credential does not need the CLI installed.
- `schema/tako.schema.json`, `docs/CONFIGURATION.md`, `examples/22-private-registry/README.md` and `README.md` document the new keys.
- No **BREAKING** changes: existing `username`/`password` entries are unchanged.

## Out of Scope

- `public.ecr.aws` (ECR Public uses `aws ecr-public` and only needs auth for higher rate limits). It can be added later as `provider: ecr-public`.
- Azure Container Registry (`az acr login --expose-token`), Docker Hub access tokens, and generic `provider: exec` with an arbitrary command. They can be added later as further adapters.
- Minting tokens on nodes (instance roles / workload identity on the VPS). Tako's model keeps credentials on the deploying machine.
- Reading registry passwords from `secretSources` (a separate gap noted in research).
- Pushing images to registries; Tako builds through takod and transfers images.

## Capabilities

### New Capabilities
- `registry-auth`: how `registries.<host>` credentials are declared, validated, minted by providers, cached, passed to nodes, and kept out of persistent storage.

### Modified Capabilities
- `secret-redaction`: adds a requirement that minted registry tokens are registered for redaction.

## Impact

- **Config:** `pkg/config/types.go:184-187` (`RegistryConfig` gains `Provider`, `Region`, `Profile`, `Account`), `pkg/config/registries.go` (raw check skips provider entries without a password; `validateRegistries` gains provider rules), `schema/tako.schema.json:286-304`.
- **Minting:** new `pkg/registryauth/` package (provider adapters, host parsing, in-memory cache) reusing the exec helper pattern of `pkg/secrets/provider.go:324-360` (`runProviderCommand`, `providerCommandContext`, `providerLookPath`).
- **Deployer:** `pkg/deployer/registry.go:31` `registryAuths()` becomes `registryAuths(ctx) ([]takod.RegistryAuth, error)` and calls the minter. Callers: `pkg/deployer/takod_deploy.go:1685`, `pkg/deployer/run.go:136`, `pkg/deployer/deployer.go:611-634` (build preamble).
- **Redaction:** `pkg/engine/engine.go:179` `RegisterRegistrySecrets` plus a redactor hook passed to the minter.
- **takod:** no change. Request shape (`RegistryAuth{Server, Username, Password}`) is unchanged, so no new capability is needed.
- **Docs/tests:** see tasks.md.
