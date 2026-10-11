# Tasks

All tasks land in one CLI-only PR, in this order (design.md "Implementation Order").

## 1. Shared exec helper

- [ ] 1.1 Move `runProviderCommand`, the seams and the LookPath message from `pkg/secrets/provider.go:29-31,324-361` to `internal/providercli`. Add an option that keeps stdout out of error messages and truncates stderr. `pkg/secrets` keeps its current behavior, and `SetProviderCommandForTesting` keeps faking both users. Verify that:
  - `go test ./pkg/secrets/...` passes unchanged;
  - a helper test shows that, with the option set, a failing command with empty stderr and a token on stdout yields an error without the token.

## 2. Config and validation

- [ ] 2.1 Add `Provider`, `Region`, `Profile` and `Account` (all `omitempty`) to `RegistryConfig` (`pkg/config/types.go:184-187`). Verify with a decode test for YAML and JSON.
- [ ] 2.2 Update `validateRawRegistryCredentials` so provider entries without a password pass, and add the D6 rules to `validateRegistries`. Verify with table tests in `pkg/config/registries_test.go`:
  - ECR host parsing (standard, fips, `.cn`);
  - gcr and artifact-registry hosts;
  - ghcr;
  - a mismatched host;
  - password plus provider;
  - username with ecr;
  - an explicit region that conflicts with the host;
  - an unknown provider.
- [ ] 2.3 Update `schema/tako.schema.json:286-304` (`oneOf` required sets, `provider` enum, option keys). Verify with the `schema/tako_schema_test.go` drift checks.
- [ ] 2.4 Move `runImageRegistryHost` (`cmd/run.go:145`) to a shared package, and keep `cmd/run.go` using it. Verify with the existing `cmd/run_test.go` and a table test covering bare, Docker Hub, port-qualified and `localhost` references.

## 3. Minting

- [ ] 3.1 Add `pkg/registryauth` with:
  - the D2 adapters and host parsing;
  - output checks (trimmed, 16 characters to 4096 bytes, no control characters);
  - `Minter` (cache, refresh ages, `singleflight`, a `register` callback invoked before return).

  Verify with re-exec fake tests:
  - the exact argv per provider, including region, profile and account flags;
  - the ghcr token order `GH_TOKEN`, `GITHUB_TOKEN`, then `gh auth token`, and the username order;
  - empty, short, oversized and control-character output rejected without echoing it;
  - the missing-CLI message;
  - a re-mint after the refresh age;
  - one mint for concurrent callers;
  - `register` called before `Auth` returns;
  - a failing CLI whose stdout holds a token yields an error without it.

## 4. Wiring

- [ ] 4.1 Change `registryAuths()` (`pkg/deployer/registry.go:31`) to `registryAuths(ctx, imageRef) ([]takod.RegistryAuth, error)`, implementing D3:
  - static entries are unchanged;
  - provider entries are filtered to the image's host for pulls;
  - all provider entries are attached for builds.

  Update `takod_deploy.go:1686`, `run.go:136` and `deployer.go:611-631`. Verify with deployer tests:
  - an ECR provider entry yields `{Registry, Username: "AWS", Password: <fake>}` for an ECR image;
  - a `docker.io` image pull mints nothing;
  - a build attaches every provider entry;
  - no CLI runs when `pullImage` is false;
  - static-only configs produce byte-identical requests.
- [ ] 4.2 Create the minter per operation in the engine, with `RegisterSecret` as the callback. Add a preflight mint (D4) before the first mutation in deploy, rollback, project rollback, scale, promote, `run` and placement apply. It mints exactly the provider entries those operations will attach, computed from the same inputs that decide `PullImage` and remote builds. Map mint failures to exit 1. Verify with engine tests:
  - a mint failure in each operation fails before any takod mutation, with exit 1;
  - an operation that attaches no provider credential runs no CLI.
- [ ] 4.3 Add the ghcr scope hint to `wrapRegistryAuthError` (`pkg/deployer/registry.go:58`) for provider entries. Verify with a unit test.

## 5. Redaction and persistence

- [ ] 5.1 Test that a minted token never appears in human output, NDJSON events, result documents, or errors that embed CLI stderr or docker output. Verify with an engine test using a fake minter and a docker error that echoes the token.
- [ ] 5.2 Test that no token is written to `.tako/`, deployment history or takod desired state. Verify by scanning the files written in an engine test, and with a takod test pinning that persisted desired state has no `registryAuths`.

## 6. Docs and verification

- [ ] 6.1 Update:
  - `docs/CONFIGURATION.md` (registries section: providers, the host rules, the `GH_TOKEN` precedence, and that builds mint every provider entry);
  - `examples/22-private-registry/README.md`;
  - `README.md:218,289,442`;
  - `docs/ORCHESTRATION-MODEL.md:185`;
  - `docs/BUILD-DEPLOY-OPTIMIZATION.md:76`.

  Verify that the examples pass `tako config validate`.
- [ ] 6.2 In `docs/MACHINE-INTERFACE.md` "Redaction Guarantees" (`:795-804`), add that provider-minted registry tokens are registered before use, never appear in errors or events, and are never persisted. Under "Exit Codes", note that a failed mint exits 1 before mutation.
- [ ] 6.3 Run `go build ./...` and `go test ./...`. Compare failures against master, because the Windows/WSL baseline has known failures.
