# Design

## Context

See proposal.md for the motivation and specs/deploy-hooks/spec.md for the behavior contract. This section records only the current-code facts that shape the approach. Each one is labelled **Observed** (read in the code at the cited location) or **Proposed**.

**Deploy lifecycle (Observed)**
- `cmd/deploy.go:105-181` `runDeploy` runs these steps in order:
  1. Load the config.
  2. `prepareDeployClusterAttachment` (:111).
  3. `PlanDeploy` (:138).
  4. Defer `session.Close()` (:142).
  5. `--plan` hash verification (:144-148).
  6. Return early for `--plan-only` (:149-151).
  7. The confirmation gate, which in machine modes emits `ConfirmationRequired` and returns a `ConfirmationRequiredError` (:153-169).
  8. `commitDeployClusterAttachment` (:170).
  9. `session.Apply` (:174).
  10. `emitResultDocument` (:176).
- `pkg/engine/deploy.go:129-577` `PlanDeploy` does the following:
  - Resolves git/source info (:194).
  - Takes the local state lock (:204-214).
  - Registers secrets with the redactor (:229-256).
  - Acquires remote leases (:418-431).
  - Computes the plan and emits `plan.computed` (:532-573).
  - The session keeps the leases until `Close` (:97-117).
- `DeploySession.Apply` (:626-1326) does all builds (`buildSharedImages` :857, then per-service `DeployServiceTakod` :936-941) and all mutations. No build happens before Apply, so a Kamal-style `pre-build` point would fall at the same place as `pre-deploy`.
- Apply records in-progress history at :853 (`RecordStartedDeploymentStateContext`). On success it returns a result (:1322-1325). On failure it returns `result` plus an error (:1139-1189). Domain-check problems after success return `result` and an error (:1304-1317), which can be an attention-class error.
- `DeployResult` (`pkg/engine/types.go`) has no deployment ID field. The ID is assigned by the state saver (`pkg/engine/state.go:67`).

**Rollback and promote (Observed)**
- These are single engine methods that take the lock and leases internally and release them with `defer`:
  - Rollback: `pkg/engine/rollback.go:78-165`, target chosen at :215-226 before any mutation.
  - Promote: `pkg/engine/promote.go:61-142`, revision and image selected at :202-206.
- The CLI wrappers are `cmd/rollback.go:43-77` and `cmd/promote.go:32-55`. Each emits the result after the engine returns.

**Setup (Observed)**
- `cmd/setup.go:48-94` loops over nodes with no leases, aborts on the first failing node, and always emits a `SetupResult` (`pkg/engine/setup.go:75`).

**Leases (Observed)**
- Leases renew in the background (`pkg/engine/leases.go:158`, `startRenewal` :615-668). A long hook under a held lease does not expire it.

**Machine output (Observed)**
- In machine modes, stdout is reserved (`cmd/engine.go:23-48`, `machineOutputEnabled` :52).
- `humanEventSink` writes event messages to stdout in text mode and to stderr in machine modes. It routes `data.stream == "stderr"` to stderr (:177-193).
- The event stream redacts `Message` and top-level string `Data` values (`pkg/takoapi/events/sinks.go:106-129`), using the engine redactor (`pkg/engine/engine.go:90-100`). That redactor ignores values shorter than 4 characters (`pkg/secrets/redactor.go:23,82`).
- Exit codes come from `engine.Classify` (`pkg/engine/errors.go:85-118`) through `cmd/root.go:138-159`. The process context is cancelled on SIGINT/SIGTERM (`cmd/root.go:125`).
- Release commands already establish a streamed-output event pattern: `deploy.release.started/output/completed/failed` with `data.data`, `exitCode`, and `durationMs` (`pkg/deployer/release.go:144-199,221-266`).

**Secrets in the process environment (Observed)**
- `config.LoadConfig` copies every key of the project `.env` into the process environment with `os.Setenv` (`pkg/config/types.go:1217-1233`).
- It then expands `${VAR}` across the whole raw file (`expandEnvWithTrim` :1141-1169).
- Raw-content credential checks run before expansion (`validateRawRegistryCredentials` :1243).
- Unknown top-level keys are rejected (`pkg/config/types_test.go:23-47`), so `hooks:` must be added to `Config` (`pkg/config/types.go:23-54`).

**Git ignore behavior (Observed)**
- `tako init` writes `.tako/` (the whole directory) into the project `.gitignore` (`cmd/init.go:136-158`). The repository `.gitignore` also ignores `.tako/`.
- `pkg/secrets/manager.go:27-37` additionally writes `.tako/.gitignore` with `secrets*`, `*.key`, `*.env`, state, and logs.
- Kamal-style `.tako/hooks/` would therefore not be committable in a `tako init` project.

**Schema and coverage (Observed)**
- `schema/tako.schema.json` is hand-maintained and asserted by `schema/tako_schema_test.go`.
- No new commands are added, so `cmd/machine_coverage_test.go` needs no new entries.

## Goals / Non-Goals

**Goals:**
- One hook runner package used by four commands, with identical resolution, environment, output, timeout, and failure behavior.
- Hooks are observable only through the existing event stream. There is no new output channel.
- Fail closed when a defined hook cannot run. Never skip a hook silently.
- Zero behavior change for projects without hooks: no events and no result fields.

**Non-Goals:**
- A plugin API, remote hooks, or hook chaining (multiple scripts per hook).
- Changing `deploy.release` semantics.
- Making hooks available to `pkg/engine` library consumers by default. The engine gains only nil-safe callback seams.

## Decisions

### D1. Package and layering: `pkg/hooks` plus CLI wiring, with narrow engine seams
**Proposed.**
- New package `pkg/hooks` with these parts:
  - `Name` constants and a `Known()` list.
  - `Resolve(dir, cfg.Hooks, goos)`, which returns `map[Name]Invocation` or a typed resolution error.
  - `BuildEnv(base []string, passEnv []string, ctx Context, goos)`.
  - `Runner.Run(ctx, Invocation, env, sink events.Sink, level)`, which returns an `Outcome` with name, status, exitCode, and durationMs.
- `cmd/hooks.go` owns the orchestration. It holds a per-invocation `hookSession` with the resolved hooks, skip flag, outcomes, and failure tracking. It uses `cliEngine().EventStream()` as the sink so output is redacted and routed exactly like other events.
- `RollbackRequest` and `PromoteRequest` gain `BeforeMutation func(ctx context.Context, info MutationInfo) error`. Rollback calls it after `rollback.go:226`, once the target is shown and before notifications or mutation. Promote calls it after `promote.go:206`. `MutationInfo` carries the services, servers, target deployment ID, revision, and image.
- When the seam is nil, behavior is unchanged.
- Deploy needs no seam. `pre-deploy` runs in `runDeploy` between the confirmation gate and `commitDeployClusterAttachment` (`cmd/deploy.go:169-170`), while the session still holds the lock and leases.
- **Alternatives considered:**
  - Engine-level `Options.Hooks` runner: rejected. It couples the engine to local process execution and to the CLI's working directory.
  - Pure cmd-level pre hooks for rollback and promote, run before calling the engine: rejected. The hook would not know the rollback target or promote revision, and could race a concurrent operation between its check and the lease acquisition.

### D2. Lease interaction
**Proposed.**
- `pre-*` hooks run while the lock and leases are held, so whatever the hook validated stays true until mutation. Background renewal (leases.go:158) makes long hooks safe up to `hooks.timeout` (max 1h).
- `post-*` and `on-failure` hooks run after the lock and leases are released, so a slow smoke test does not block other operators:
  - Deploy: cmd calls `session.Close()` explicitly before post hooks. `Close` is idempotent (deploy.go:97-101).
  - Rollback and promote: the engine's defers have already run when it returns.
- `pre-setup` and `post-setup` hold nothing, because setup takes no leases.
- **Alternative considered:** pre-deploy before `PlanDeploy` (Kamal's pre-connect timing). Rejected. The hook could not see the plan hash or empty or destructive flags, and a failure there would not protect against a concurrent deploy.

### D3. Hook location
**Proposed.**
- The default is `.tako-hooks/`, relative to `filepath.Dir(configPath)`. The config path comes from `resolveDeployConfigPath` (cmd/deploy.go:92) or the equivalent for the other commands.
- The location is configurable with `hooks.path`. Validation rejects absolute paths, paths that escape after `filepath.Clean`, and paths under `.tako`. That last case gets a dedicated message citing the gitignore.
- **Alternatives considered:**
  - `hooks/`: collides with application source, for example React `hooks/` directories.
  - `.tako/hooks/`: ignored by the `.tako/` gitignore line.
  - `.tako/hooks` with a `!hooks/` negation in `.tako/.gitignore`: does not work, because a parent-directory ignore in the root `.gitignore` cannot be re-included.

### D4. Cross-platform execution
**Proposed.** `Resolve` scans the directory and, for each known hook name, does the following:
- **Unix:** use only the extensionless `<name>`.
  - If it exists but `mode&0111 == 0`, return a `NotExecutableError` with the `chmod +x` / `git update-index --chmod=+x` hint. This is the common case when hooks are authored on Windows and run in Linux CI.
  - If only Windows variants exist, return a `NoRunnableVariantError`.
- **Windows:** native variants first.
  - `.ps1` runs as `pwsh` (or `powershell.exe`) `-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File <path>`.
  - `.cmd` and `.bat` run as `%ComSpec% /d /c <path>`.
  - `.exe` runs directly.
  - More than one native variant is an `AmbiguousHookError`.
  - Otherwise, the extensionless file is run through an interpreter found from its shebang. Tako takes the basename of the shebang path, or the argument after `env`, and looks it up on PATH. With no shebang it uses `sh`, then `bash`. The lookup skips `%SystemRoot%\System32\bash.exe` (the WSL launcher, which runs in a different filesystem).
- `*.sample` files and files that do not match a hook name are ignored. Unknown names that look like hooks (`pre-*`, `post-*`, `on-*`) produce one `warning` event so typos such as `pre_deploy` are visible.
- `hooks.commands[name]` is an argv list run with `exec.Command(argv[0], argv[1:]...)` in the project directory after `exec.LookPath`. A name that has both a file and a command is an `AmbiguousHookError`.
- Resolution runs at command start, before `PlanDeploy` or the engine call. A broken hook setup therefore fails with exit code 2 (`InvalidRequestError`) before any lock is taken.
- With `--skip-hooks`, resolution errors are downgraded to a debug event.
- **Alternatives considered:**
  - Requiring `sh` everywhere: breaks Windows machines without Git Bash.
  - Config commands only: loses Kamal-style executable scripts, which can carry their own logic and shebangs.
  - Running through a shell string: introduces quoting differences across platforms and injection risk.

### D5. Environment construction (secret safety)
**Proposed.** The hook environment is built from scratch rather than from `os.Environ()`. `os.Environ()` contains every `.env` key (types.go:1229-1231) and any `${VAR}` sources. The environment has three parts:
- **Baseline allowlist** (copied if set):
  - `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TERM`, `LANG`, `LC_*`, `TZ`, `TMPDIR`, `CI`.
  - On Windows also: `USERPROFILE`, `USERNAME`, `TEMP`, `TMP`, `SystemRoot`, `SystemDrive`, `windir`, `ComSpec`, `PATHEXT`, `APPDATA`, `LOCALAPPDATA`, `ProgramData`, `ProgramFiles`, `ProgramFiles(x86)`, `PSModulePath`.
- **`hooks.passEnv`:** exact names or a single trailing `*` prefix pattern. These are matched against the process environment, so `.env`-loaded keys are reachable only when named explicitly. This is the "explicitly requested" exception.
  - Values of passed variables whose names match the redactor's credential-name heuristics (`TOKEN|SECRET|KEY|PASSWORD|PASS|CREDENTIAL|AUTH`) are registered with the engine redactor. Their echoes in hook output are then masked.
  - Values of other passed variables are not registered. Registering a value such as `NODE_ENV=production` would redact the environment name from every event.
- **`TAKO_*` context:** any inherited `TAKO_*` variable is dropped first. Context values are assembled from data the command already holds:
  - Deploy: `planDoc` (Services, Servers, Revision, Source, Git, `Hash()`, Empty, Destructive) and `DeployResult`.
  - Rollback and promote: `MutationInfo` and the result.
  - Hosts come from `cfg.Servers[name].Host`.
  - `TAKO_PERFORMER` is `remotestate.GetCurrentUser()` (`internal/state/manager.go:398`).
  - `TAKO_VERSION` is `cmd.Version`.
  - `TAKO_ERROR` is `cliEngine().RedactSensitive(err.Error())`, truncated to 4 KiB.
  - `TAKO_ERROR_CLASS` uses the class names that `Classify` maps to exit codes.
- **Secret registration for setup:** `tako setup` does not currently register config secrets before emitting events. Before running hooks, `cmd/hooks.go` registers server passwords, service env values, registry and ACME credentials, and notification URLs, using the existing `RegisterSecret`, `RegisterRegistrySecrets`, and `RegisterACMEDNSSecrets` engine methods. This guarantees hook output is redacted uniformly.
- **Raw config check:** a new `validateRawHookCommands` runs next to `validateRawRegistryCredentials` (types.go:1243). It rejects `${` inside `hooks.commands`, so secrets cannot be expanded into argv. Argv appears in `hook.started` data and in process listings.
- **Alternatives considered:**
  - Inheriting the environment and stripping `.env` keys and `${VAR}`-referenced names: fragile. Provider tokens such as `VAULT_TOKEN` are not referenced in the config but are still secrets.
  - Kamal's full inheritance: violates the project rule that secrets never reach hook environments unless explicitly requested.

### D6. Output and events
**Proposed.**
- The hook stdin is `nil`, which Go maps to the null device.
- stdout and stderr are read through two `bufio.Scanner`s with a 1 MiB line cap, matching release.go:237. Each line becomes a `hook.output` event with these fields:
  - Phase `hooks` (new `events.PhaseHooks`), level `info`, or `debug` when `hooks.output: quiet`.
  - `Message` set to the line with a newline.
  - `Data{"hook", "stream": "stdout"|"stderr", "data": line}`.
- The existing `humanEventSink` (cmd/engine.go:177-193) prints the message to stdout in text mode, to stderr in machine modes, and to stderr for `stream=stderr`. Machine stdout never receives raw hook bytes, and the NDJSON `seq` ordering keeps hook lines between `hook.started` and the terminal hook event.
- New constants in `pkg/takoapi/events/events.go`: `hook.started`, `hook.output`, `hook.completed`, `hook.failed`, `hook.skipped`, and `PhaseHooks`. These are additive under `v1alpha1`, as allowed by docs/MACHINE-INTERFACE.md "Compatibility Promises".
- `hook.started` data contains `hook`, `source` (`file`|`config`), `path` (relative to the project) or `argv`, and `timeoutSeconds`.
- **Alternative considered:** passing hook stdio straight through to the terminal in text mode. Rejected. It bypasses redaction and makes text and machine modes behave differently.

### D7. Failure semantics and exit codes
**Proposed.** No new exit code is introduced; codes 0-6 stay stable.

- **Pre-hook failure** (non-zero exit, start error, or timeout):
  - A new `engine.HookFailedError{Hook, ExitCode, TimedOut, Err}` is classified as `ClassFailed`, giving exit code 1. It is an operation that ran and failed by policy.
  - Context cancellation still classifies as `ClassCancelled` (5), because `Classify` checks `context.Canceled` first (errors.go:89-91) and the error wraps `ctx.Err()`.
  - For deploy, cmd builds a minimal `DeployResult` (status `failed`, error, `planHash`, `hooks`), because `Apply` was never called.
  - For rollback and promote, the `BeforeMutation` error propagates out of the engine. Today the engine returns `nil` results on early errors, so cmd builds a minimal failed result there too.
  - No history entry is written, which matches other pre-mutation failures.
- **Post-hook failure:**
  - The error is wrapped in `engine.AttentionError` (exit code 6) unless the operation error is already non-nil.
  - If the operation already returned an attention error (domain checks, `deploy.go:1311-1316`), the code stays 6.
  - The result status is not modified.
  - **Alternative considered:** a warning with exit code 0. Rejected, because CI smoke-test hooks would then pass silently.
  - **Resolved:** exit 6 is kept. Operators whose post hook is non-critical (a chat notification, a cache warm) make it exit 0 themselves, for example `notify-chat ... || true` in a shell script or `exit 0` at the end of a `.ps1`. `docs/HOOKS.md` shows this pattern. Tako adds no per-hook "ignore failure" setting.
- **`on-failure`:**
  - Runs once when an operation that reached its pre-hook stage fails, including the pre-hook's own failure and cancellation. A fresh `context.Background()` with the hook timeout is used, because the command context may be cancelled.
  - Its own failure emits `hook.failed` plus a `warning`. The result's `hooks` entry is recorded and the original error is returned unchanged.
  - Failures before that stage (config, lock contention, connectivity during planning) do not run it. Wrapper scripts can use exit codes for those.

### D8. Timeouts and process-tree termination
**Proposed.**
- `hooks.timeout` is parsed with `time.ParseDuration` and must be within [1s, 1h]. The default is 5m, mirroring `DefaultReleaseTimeout` (release.go:21).
- The runner uses `exec.CommandContext` with a custom `Cancel` and `WaitDelay = 10s`:
  - **Unix:** `SysProcAttr{Setpgid: true}`. Cancel sends SIGTERM to `-pgid`, and SIGKILL follows after `WaitDelay`.
  - **Windows:** the process is assigned to a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, using `golang.org/x/sys/windows`, which is already in go.mod:36. Cancel terminates the job.
- Build tags select the implementation: `proc_unix.go` and `proc_windows.go`.

### D9. Result documents
**Proposed.**
- `HookOutcome{Name, Status, ExitCode *int, DurationMs int64}` is defined in `pkg/engine/types.go`.
- `Hooks []HookOutcome \`json:"hooks,omitempty"\`` is added to `DeployResult`, `RollbackResult`, `PromoteResult`, and `SetupResult`.
- `DeployResult.DeploymentID \`json:"deploymentId,omitempty"\`` is populated from `deployment.ID` after `RecordStartedDeploymentStateContext` (deploy.go:853). It feeds `TAKO_DEPLOYMENT_ID`.
- Ordering: post hooks must run before `emitResultDocument`, so that NDJSON's terminal `result` event stays last and includes the post-hook outcomes. On the deploy happy path the order is:
  1. Apply.
  2. Close.
  3. post-deploy.
  4. emit result.

### D10. Config surface
**Proposed.**

```yaml
hooks:
  path: .tako-hooks          # default
  timeout: 5m                # 1s..1h
  output: verbose            # verbose | quiet
  passEnv: [GITHUB_TOKEN, SLACK_*]
  commands:
    pre-deploy: ["make", "check"]
```

- Implemented as `Config.Hooks *HooksConfig` with yaml and json tags. Validation lives in `pkg/config/validator.go`.
- The schema gets a `hooks` object with `additionalProperties: false`, a `commands` object with `propertyNames` enumerating the hook names, and `minItems: 1` arrays of strings.
- `passEnv` entries must match `^[A-Za-z_][A-Za-z0-9_]*\*?$`. Names starting with `TAKO_` are rejected, because Tako owns that namespace.
- `hooks.output` accepts only `verbose` and `quiet` in this change. An `events-only` value for CI logs is deferred to future work; adding it later is a new enum value and is not breaking.

### D11. takod capability gate
**Proposed.** None is required. Hooks execute only in the CLI process, and no request to takod changes. A newer CLI with an older takod behaves identically apart from hooks.

### D12. `tako validate` checks hooks without running them
**Proposed (resolved open question).**
- `runValidate` (`cmd/validate.go:35`) calls `hooks.Resolve(dir, cfg.Hooks, runtime.GOOS)` after the config loads, with `dir` derived from the config path exactly as the lifecycle commands do (D3). Nothing is executed: no hook process starts and no `hook.*` events are emitted.
- Each resolution error (`NotExecutableError`, `NoRunnableVariantError`, `AmbiguousHookError`, a `hooks.commands` argv[0] not found on PATH) is appended as an `engine.ValidateFinding` with `Severity: error`, `Field: "hooks.<name>"` where applicable, and the same message the lifecycle command would print. Any error finding makes `valid: false` and exit code 2, matching the existing `fail` path (`cmd/validate.go:45-59`).
- Each hook-like unknown file name (`pre-*`, `post-*`, `on-*` not in `Known()`, for example `pre_deploy` or `post-deploy.sh` on Linux) becomes a `warn` finding. Warnings do not change `valid` or the exit code.
- `Resolve` therefore returns all errors and unknown names instead of stopping at the first, so validate can report them together. The lifecycle commands still fail on the first error.
- With no hooks directory and no `hooks.commands`, validate emits no hook findings.
- **Alternative considered:** a `tako doctor` check. Rejected for now: `validate` already runs offline with the strict preflight path and has a typed `ValidateResult`; `doctor` contacts servers.

## Risks / Trade-offs

- [Strict environment surprises users porting Kamal hooks, for example a hook needing `AWS_PROFILE` or `DOCKER_HOST`] → The docs list the baseline and show `passEnv`. The `hook.failed` message suggests `hooks.passEnv` when the exit is non-zero and stderr mentions an unset variable. This is a best-effort hint, not a contract.
- [Hooks authored on Windows lose the executable bit in git] → Fail closed with an explicit `git update-index --chmod=+x` hint. Document this in CI-CD.md.
- [The `pre-deploy` hook holds deploy leases, so a hung hook blocks the environment] → The timeout is bounded at 1h with a 5m default, and the process tree is killed on timeout.
- [Output redaction cannot catch secrets the hook fetched itself] → Document that redaction covers Tako-known values only. Hook authors must not echo credentials.
- [Post-hook exit code 6 may break CI pipelines that treated any non-zero exit as a deploy failure and auto-rolled back] → The result document `status` stays `success`. The docs explain that 6 means "deployed, needs attention".
- [Shebang interpreter discovery on Windows may find an unexpected `sh`, such as Cygwin or MSYS] → The resolved interpreter path is reported in `hook.started` data.
- [Sibling `add-project-wide-rollback` may restructure `Engine.Rollback`] → The `BeforeMutation` seam is defined on the request type and documented as "after target selection, before first mutation". Whichever change lands second rebases onto it.

## Migration Plan

- Purely additive, so no migration is needed. Existing projects produce byte-identical machine output because the new fields are `omitempty` and no hook events are emitted when no hooks exist.
- Rollout: ship behind no flag. The feature is inert until a `.tako-hooks/` directory or a `hooks:` block exists.
- Rollback of the feature: delete the hooks directory or the config block, or pass `--skip-hooks`.

## Future Work

- `hooks.output: events-only` for CI logs (deferred; additive enum value).
