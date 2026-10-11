# Tasks

## 1. takod: network fix, records, listing, volume removal, capability

- [ ] 1.1 Make `cleanupNetworks` (`pkg/takod/cleanup.go:255-285`) remove, for an environment-scoped request, only `runtimeid.NetworkName(project, env)` and export networks whose `tako.environment` label equals `env`; keep the project-wide prefix path for environment-less requests. Verify with a test that destroying `prod` keeps `prod-eu` networks and one that `preview` keeps `preview-pr-42` networks (`go test ./pkg/takod -run CleanupNetworks`)
- [ ] 1.2 Add state document kind `preview` (`stateDocumentPreview`, `pkg/takod/state.go:15-20`) stored at `desired/<project>/<environment>/preview.json` with project/environment identity validation (`validateProjectEnvIdentity`, `state.go:237`); verify write/read/delete round trip and identity mismatch rejection
- [ ] 1.3 Add `GET /v1/state/previews?project=<p>` returning every `preview.json` under `desired/<project>/*/`; register it in `registeredRoutes` (`pkg/takod/lifecycle_gate.go:25-41`) as read-only; verify listing across two environments, empty project, and the lifecycle-gate route test
- [ ] 1.4 Add `CleanupRequest.RemovePreviewVolumes` (`cleanup.go:15-35`): remove volumes labeled exactly `tako.project=<p>` and `tako.environment=<env>`, only when `env` starts with `preview-` and its `preview.json` exists, otherwise reject with an error; verify the guard (`staging` rejected, missing record rejected) and removal with a fake docker runner
- [ ] 1.5 Add `CapabilityPreviewLifecycleV1 = "preview.lifecycle-v1"` with doc comment and advertise it (`pkg/takod/server.go:2636`); update the capability-list tests in `server_test.go`

## 2. Config: preview block, synthesis, policy

- [ ] 2.1 Add `PreviewConfig` (`domain`, `ttl`, `max`, `maxReplicas`, `keepJobs`, `env`, `services.<name>.env`) and `EnvironmentConfig.Preview` (`pkg/config/types.go:778-796`); verify strict decode rejects unknown keys in YAML and JSON
- [ ] 2.2 In `pkg/config/environment_extends.go`, treat `preview` like `abstract`: captured from the declaring node, stripped from resolved nodes, not inherited; record declaring environments in `environmentResolution`; exempt abstract environments with `preview` from `unusedAbstractEnvironments`; verify the "child of a preview template" and "preview template is not unused" scenarios
- [ ] 2.3 Add `LoadOptions{Preview *PreviewInstance}` and `LoadConfigWithOptions`; keep `LoadConfig` delegating with zero options; inject `preview-<name>: {extends: <base>}` before chain resolution in both YAML and JSON adapters; fail on identity collision and on a base without `preview:`; verify the fast path still returns original bytes when no preview is requested (existing `FastPath` test) and add tests for abstract and concrete bases
- [ ] 2.4 Implement name normalization and identity (`NormalizePreviewName`, D2) with a table test (case, separators, 40-char truncation with hash, empty input)
- [ ] 2.5 Implement base selection (D3) as `ResolvePreviewBase(cfg/raw, explicitEnv)` with tests for single base, ambiguous, explicit non-preview env, none
- [ ] 2.6 Implement `applyPreviewPolicy` in new `pkg/config/preview.go` following D4 steps 1-6 (job drop, isolation checks, backup/monitoring strip, replica cap, proxy rewrite, env injection) and expose `cfg.PreviewInstance()`; table tests per step, including `global` placement not capped and `kind: run` kept
- [ ] 2.7 Implement host rendering and validation (D5): placeholders, label/total length, uniqueness within the preview, collision with hosts of declared environments; tests for both template styles and every failure
- [ ] 2.8 Static validation of declared `preview:` blocks in `ValidateConfig` (placeholders, `{url:x}`/`{host:x}` targets, `ttl` incl. `none`, `max`/`maxReplicas` ≥ 1, sample render with `pr-1`) and isolation `warn` findings in `ValidationWarnings`; verify `cmd/validate_test.go` cases
- [ ] 2.9 Add `cfg.SecretsEnvironment(env)` and use it in `secrets.NewManagerForConfig` (`pkg/secrets/sources.go:84`) and the redaction/hook secret registration paths; verify a preview of base `preview` reads `.tako/secrets.preview` and not `secrets.staging`
- [ ] 2.10 Add `preview` (and nested keys) to `schema/tako.schema.json` and assertions in `schema/tako_schema_test.go`; verify `go test ./schema`

## 3. Engine and CLI: deploy

- [ ] 3.1 Add persistent `--preview` flag and the `previewSupported` command annotation; root pre-run rejects `--preview` on unannotated commands with exit 2; extend `getEnvironmentName` (`cmd/root.go:353`) to return the identity; verify a rejection test for `backup` and acceptance for each supported command
- [ ] 3.2 `tako deploy --preview`: load via `LoadConfigWithOptions`, add `--ttl` and `--no-prune`, require `preview.lifecycle-v1` on all target servers before build, check base mismatch from existing record, opportunistic prune (task 4.3), `max` check, then the normal deploy; verify engine tests with fake takod for first deploy, redeploy, limit reached, old node
- [ ] 3.3 Write the `PreviewRecord` to every target server after a successful rollout, under the deploy lease; preserve `createdAt` on redeploy; verify record contents and that a failed rollout writes no new record
- [ ] 3.4 Add `DeployResult.preview` (`pkg/engine/types.go`) and populate it; add events `preview.pruned`/`preview.destroyed` constants in `pkg/takoapi/events/events.go`; verify golden output in `cmd/machine_output_test.go`
- [ ] 3.5 Add `TAKO_PREVIEW_NAME`, `TAKO_PREVIEW_BASE`, `TAKO_PREVIEW_EXPIRES_AT` to the hook env (`pkg/hooks/env.go:63-94`) for preview runs only; verify both scenarios of the deploy-hooks delta
- [ ] 3.6 After `add-deploy-summary` lands: set `summary.environment.kind = "preview"`, `summary.environment.preview`, URL `kind: "preview"`, human line with expiry, and undo `tako preview destroy <name>` on first deploy; verify summary builder tests

## 4. Engine and CLI: list, destroy, prune, other commands

- [ ] 4.1 New `cmd/preview.go` with `preview list` (engine `ListPreviews`: query all config servers, merge by newest `updatedAt`, `expired` status, exit 6 on unreachable servers); verify merge and partial-failure tests
- [ ] 4.2 `preview destroy <name>`: resolve base from record, union services from template and replicated desired state, union servers, run `PlanDestroy`/`Apply` (`pkg/engine/destroy.go:122`, `:236`), then `RemovePreviewVolumes`, then delete record and state; `not_found` when no record; `--yes` required in non-interactive/machine modes; verify full removal, idempotency, and retry after a simulated mid-way failure
- [ ] 4.3 `preview prune [--dry-run]` and the shared prune routine used by deploy: per-preview lease, skip on held lease, exit 6 on any failure; verify expired/active selection and skipped reporting
- [ ] 4.4 Wire `--preview` into `ps`, `logs`, `exec`, `history`, `rollback`, `config explain` (record lookup first, exit 2 when missing); verify one test per command that the identity reaches the engine request
- [ ] 4.5 `tako doctor` warns about expired previews; verify a doctor test with a fake listing
- [ ] 4.6 Register `preview list|destroy|prune` as full contract in `cmd/machine_coverage_test.go` and the coverage table in `docs/MACHINE-INTERFACE.md`; document the result documents, `DeployResult.preview`, events and exit codes there

## 5. Docs, examples, generated files

- [ ] 5.1 New `docs/PREVIEW-ENVIRONMENTS.md`: template setup (abstract `preview` extending staging, dropping services), DNS wildcard requirement, certificates and rate limits, secrets file, isolation rules, TTL/prune, limits, troubleshooting
- [ ] 5.2 `docs/CONFIGURATION.md`: `preview:` block reference and placeholders; note in the environment inheritance section that `preview` is not inherited
- [ ] 5.3 `docs/CI-CD.md`: GitHub Actions example for `pull_request` opened/synchronize (`deploy --preview pr-${{ github.event.number }}`), closed (`preview destroy --yes`), and a nightly `preview prune --yes`
- [ ] 5.4 Add `examples/23-preview-environments` (`tako.yaml`, `.env.example` with placeholders only) and load it in the example-config test
- [ ] 5.5 Regenerate `man/` (`make man`) and `completions/`; verify CI's generated-files check passes
- [ ] 5.6 Full verification: `go build ./...`, `go vet ./...`, gofmt on Linux, `go test ./...` on Linux (WSL) compared against master with no new failures; `openspec validate add-preview-environments --strict`
