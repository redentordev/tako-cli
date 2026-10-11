# Spec Delta

## Purpose

Lets operators run their own committable scripts or commands on the local machine at defined points of Tako's mutating operations, so they can gate, observe, and react to deploys without breaking the machine interface or exposing secrets.

## ADDED Requirements

### Requirement: Hook set and command coverage
Tako SHALL recognize exactly these hook names: `pre-deploy`, `post-deploy`, `pre-rollback`, `post-rollback`, `pre-promote`, `post-promote`, `pre-setup`, `post-setup`, and `on-failure`. Hooks SHALL run only for `tako deploy`, `tako rollback`, `tako promote`, and `tako setup`, and SHALL always run on the machine executing the CLI, never on a node.

#### Scenario: Only lifecycle commands trigger hooks
- **WHEN** `.tako-hooks/pre-deploy` exists and the operator runs `tako scale web=3`
- **THEN** no hook is executed

#### Scenario: Hooks run locally
- **WHEN** a `pre-deploy` hook runs during `tako deploy`
- **THEN** the hook process is started on the operator's machine with the project directory as its working directory
- **AND** no command is sent to any node to run the hook

### Requirement: Hook discovery location
Tako SHALL look for hook files in the directory named by `hooks.path`, resolved relative to the directory that contains the loaded config file, defaulting to `.tako-hooks`. A `hooks.path` that is absolute, escapes the project directory, or lies inside `.tako/` SHALL be rejected as an invalid config. An absent directory SHALL mean no file hooks.

#### Scenario: Default directory next to the config
- **WHEN** the operator runs `tako deploy --config infra/tako.yaml` and `infra/.tako-hooks/pre-deploy` exists
- **THEN** that file is used as the `pre-deploy` hook

#### Scenario: Hooks path inside the gitignored state directory
- **WHEN** `tako.yaml` sets `hooks.path: .tako/hooks`
- **THEN** config validation fails with exit code 2 and an error explaining that `.tako/` is gitignored and naming `.tako-hooks` as the default

#### Scenario: Hooks path escaping the project
- **WHEN** `tako.yaml` sets `hooks.path: ../shared-hooks`
- **THEN** config validation fails with exit code 2

#### Scenario: No hooks directory
- **WHEN** neither the hooks directory nor `hooks.commands` exists
- **THEN** the operation runs exactly as it would without hook support and emits no hook events

### Requirement: Declared hook commands
Tako SHALL accept `hooks.commands`, a map from hook name to a non-empty argv list, executed without a shell. Unknown hook names SHALL fail validation. A hook defined both as a file and in `hooks.commands` SHALL fail with an invalid-request error before any lock is taken. `${VAR}` references SHALL be rejected in `hooks.commands`.

#### Scenario: Argv command runs without a shell
- **WHEN** `hooks.commands.pre-deploy` is `["make", "check"]`
- **THEN** Tako runs the `make` executable found on PATH with the single argument `check`, without a shell

#### Scenario: Unknown hook name
- **WHEN** `hooks.commands` contains the key `pre-build`
- **THEN** config validation fails with exit code 2 and lists the valid hook names

#### Scenario: Ambiguous definition
- **WHEN** `.tako-hooks/post-deploy` exists and `hooks.commands.post-deploy` is also set
- **THEN** the command fails with exit code 2 before acquiring the local lock or remote leases

#### Scenario: Environment reference in a hook command
- **WHEN** `hooks.commands.post-deploy` is `["notify", "${SLACK_TOKEN}"]`
- **THEN** config validation fails with exit code 2 and directs the operator to `hooks.passEnv`

### Requirement: Cross-platform hook file resolution
On Linux and macOS, a hook file SHALL be the extensionless file named after the hook and SHALL be executable. On Windows, Tako SHALL prefer `<hook>.ps1`, `<hook>.cmd`, `<hook>.bat`, or `<hook>.exe`. If none exists, Tako SHALL run the extensionless file through the interpreter named by its shebang, or `sh`, found on PATH. Unrunnable or ambiguous hooks SHALL fail with exit code 2.

#### Scenario: Non-executable hook on Linux
- **WHEN** `.tako-hooks/pre-deploy` exists without the executable bit on Linux
- **THEN** the command fails with exit code 2 before acquiring locks
- **AND** the error names the file and suggests `chmod +x` or `git update-index --chmod=+x`

#### Scenario: PowerShell hook on Windows
- **WHEN** `.tako-hooks/pre-deploy.ps1` exists on Windows
- **THEN** Tako runs it with `pwsh`, or with `powershell.exe` when `pwsh` is absent, non-interactively and without loading a profile

#### Scenario: Shell script on Windows
- **WHEN** only `.tako-hooks/post-deploy` with a `#!/usr/bin/env bash` shebang exists on Windows and `bash` is on PATH
- **THEN** Tako runs it with that `bash`, never with the WSL launcher in the Windows system directory

#### Scenario: Windows-only variant on Linux
- **WHEN** only `.tako-hooks/pre-deploy.ps1` exists and the CLI runs on Linux
- **THEN** the command fails with exit code 2 stating that no runnable `pre-deploy` variant exists for this platform

#### Scenario: Two native variants on Windows
- **WHEN** both `pre-deploy.ps1` and `pre-deploy.cmd` exist on Windows
- **THEN** the command fails with exit code 2 naming both files

#### Scenario: Sample and unrelated files
- **WHEN** the hooks directory contains `pre-deploy.sample` and `README.md`
- **THEN** neither file is executed and no error is raised

### Requirement: Deploy hook placement
`pre-deploy` SHALL run after the plan is computed, `--plan` is verified, and approval is given, while the lock and leases are held, and before workspace attachment or any mutation. `post-deploy` SHALL run after a successful deploy once the lock and leases are released. `--plan-only`, plan-drift refusals, and confirmation refusals SHALL run no hooks.

#### Scenario: Pre-deploy sees the approved plan
- **WHEN** an operator runs `tako deploy --yes` with a `pre-deploy` hook
- **THEN** the hook runs once after the plan is computed and before any image build or container change
- **AND** its environment contains the plan hash that the `DeployResult` later reports

#### Scenario: Plan-only never runs hooks
- **WHEN** an operator runs `tako deploy --plan-only` with `pre-deploy` and `post-deploy` hooks
- **THEN** no hook is executed

#### Scenario: Confirmation required in machine mode
- **WHEN** a destructive plan is computed under `--output json` without `--yes`
- **THEN** Tako emits the `ConfirmationRequired` document, exits 2, and runs no hook

#### Scenario: Post-deploy runs after release of leases
- **WHEN** a deploy succeeds and `post-deploy` runs for several minutes
- **THEN** another operator can acquire the environment's remote leases while the hook is still running

#### Scenario: Up-to-date deploy
- **WHEN** the plan is empty and the deploy reports all services up to date
- **THEN** `pre-deploy` and `post-deploy` both run, and `TAKO_PLAN_EMPTY` is `true`

### Requirement: Rollback, promote, and setup hook placement
`pre-rollback` and `pre-promote` SHALL run after the lock and leases are held and the target is resolved, before any mutation. `pre-setup` SHALL run before the first node is set up. The matching `post-*` hook SHALL run after the operation succeeds and releases its lock and leases.

#### Scenario: Pre-rollback knows the target
- **WHEN** an operator runs `tako rollback --service web` with a `pre-rollback` hook
- **THEN** the hook receives the selected target deployment ID in `TAKO_ROLLBACK_TARGET` before any container is changed

#### Scenario: Pre-promote knows the revision
- **WHEN** an operator runs `tako promote web` with a `pre-promote` hook
- **THEN** the hook receives the selected warmed revision in `TAKO_REVISION` before proxy routes change

#### Scenario: Setup hooks
- **WHEN** an operator runs `tako setup` for two nodes and both succeed
- **THEN** `pre-setup` runs once before the first node and `post-setup` runs once after the last node

### Requirement: Pre-hook failure aborts without mutation
A `pre-*` hook that exits non-zero, cannot be started, or times out SHALL abort the operation before any mutation. The process SHALL exit with code 1, emit `hook.failed`, emit the operation's result document with a failed status and the hook error, and record no deployment history entry.

#### Scenario: Failing pre-deploy
- **WHEN** `pre-deploy` exits with status 3
- **THEN** no image is built, no container is changed, and no deployment history entry is written
- **AND** `tako deploy --output json` prints a `DeployResult` with `status: failed`, an `error` naming the `pre-deploy` hook and exit status 3, and exits with code 1

#### Scenario: Failing pre-deploy releases authority
- **WHEN** `pre-deploy` fails
- **THEN** the local state lock and remote leases taken during planning are released before the process exits

### Requirement: Post-hook failure signals attention
If a `post-*` hook fails after a successful operation, Tako SHALL leave the operation's recorded status and result status unchanged, mark the hook failed in the result, emit `hook.failed`, and exit with code 6. If the operation already exits 6, the code SHALL stay 6.

#### Scenario: Failing post-deploy
- **WHEN** a deploy succeeds and `post-deploy` exits with status 1
- **THEN** the `DeployResult` keeps `status: success`, its `hooks` entry for `post-deploy` reports `failed` with exit code 1, and the process exits with code 6

#### Scenario: Non-critical post hook opts out of attention
- **WHEN** a deploy succeeds and the `post-deploy` script runs a failing notification command as `notify-chat || true` and then exits 0
- **THEN** the `post-deploy` hook is reported as `completed` and the process exits with code 0

### Requirement: Failure hook
When a hooked operation fails after its `pre-*` hook stage has been reached, including a failing `pre-*` hook or cancellation, Tako SHALL run `on-failure` once. A failure of `on-failure` itself SHALL produce a warning and SHALL NOT change the exit code. `on-failure` SHALL NOT run for failures that occur before that stage.

#### Scenario: Deploy failure triggers on-failure
- **WHEN** a deploy fails while reconciling a service
- **THEN** `on-failure` runs with `TAKO_COMMAND=deploy`, `TAKO_RESULT=failed`, and a redacted `TAKO_ERROR`
- **AND** the process exits with code 1 regardless of the hook's exit status

#### Scenario: Lock contention does not trigger on-failure
- **WHEN** `tako deploy` exits 3 because another operation holds the local lock
- **THEN** no hook runs

#### Scenario: Pre-hook failure triggers on-failure
- **WHEN** `pre-promote` exits non-zero
- **THEN** `on-failure` runs with `TAKO_HOOK=on-failure` and `TAKO_FAILED_HOOK=pre-promote`

### Requirement: Hook environment contract
A hook SHALL get only a platform baseline (PATH, HOME/USERPROFILE, temp dirs, locale, terminal, Windows system vars, `CI`), variables matched by `hooks.passEnv` (exact names or `PREFIX*`), and the `TAKO_*` context variables. Tako SHALL NOT pass other process variables, including `.env` keys loaded with the config. Inherited `TAKO_*` variables SHALL NOT pass through.

#### Scenario: Dotenv values are not inherited
- **WHEN** the project `.env` defines `DATABASE_PASSWORD` and a `post-deploy` hook prints its environment
- **THEN** `DATABASE_PASSWORD` is absent from the hook environment

#### Scenario: Explicit passthrough
- **WHEN** `hooks.passEnv` is `["GITHUB_TOKEN", "SLACK_*"]` and those variables are set in the CLI's environment
- **THEN** the hook receives `GITHUB_TOKEN` and every variable whose name starts with `SLACK_`

#### Scenario: Tako-managed secrets are never injected
- **WHEN** `.tako/secrets` and `secretSources` resolve secret values for the environment
- **THEN** no hook environment variable contains any of those values unless the operator passed the same process variable through `hooks.passEnv`

### Requirement: Hook context variables
Every hook SHALL receive `TAKO_HOOK`, `TAKO_COMMAND` (canonical name), `TAKO_PROJECT`, `TAKO_ENVIRONMENT`, `TAKO_SERVICES`, `TAKO_SERVERS`, `TAKO_HOSTS`, `TAKO_PERFORMER`, `TAKO_RECORDED_AT` (RFC 3339 UTC), and `TAKO_VERSION`. When known, Tako SHALL add `TAKO_REVISION`, `TAKO_GIT_*`, `TAKO_PLAN_*`, `TAKO_DEPLOYMENT_ID`, `TAKO_ROLLBACK_TARGET`, `TAKO_RESULT`, `TAKO_DURATION_SECONDS`, `TAKO_ERROR_CLASS`, `TAKO_ERROR`, and `TAKO_FAILED_HOOK`.

#### Scenario: Post-deploy context
- **WHEN** `post-deploy` runs after deploying services `api` and `web` from git commit `abc1234`
- **THEN** `TAKO_SERVICES` is `api,web`, `TAKO_GIT_COMMIT` is the full commit hash, `TAKO_RESULT` is `success` or `warmed`, and `TAKO_DEPLOYMENT_ID` matches the recorded deployment

#### Scenario: Error text is redacted
- **WHEN** a deploy fails with an error message that contains a registered secret value
- **THEN** `TAKO_ERROR` in the `on-failure` environment contains the redacted form, not the value

#### Scenario: Command alias
- **WHEN** a hooked command is invoked through an alias
- **THEN** `TAKO_COMMAND` holds the canonical command name

### Requirement: Hook output routing
Tako SHALL capture hook stdout and stderr line by line and emit each line, redacted, as a `hook.output` event. It SHALL never write hook output directly to process stdout in machine modes. `hooks.output: quiet` SHALL emit output at debug level. Hooks SHALL get no stdin.

#### Scenario: JSON output stays parseable
- **WHEN** `tako deploy --output json` runs a `pre-deploy` hook that prints `hello` to stdout
- **THEN** process stdout contains only the `DeployResult` JSON document and `hello` appears on stderr

#### Scenario: NDJSON stream
- **WHEN** `tako deploy --events ndjson` runs a hook that prints `hello`
- **THEN** stdout contains a `hook.output` event with `data.hook` set to the hook name, `data.stream` set to `stdout`, and `data.data` set to `hello`, before the terminal `result` event

#### Scenario: Quiet output
- **WHEN** `hooks.output` is `quiet` and the deploy runs without `--verbose` in text mode
- **THEN** hook output lines are not printed to the terminal but still appear as debug-level `hook.output` events in NDJSON mode

#### Scenario: Hook waiting on input
- **WHEN** a hook reads from stdin
- **THEN** it receives end-of-file immediately

### Requirement: Hook lifecycle events
Tako SHALL emit `hook.started`, then `hook.completed` or `hook.failed` for every executed hook, in phase `hooks`. Each SHALL carry `data.hook`. Terminal events SHALL carry `exitCode` and `durationMs`, and `hook.failed` SHALL add `error` and `timedOut`. A hook suppressed by `--skip-hooks` SHALL emit `hook.skipped` with `data.reason`.

#### Scenario: Successful hook events
- **WHEN** `pre-deploy` exits 0 under `--events ndjson`
- **THEN** the stream contains `hook.started` followed by `hook.completed` with `data.hook: "pre-deploy"`, `data.exitCode: 0`, and a non-negative `data.durationMs`

#### Scenario: Timed-out hook event
- **WHEN** a hook exceeds its timeout
- **THEN** the stream contains `hook.failed` with `data.timedOut: true`

### Requirement: Hook outcomes in result documents
`DeployResult`, `RollbackResult`, `PromoteResult`, and `SetupResult` SHALL include an optional `hooks` array with one entry per hook considered, holding `name`, `status` (`completed`, `failed`, `timed_out`, `skipped`), and, when the hook ran, `exitCode` and `durationMs`. `DeployResult` SHALL include `deploymentId` once history is recorded. Both fields SHALL be omitted when empty.

#### Scenario: Result without hooks
- **WHEN** a project has no hooks and runs `tako deploy --output json`
- **THEN** the `DeployResult` has no `hooks` field and is otherwise unchanged

#### Scenario: Result with hooks
- **WHEN** `pre-deploy` and `post-deploy` both complete
- **THEN** the `DeployResult.hooks` array lists `pre-deploy` then `post-deploy`, each with status `completed` and exit code 0

### Requirement: Hook timeout and cancellation
Each hook SHALL be bounded by `hooks.timeout`, a Go duration from 1s to 1h, defaulting to 5m. On timeout or CLI cancellation, Tako SHALL terminate the hook's whole process tree. A timeout SHALL count as a hook failure. Cancellation SHALL exit with code 5.

#### Scenario: Timeout kills child processes
- **WHEN** a `pre-deploy` hook starts a background child and sleeps past `hooks.timeout: 2s`
- **THEN** the hook and its child are terminated, `pre-deploy` is reported as `timed_out`, and the deploy aborts with exit code 1

#### Scenario: Ctrl-C during a hook
- **WHEN** the operator interrupts the CLI while `pre-deploy` runs
- **THEN** the hook process tree is terminated, no mutation occurs, and the process exits with code 5

#### Scenario: Invalid timeout
- **WHEN** `hooks.timeout` is `2h`
- **THEN** config validation fails with exit code 2

### Requirement: Skipping hooks
`tako deploy`, `tako rollback`, `tako promote`, and `tako setup` SHALL accept `--skip-hooks`, which suppresses every hook, including `on-failure`, for that invocation. Hook resolution errors such as non-executable or ambiguous files SHALL NOT fail an invocation that passes `--skip-hooks`.

#### Scenario: Emergency deploy without hooks
- **WHEN** an operator runs `tako deploy --skip-hooks --yes` with a broken `pre-deploy` hook
- **THEN** the deploy proceeds, no hook process starts, and a `hook.skipped` event is emitted for each defined hook

### Requirement: Validate checks hooks without running them
`tako validate` SHALL resolve hooks with the same rules the lifecycle commands use and SHALL NOT execute any hook. Each hook resolution error SHALL be reported as an `error` finding in `ValidateResult`, making the config invalid with exit code 2. Each file in the hooks directory whose name looks like a hook (`pre-*`, `post-*`, `on-*`) but is not a known hook name or runnable variant SHALL be reported as a `warn` finding. A project with no hooks SHALL get no hook findings.

#### Scenario: Non-executable hook is a validate error
- **WHEN** `.tako-hooks/pre-deploy` exists without the executable bit on Linux and the operator runs `tako validate --output json`
- **THEN** the `ValidateResult` has `valid: false` and an `error` finding naming the file and suggesting `chmod +x`
- **AND** the process exits with code 2
- **AND** no hook process is started

#### Scenario: Ambiguous hook is a validate error
- **WHEN** `.tako-hooks/post-deploy` exists and `hooks.commands.post-deploy` is also set
- **THEN** `tako validate` reports an `error` finding for `post-deploy` and exits with code 2

#### Scenario: Misspelled hook name is a validate warning
- **WHEN** the hooks directory contains `pre_deploy` and `post-deploi`
- **THEN** `tako validate` reports a `warn` finding for each file, suggesting the known hook names
- **AND** the result stays `valid: true` and the process exits with code 0

#### Scenario: Validate never runs hooks
- **WHEN** a valid `pre-deploy` hook exists and the operator runs `tako validate --events ndjson`
- **THEN** no hook process starts and no `hook.*` event is emitted
