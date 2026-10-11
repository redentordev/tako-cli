# Lifecycle Hooks

Hooks are scripts or commands from your repository that Tako runs **on the
machine executing the CLI** (your laptop or the CI runner) at fixed points of
`tako deploy`, `tako rollback`, `tako promote`, and `tako setup`. Use them to
gate a deploy (tests, a change freeze, a manual check), to announce it, or to
react to a failure. Hooks never run on a node; for one-off commands inside
your containers, see [The difference from `deploy.release`](#the-difference-from-deployrelease).

No other command runs hooks. `tako validate` checks them without running
them.

## Quick start

```bash
mkdir .tako-hooks
cat > .tako-hooks/pre-deploy <<'EOF'
#!/bin/sh
set -eu
echo "Deploying $TAKO_PROJECT ($TAKO_SERVICES) to $TAKO_ENVIRONMENT at ${TAKO_GIT_COMMIT_SHORT:-unknown}"
make test
EOF
chmod +x .tako-hooks/pre-deploy
git add .tako-hooks/pre-deploy
tako validate        # resolves hooks without running them
tako deploy
```

A non-zero exit from `pre-deploy` stops the deploy before anything changes.

## Hook names and placement

| Hook | Command | Runs |
|------|---------|------|
| `pre-deploy` | `deploy` | After the plan is computed, `--plan` is verified, and the deploy is approved (`--yes` or the prompt), while the local lock and remote leases are held, before workspace attachment, image builds, or any container change. |
| `post-deploy` | `deploy` | After a successful deploy (including `warmed` blue-green deploys and up-to-date deploys), once the lock and leases are released. |
| `pre-rollback` | `rollback` | After the lock and leases are held and the rollback target is selected, before any container changes. |
| `post-rollback` | `rollback` | After a successful rollback, once the lock and leases are released. |
| `pre-promote` | `promote` | After the lock and leases are held and the warmed revision and image are selected, before proxy routes change. |
| `post-promote` | `promote` | After a successful promotion, once the lock and leases are released. |
| `pre-setup` | `setup` | Once, before the first node is set up. |
| `post-setup` | `setup` | Once, after the last node is set up successfully. |
| `on-failure` | all four | Once, when the operation fails after its `pre-*` stage was reached, including a failing `pre-*` hook or Ctrl-C. |

Placement consequences:

- `tako deploy --plan-only`, a `--plan` drift refusal, and a declined or
  missing confirmation (the `ConfirmationRequired` document under
  `--output json`) run **no** hook.
- A deploy whose plan is empty still runs `pre-deploy` and `post-deploy`, with
  `TAKO_PLAN_EMPTY=true`.
- Failures before the `pre-*` stage, such as an invalid config, lock
  contention (exit 3), or an unreachable node during planning, run no hook,
  not even `on-failure`.
- `post-*` hooks run after the remote leases are released, so a slow
  notification never blocks another operator.
- A `pre-*` hook holds the lock and leases while it runs. Keep it short, and
  rely on `hooks.timeout` as a backstop.

## Where hooks live

Hook files live in `.tako-hooks/`, next to the config file that was loaded
(`tako deploy --config infra/tako.yaml` uses `infra/.tako-hooks/`). Change the
directory with `hooks.path`; it must stay inside the project and outside
`.tako/`, which is gitignored. Hooks run with the config file's directory as
their working directory, with no stdin (reads see end-of-file immediately).

```
.tako-hooks/
  pre-deploy            # Linux/macOS (and Windows via sh)
  pre-deploy.ps1        # Windows
  post-deploy
  on-failure
  pre-deploy.sample     # ignored
  README.md             # ignored
```

A missing directory simply means there are no file hooks. Files ending in
`.sample` and files that do not look like hooks are ignored. A file that looks
like a hook (`pre-*`, `post-*`, `on-*`) but is not a known name, such as
`pre_deploy` or `post-deploi`, never runs; Tako prints a warning with the
closest known name, and `tako validate` reports it as a `warn` finding.

### Linux and macOS

The hook is the extensionless file named after the hook, and it must be
executable. A hook without the executable bit fails the command with exit code
2 before any lock is taken:

```
hook pre-deploy: .tako-hooks/pre-deploy is not executable; run `chmod +x .tako-hooks/pre-deploy` (or `git update-index --chmod=+x .tako-hooks/pre-deploy` when the file was committed from Windows)
```

If only Windows variants (`pre-deploy.ps1`, `.cmd`, `.bat`, `.exe`) exist, the
command fails with exit code 2 because no variant is runnable on this
platform.

### Windows

Tako looks for, in this order:

| File | Runs as |
|------|---------|
| `<hook>.ps1` | `pwsh` (PowerShell 7), or `powershell.exe` when `pwsh` is not on PATH, with `-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File` |
| `<hook>.cmd`, `<hook>.bat` | `%ComSpec% /d /c` |
| `<hook>.exe` | directly |
| `<hook>` (extensionless) | the interpreter named by its shebang (`#!/usr/bin/env bash` runs `bash`), otherwise `sh`, then `bash`, found on PATH, for example from Git for Windows |

Two native variants of the same hook (`pre-deploy.ps1` and `pre-deploy.cmd`)
are ambiguous and fail with exit code 2, naming both files. A native variant
wins over the extensionless file, so one repository can ship `pre-deploy` for
Linux CI and `pre-deploy.ps1` for Windows laptops.

For extensionless scripts Tako never uses the WSL launcher `bash.exe` in
`C:\Windows\System32` or `WindowsApps`, which would run the script inside a
Linux distribution with a different filesystem and environment. If no
suitable interpreter is found, the command fails with exit code 2 and names
the interpreter it looked for.

## Declared commands

Instead of a file, a hook can be an argv list in `tako.yaml`. It runs without a
shell: no globbing, pipes, or variable expansion.

<!-- tako:snippet partial -->
```yaml
hooks:
  commands:
    pre-deploy: [make, check]
    post-deploy: [./scripts/notify-deploy, --channel, deploys]
```

`argv[0]` is looked up on PATH; a value containing a path separator is
resolved relative to the project directory. Unknown hook names fail
validation and list the valid names. A hook defined both as a file and under
`hooks.commands` fails with exit code 2 before any lock is taken. `${VAR}`
inside `hooks.commands` is rejected: pass the variable with `hooks.passEnv`
and read it inside the hook instead.

See [CONFIGURATION.md](./CONFIGURATION.md#lifecycle-hooks) for every
`hooks:` key and its default.

## Environment

A hook does **not** inherit the CLI's environment. It gets exactly:

1. A platform baseline, when set: `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`,
   `TERM`, `LANG`, `LC_*`, `TZ`, `TMPDIR`, `CI`, and on Windows `USERPROFILE`,
   `USERNAME`, `TEMP`, `TMP`, `SystemRoot`, `SystemDrive`, `windir`,
   `ComSpec`, `PATHEXT`, `APPDATA`, `LOCALAPPDATA`, `ProgramData`,
   `ProgramFiles`, `ProgramFiles(x86)`, and `PSModulePath`.
2. Variables matched by `hooks.passEnv`: exact names or `PREFIX*` patterns.
   Names are case-insensitive on Windows.
3. The `TAKO_*` context variables below. `TAKO_*` variables already set in
   the CLI's environment are dropped, so a hook can trust them.

### Secret rules

- `.env` keys loaded with the config, `.tako/secrets`, `secretSources`
  values, registry credentials, and server passwords never reach a hook. The
  only way to give a hook a secret is to export it in the CLI's environment
  and list it in `hooks.passEnv`.
- Passed variables whose names look like credentials (containing `TOKEN`,
  `SECRET`, `KEY`, `PASSWORD`, `PASS`, `CREDENTIAL`, or `AUTH`) are
  registered with Tako's redactor, together with every secret Tako already
  knows, so their values are masked in hook output, events, and
  `TAKO_ERROR`.
- If a failing hook's stderr mentions an unset or unbound variable, the
  `hook.failed` event carries a hint pointing at `hooks.passEnv`.

### Context variables

Lists are comma-separated and sorted. Booleans are `true` or `false`.
Variables marked *when known* are omitted when they do not apply.

| Variable | Set | Value |
|----------|-----|-------|
| `TAKO_HOOK` | always | Hook name, e.g. `pre-deploy` or `on-failure`. |
| `TAKO_COMMAND` | always | Canonical command: `deploy`, `rollback`, `promote`, or `setup`, also when invoked through an alias. |
| `TAKO_PROJECT` | always | `project.name`. |
| `TAKO_ENVIRONMENT` | always | Target environment. |
| `TAKO_SERVICES` | always | Services in scope: the plan's services for deploy, the target service for rollback and promote; empty for setup. |
| `TAKO_SERVERS` | always | Server names in scope. |
| `TAKO_HOSTS` | always | Host addresses of those servers. |
| `TAKO_PERFORMER` | always | The operator identity Tako records in deployment history. |
| `TAKO_RECORDED_AT` | always | Time the hook started, RFC 3339 UTC. |
| `TAKO_VERSION` | always | Tako CLI version. |
| `TAKO_REVISION` | when known | Deploy revision; for rollback the target's commit; for promote the selected warmed revision. |
| `TAKO_IMAGE` | when known | Image being rolled back to or promoted. |
| `TAKO_GIT_COMMIT` | when known | Full git commit of the deployed checkout. |
| `TAKO_GIT_COMMIT_SHORT` | when known | Short commit. |
| `TAKO_GIT_BRANCH` | when known | Git branch. |
| `TAKO_GIT_DIRTY` | when known | Whether the working tree had uncommitted changes. |
| `TAKO_PLAN_HASH` | deploy | Hash of the approved plan; matches `planHash` in the `DeployResult`. |
| `TAKO_PLAN_EMPTY` | deploy | `true` when everything is already up to date. |
| `TAKO_PLAN_DESTRUCTIVE` | deploy | `true` when the plan needed destructive confirmation. |
| `TAKO_DEPLOYMENT_ID` | when known | Recorded deployment ID (`post-deploy`, `on-failure` after history was written). Matches `deploymentId` in the `DeployResult`. |
| `TAKO_ROLLBACK_TARGET` | rollback | Deployment ID being rolled back to. |
| `TAKO_RESULT` | `post-*`, `on-failure` | Operation status, e.g. `success`, `warmed`, `rolled_back`, or `failed`. |
| `TAKO_DURATION_SECONDS` | `post-*`, `on-failure` when known | Operation duration in seconds. |
| `TAKO_ERROR_CLASS` | `on-failure` | `failed`, `invalid`, `locked`, `connectivity`, `cancelled`, or `attention`. |
| `TAKO_ERROR` | `on-failure` | Redacted error message, truncated to 4 KiB. |
| `TAKO_FAILED_HOOK` | `on-failure` | Name of the `pre-*` hook that failed, when a hook caused the failure. |

## Output and events

Tako reads hook stdout and stderr line by line (lines are capped at 1 MiB),
redacts each line, and emits it as a `hook.output` event. Hook output never
goes straight to process stdout:

- In text mode, lines are printed to the terminal.
- With `--output json`, they go to stderr and stdout carries only the result
  document.
- With `--events ndjson`, each line is a `hook.output` event on stdout, before
  the terminal `result` event.

`hooks.output: quiet` emits hook output at debug level: it is hidden in text
mode unless you pass `--verbose`, and still present in NDJSON.

Every executed hook emits, in phase `hooks`:

| Event | Data |
|-------|------|
| `hook.started` | `hook`, `source` (`file` or `config`), `path` or `argv`, `timeoutSeconds`, `interpreter` when one is used |
| `hook.output` | `hook`, `stream` (`stdout` or `stderr`), `data` (one line) |
| `hook.completed` | `hook`, `exitCode`, `durationMs` |
| `hook.failed` | `hook`, `exitCode` (`-1` when the hook did not exit normally), `durationMs`, `error`, `timedOut`, optional `hint` |
| `hook.skipped` | `hook`, `reason` (`--skip-hooks`) |

`DeployResult`, `RollbackResult`, `PromoteResult`, and `SetupResult` carry a
`hooks` array, one entry per hook considered, in run order:

```json
"hooks": [
  {"name": "pre-deploy", "status": "completed", "exitCode": 0, "durationMs": 812},
  {"name": "post-deploy", "status": "failed", "exitCode": 1, "durationMs": 95}
]
```

`status` is `completed`, `failed`, `timed_out`, or `skipped`. The field is
omitted when no hook was considered. See
[MACHINE-INTERFACE.md](./MACHINE-INTERFACE.md#lifecycle-hooks).

## Failures and exit codes

| What fails | Effect | Exit code |
|------------|--------|-----------|
| A `pre-*` hook exits non-zero, cannot start, or times out | The operation is aborted before any mutation. No deployment history entry is written, the lock and leases are released, `on-failure` runs, and the result document has `status: failed` and an `error` naming the hook and its exit status. | 1 |
| Ctrl-C (or cancellation) while a hook runs | The hook's process tree is terminated, nothing is mutated, and `on-failure` runs. | 5 |
| A `post-*` hook fails after a successful operation | The operation's recorded status and result `status` stay unchanged (`success`); the hook entry reports `failed`. | 6 |
| The operation itself fails after the `pre-*` stage | `on-failure` runs; the operation's exit code is unchanged. | operation's code |
| `on-failure` fails | A warning; the exit code is unchanged. | unchanged |
| A hook cannot be resolved (not executable, ambiguous, missing interpreter) | The command fails before taking any lock. | 2 |

An operation that already exits 6 (needs attention) stays 6 when a `post-*`
hook also fails.

Exit code 6 tells CI that the deploy happened but something needs a human. If
a post hook only sends a notification that must never fail the pipeline,
swallow the error inside the script so the hook exits 0:

```sh
#!/bin/sh
./scripts/notify-chat "Deployed $TAKO_PROJECT $TAKO_GIT_COMMIT_SHORT to $TAKO_ENVIRONMENT" || true
```

## Timeouts

Each hook is bounded by `hooks.timeout` (default `5m`, allowed `1s` to `1h`).
On timeout or cancellation Tako terminates the hook's whole process tree: on
Linux and macOS the hook runs in its own process group, which receives
SIGTERM and then SIGKILL; on Windows the hook runs in a Job Object that is
closed. Background processes a hook leaves behind are also stopped when the
hook exits. A timed-out hook is reported as `timed_out` and counts as a
failure.

## Checking hooks offline

```bash
tako validate
tako validate --output json
```

`tako validate` resolves hooks with the same rules as the lifecycle commands
and starts no process. Resolution errors (not executable, ambiguous, no
runnable variant, missing interpreter or command) are `error` findings with
field `hooks.<name>` and make the config invalid (exit 2). Misspelled hook
files are `warn` findings and leave `valid: true`. A project without hooks
gets no hook findings. Run it in CI before deploying, and on Windows before
committing a new hook.

## Skipping hooks

```bash
tako deploy --skip-hooks --yes
```

`--skip-hooks` (on `deploy`, `rollback`, `promote`, and `setup`) suppresses
every hook for that invocation, including `on-failure`. Hook resolution
errors do not fail a skipped invocation, so a broken hook cannot block an
emergency deploy. Each defined hook emits `hook.skipped` and appears in the
result with status `skipped`.

## Migrating from Kamal

| Kamal | Tako |
|-------|------|
| `.kamal/hooks/pre-deploy`, `post-deploy` | `.tako-hooks/pre-deploy`, `post-deploy` |
| Other Kamal hooks (`pre-connect`, `pre-build`, boot and proxy hooks) | No equivalent. Put checks in `pre-deploy`; build-time logic belongs in your build. |
| Rollback | `tako rollback` runs its own `pre-rollback` and `post-rollback`, never the deploy hooks. |
| `KAMAL_*` variables | `TAKO_*` variables (see the table above): `KAMAL_PERFORMER` becomes `TAKO_PERFORMER`, `KAMAL_HOSTS` becomes `TAKO_HOSTS`, the deployed version is `TAKO_REVISION` and `TAKO_GIT_COMMIT`, and the run time is `TAKO_DURATION_SECONDS`. |
| Hooks inherit the full shell environment | Hooks get a minimal environment; list what they need in `hooks.passEnv`. |
| Skipping hooks | `--skip-hooks`. |
| A failing post hook fails the command | Exit code 6, with the operation still recorded as successful. |

Kamal hook scripts usually work after renaming the directory and the
variables. Check that they are executable (`git update-index --chmod=+x`) and
that any secret they read is listed in `hooks.passEnv`.

## The difference from `deploy.release`

| | Hooks | `deploy.release` |
|-|-------|------------------|
| Runs on | The machine running `tako` | A one-off container on a node, from the service's new image |
| Scope | Once per operation | Per service, inside the rollout |
| Sees | Your repository, local tools, `passEnv` variables | The service's environment and secrets, its network and volumes |
| Typical use | Tests, change freezes, notifications, tagging releases, opening an incident | Database migrations, cache warm-up |
| Events | `hook.*` | `deploy.release.*` |

Use a hook for anything that needs your checkout or local credentials, and
`deploy.release` (or a `kind: run` step) for anything that must run next to
your data with the service's own secrets.
