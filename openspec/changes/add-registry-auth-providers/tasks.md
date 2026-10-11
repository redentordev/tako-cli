# Tasks

## 1. Config and validation

- [ ] 1.1 Add `Provider`, `Region`, `Profile`, `Account` (all `omitempty`) to `RegistryConfig` (`pkg/config/types.go:184-187`). Verify with a decode test for YAML and JSON.
- [ ] 1.2 Update `validateRawRegistryCredentials` so provider entries without a password pass, and `validateRegistries` with the D5 rules (mutual exclusion, per-provider options, host pattern, explicit region vs host region). Verify with table tests in `pkg/config/registries_test.go`: ecr host parse (standard, fips, `.cn`), gcr/artifact-registry hosts, ghcr, mismatched host, password+provider, username with ecr, unknown provider.
- [ ] 1.3 Update `schema/tako.schema.json:286-304` (`oneOf` required sets, `provider` enum, option keys). Verify with `schema/tako_schema_test.go` drift checks.

## 2. Minting

- [ ] 2.1 Extract the CLI exec helper and seams from `pkg/secrets/provider.go:29-31,324-360` into a shared helper used by both packages; keep `SetProviderCommandForTesting` working for both. Verify that `go test ./pkg/secrets/...` passes unchanged.
- [ ] 2.2 Add `pkg/registryauth` with the D2 adapters, host parsing, output checks, and `Minter` (cache, refresh ages, singleflight, `register` callback). Verify with re-exec fake tests: exact argv per provider (region/profile/account flags), username resolution order for ghcr, empty/oversized output rejected, CLI missing message, refresh after age, one mint for concurrent callers, register called before return.

## 3. Wiring

- [ ] 3.1 Change `registryAuths()` (`pkg/deployer/registry.go:31`) to `registryAuths(ctx) ([]takod.RegistryAuth, error)` using the minter; update `takod_deploy.go:1685`, `run.go:136`, `deployer.go:611-634`. Verify with a deployer test that a provider entry produces `{Server, Username: "AWS", Password: <fake>}` and that no CLI runs when `pullImage` is false.
- [ ] 3.2 Construct the minter in the engine with `RegisterSecret` as the callback (`deploy.go:248`, `rollback_project.go:298`, `run.go:116`, single-service rollback, scale, promote). Add a deploy preflight mint when the plan has pulls or authenticated builds. Verify with an engine test that a mint failure fails before any mutation.
- [ ] 3.3 Add the ghcr scope hint to `wrapRegistryAuthError` (`pkg/deployer/registry.go:58`) for provider entries. Verify with a unit test.
- [ ] 3.4 Make `config explain` show `provider` and a `<minted at deploy>` placeholder. Verify with `cmd/config` tests.

## 4. Redaction and persistence

- [ ] 4.1 Test that a minted token never appears in human output, NDJSON events, result documents, or errors that embed CLI stderr or docker output. Verify with an engine test using a fake minter and a docker error echoing the token.
- [ ] 4.2 Test that no token is written to `.tako/`, deployment history, or takod desired state. Verify by scanning written files in an engine test.

## 5. Docs

- [ ] 5.1 Update `docs/CONFIGURATION.md` (registries section), `examples/22-private-registry/README.md`, `README.md:218,289,442`, `docs/ORCHESTRATION-MODEL.md:185`, and `docs/BUILD-DEPLOY-OPTIMIZATION.md:76`. Verify that examples pass `tako config validate`.
- [ ] 5.2 Run `go build ./...` and `go test ./...`.
