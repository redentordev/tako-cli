# Tasks

## 1. Config surface and schema

- [x] 1.1 Add `HooksConfig` (`path`, `timeout`, `output`, `passEnv`, `commands`) as `Config.Hooks` in `pkg/config/types.go`, then verify with a `LoadConfig` test that a config containing `hooks:` loads and that a config without it still loads unchanged.
- [x] 1.2 Add hook validation in `pkg/config/validator.go`. It covers path rules (absolute paths, escaping the project, paths under `.tako` with the gitignore message), timeout bounds of 1s..1h, `output` enum, `passEnv` pattern and `TAKO_` rejection, known hook names, and non-empty argv. Verify with table tests for each rejection, and assert that each error is reported as an invalid config (exit code 2).
- [x] 1.3 Add `validateRawHookCommands` next to `validateRawRegistryCredentials` (`pkg/config/types.go:1243`) to reject `${` inside `hooks.commands`. Verify with tests for YAML and JSON configs, and confirm that the error names `hooks.passEnv`.
- [x] 1.4 Add the `hooks` object to `schema/tako.schema.json` with `additionalProperties: false` and hook-name `propertyNames`. Extend `schema/tako_schema_test.go` to assert the enum and structure, and verify that `go test ./schema/...` passes.
- [x] 1.5 Document the `hooks:` block in `docs/CONFIGURATION.md`, covering keys, defaults, and the passEnv secret rules. Verify that the documented example loads through a config test fixture.

## 2. Hook resolution (`pkg/hooks`)

- [x] 2.1 Create `pkg/hooks` with hook `Name` constants, `Known()`, and the `Invocation` and `Outcome` types. Verify with `go build ./...`.
- [x] 2.2 Implement Unix resolution: the extensionless file, the executable-bit check returning `NotExecutableError` with the chmod and `git update-index` hint, `NoRunnableVariantError` when only Windows variants exist, ignoring `*.sample` and unrelated files, and warnings for hook-like unknown names. Verify with temp-dir tests guarded by `runtime.GOOS`.
- [x] 2.3 Implement Windows resolution: `.ps1` via pwsh then powershell.exe, `.cmd`/`.bat` via ComSpec, `.exe`, the extensionless file via its shebang interpreter or `sh`/`bash`, the exclusion of the System32 WSL `bash.exe`, and `AmbiguousHookError` for multiple native variants. Make `goos` and PATH lookup injectable, and verify with table tests on any OS.
- [x] 2.4 Merge `hooks.commands` with file hooks, raising `AmbiguousHookError` when both define the same hook, and resolve argv[0] with `exec.LookPath`. Verify with tests.

## 3. Hook environment (`pkg/hooks`)

- [x] 3.1 Implement `BuildEnv`:
  - Platform baseline allowlist.
  - `passEnv` exact and prefix matching.
  - Dropping inherited `TAKO_*` variables.
  - `TAKO_*` context rendering, with comma-joined sorted lists, RFC 3339 UTC timestamps, and 4 KiB truncation of `TAKO_ERROR`.

  Verify with tests:
  - A `.env`-style variable set in the process environment is absent unless listed in `passEnv`.
  - Prefix patterns match.
  - Inherited `TAKO_*` values are not passed through.
- [x] 3.2 Add the credential-name heuristic that selects which `passEnv` values are registered with the redactor. Verify with tests that `GITHUB_TOKEN` is registered and `NODE_ENV` is not.

## 4. Hook runner, events, and process control

- [x] 4.1 Add `PhaseHooks` and the `hook.started`, `hook.output`, `hook.completed`, `hook.failed`, and `hook.skipped` constants to `pkg/takoapi/events/events.go`. Verify with `go test ./pkg/takoapi/...`.
- [x] 4.2 Implement `Runner.Run`:
  - Null stdin.
  - Line-scanned stdout and stderr, with a 1 MiB cap, emitted as `hook.output` with `data.hook`, `data.stream`, and `data.data`.
  - Info or debug level according to `hooks.output`.
  - Started and terminal events carrying `exitCode`, `durationMs`, `error`, and `timedOut`.

  Verify with a test that runs a helper process (Go test binary re-exec) and asserts the event sequence and fields.
- [x] 4.3 Implement process-tree termination: `proc_unix.go` with Setpgid and SIGTERM/SIGKILL on the process group, and `proc_windows.go` with a Job Object set to `KILL_ON_JOB_CLOSE`. Use `cmd.Cancel` and `WaitDelay`. Verify with a test where the hook spawns a child and exceeds a 2s timeout: the outcome must be `timed_out` and the child must no longer be running.
- [x] 4.4 Verify that cancelling the parent context mid-hook returns an error satisfying `errors.Is(err, context.Canceled)`, so `engine.Classify` maps it to exit code 5.

## 5. Engine types and seams

- [x] 5.1 Add `HookOutcome` and an `omitempty` `Hooks` field to `DeployResult`, `RollbackResult`, `PromoteResult`, and `SetupResult`. Add `DeployResult.DeploymentID`, populated after `RecordStartedDeploymentStateContext` (`pkg/engine/deploy.go:853`). Verify with a JSON test that a result without hooks serializes byte-identically to today, apart from `deploymentId` on recorded deploys.
- [x] 5.2 Add `engine.HookFailedError`, classified as `ClassFailed`, which still unwraps to context errors. Verify with `Classify` table tests: exit 1 for a hook exit, and exit 5 for a wrapped `context.Canceled`.
- [x] 5.3 Add `BeforeMutation func(ctx, MutationInfo) error` to `RollbackRequest` and call it after target selection (`pkg/engine/rollback.go:226`). Verify with an engine test that a returned error causes no mutation calls and releases the lock and leases.
  - Note: rollback needs live SSH leases and a takod client before target selection, and no engine fakes exist for them. Verified instead with `callBeforeMutation` unit tests, early-rejection tests (the callback is not reached), and cmd tests through the engine seam. Lock and lease release relies on the existing deferred releases that wrap the call.
- [x] 5.4 Add `BeforeMutation` to `PromoteRequest` and call it after revision and image selection (`pkg/engine/promote.go:206`). Verify with an engine test that a returned error causes no proxy switch.
  - Note: verified the same way as 5.3. The call sits before `PreflightAssignmentMutations` and any proxy switch, and the cmd tests assert that a failing `pre-promote` stops the engine before its mutation step.

## 6. CLI wiring

- [x] 6.1 Create `cmd/hooks.go`:
  - A `hookSession` that resolves hooks at command start relative to the config directory.
  - A `--skip-hooks` downgrade that emits `hook.skipped`.
  - Secret registration through the existing engine `Register*` helpers.
  - Outcome collection.
  - Pre, post, and on-failure helpers that apply the D7 error wrapping (`HookFailedError`, `AttentionError`, and warning-only for on-failure).

  Verify with unit tests using a fake runner.
- [x] 6.2 Wire `tako deploy`:
  - Resolve hooks before `PlanDeploy`.
  - Run `pre-deploy` after the confirmation gate and before `commitDeployClusterAttachment` (`cmd/deploy.go:169-170`).
  - On pre-hook failure, emit a minimal failed `DeployResult`.
  - After Apply, call `session.Close()`, then run `post-deploy` or `on-failure`, then emit the result.
  - Add the `--skip-hooks` flag.

  Verify with cmd tests that `--plan-only`, plan drift, and `ConfirmationRequired` run no hooks, and that a failing `post-deploy` yields exit 6 with `status: success`.
- [x] 6.3 Wire `tako rollback` and `tako promote` through `BeforeMutation` for the pre hooks, run post or on-failure hooks after the engine returns, and add `--skip-hooks`. Verify with cmd tests using fake engine seams that `TAKO_ROLLBACK_TARGET` and `TAKO_REVISION` reach the hook environment.
- [x] 6.4 Wire `tako setup`: run `pre-setup` before the node loop and `post-setup` or `on-failure` after it (`cmd/setup.go:73-93`), and add `--skip-hooks`. Verify with a cmd test.
- [x] 6.5 Add a machine-output test that runs a hook printing to stdout under `--output json` and under `--events ndjson`:
  - Under `--output json`, stdout parses as a single JSON document.
  - Under `--events ndjson`, every stdout line is a valid event, `hook.output` precedes the terminal `result` event, and the result carries `hooks`.

  Verify with `go test ./cmd/ -run Hook`.
- [x] 6.6 Wire hook checks into `tako validate` (`cmd/validate.go`, design D12): call `hooks.Resolve` after the config loads, without executing anything. Make `Resolve` collect all errors and hook-like unknown names instead of stopping at the first. Map resolution errors to `error` findings (exit 2) and hook-like unknown names to `warn` findings. Verify with cmd tests for a non-executable hook (Unix-guarded), an ambiguous file-plus-command hook, misspelled names (`pre_deploy`, `post-deploi`) that stay `valid: true`, a project with no hooks (no findings), and an NDJSON run asserting no `hook.*` events and no process start.
- [x] 6.7 Regenerate man pages (`make man`) and any completions affected by `--skip-hooks`, and confirm the CI generated-artifact check passes locally (`git diff --exit-code man/ completions/` after regeneration).

## 7. Documentation

- [x] 7.1 Write `docs/HOOKS.md` covering:
  - Hook names and their placement relative to locks and leases.
  - Directory layout and Windows resolution rules.
  - `hooks.commands`.
  - The environment variable table and secret rules.
  - Output and events.
  - Failure and exit-code semantics, including the `|| true` pattern for non-critical post hooks so they do not turn a deploy into exit 6.
  - Checking hooks offline with `tako validate`.
  - Timeouts and `--skip-hooks`.
  - Kamal migration notes.
  - The difference from `deploy.release`.

  Verify that every `TAKO_*` variable named in the spec appears in the table.
- [x] 7.2 Update `docs/MACHINE-INTERFACE.md` with the new hook events and phase, the `hooks` and `deploymentId` result fields, and how hook failures map onto exit codes 1, 5, and 6. Verify against the event constants added in 4.1.
- [x] 7.3 Update `docs/CI-CD.md` with the executable bit from Windows (`git update-index --chmod=+x`), `passEnv` for CI tokens, and `--skip-hooks` for emergency deploys. Add a README feature bullet and verify that the links resolve.

## 8. Integration checks

- [ ] 8.1 Run `go build ./...` and `go test ./...` on Linux CI and confirm that they pass. On Windows, confirm that any failures are limited to the known Windows-only cases (symlinks, directory fsync, POSIX paths).
- [ ] 8.2 Run a manual end-to-end check against a test VPS with `pre-deploy`, `post-deploy`, and `on-failure` hooks. Run it once from Linux with shell scripts and once from Windows with `.ps1`. Confirm ordering, environment contents (no `.env` secrets), and exit codes 0, 1, and 6, and record the transcript in the PR description.
