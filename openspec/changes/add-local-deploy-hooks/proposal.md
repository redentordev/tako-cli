# Proposal

## Why

Teams that deploy with Tako have no supported way to run their own local checks or side effects around a deploy: verifying the commit is pushed, running a smoke test, posting to a chat channel, tagging a release, or warming a CDN. Today they wrap `tako deploy` in shell scripts. Those wrappers cannot see the computed plan, the revision, or the outcome. They also cannot block a deploy after Tako takes its lock and leases, and they break the `--output json` / `--events ndjson` contract when they print to stdout. Kamal's `.kamal/hooks` shows that local, committable lifecycle hooks are a small feature that delivers a lot. Tako now has a stable engine event stream and typed exit codes, so hooks can be added without weakening the machine interface.

## What Changes

- New **local lifecycle hooks**. These are executables or declared argv commands that run on the operator's machine (never on nodes) at fixed points: `pre-deploy`, `post-deploy`, `pre-rollback`, `post-rollback`, `pre-promote`, `post-promote`, `pre-setup`, `post-setup`, and `on-failure`.
- New hook directory, `.tako-hooks/` by default, resolved next to `tako.yaml`. It is deliberately outside `.tako/`, because `tako init` writes `.tako/` into the project `.gitignore` and hooks must be committable. Hook files are named after the hook. On Windows, `.ps1`, `.cmd`, `.bat`, and `.exe` variants are resolved, and extensionless scripts run through a discovered `sh`/`bash`.
- New optional top-level `hooks:` block in `tako.yaml`. It contains `path`, `timeout`, `output` (`verbose` | `quiet`), `passEnv`, and `commands` (argv per hook, run without a shell, for portability). The block is additive. Every config that validates today keeps validating.
- Hook failure semantics:
  - A non-zero `pre-*` exit aborts the operation before any mutation, with exit code 1.
  - A `post-*` failure turns a successful operation into exit code 6 (attention). The operation's own status stays unchanged. Non-critical hooks can opt out with `|| true` (shell) or an equivalent in the script, which the docs show.
  - An `on-failure` hook failure is reported as a warning and never changes the exit code.
- Hooks receive a curated, **secret-free** environment. This includes `TAKO_*` context variables (project, environment, services, servers/hosts, revision, git commit, performer, command, plan hash, deployment outcome) plus an explicit `passEnv` allowlist. Hooks do not inherit the process environment by default. `tako.yaml` loading exports every `.env` key into the process environment, so inheritance would leak secrets.
- Hook output never reaches process stdout in machine modes. Every line becomes a redacted `hook.output` event. In machine modes it renders to stderr. In NDJSON it is streamed as events.
- New additive events: `hook.started`, `hook.output`, `hook.completed`, `hook.failed`, `hook.skipped`. There is a new `hooks` phase.
- Additive `hooks` array (per-hook outcome) on `DeployResult`, `RollbackResult`, `PromoteResult`, and `SetupResult`. `DeployResult` also gains an additive `deploymentId`.
- New `--skip-hooks` flag on `tako deploy`, `tako rollback`, `tako promote`, and `tako setup`.
- `tako validate` checks hooks without executing them. It runs the same hook resolution as the lifecycle commands. Resolution errors become `error` findings, and misspelled hook-like file names become `warn` findings.
- Per-hook timeout (default 5m). Cancellation and timeout kill the hook's whole process tree.
- No breaking changes to `tako.yaml`, CLI flags, exit codes, or JSON result documents. All additions are new optional keys, flags, fields, and event types within `v1alpha1`.

### Out of scope

- Running hooks on remote nodes. Remote one-off commands remain the job of the existing per-service `deploy.release` command.
- Hooks for `scale`, `run`, `remove`, `destroy`, `maintenance`, `backup`, `jobs`, and other commands. The hook-name registry is designed so these can be added later without breaking changes.
- `pre-build`/`pre-connect` equivalents. Tako builds inside `DeploySession.Apply`, so a pre-build point would be indistinguishable from `pre-deploy`. Connection happens during planning.
- Per-environment or per-service hook overrides, which interact with `add-environment-extends`. Hooks can branch on `TAKO_ENVIRONMENT` instead.
- Interactive hooks (stdin is not attached), hook retries, and parallel hooks.
- Injecting `.tako/secrets` or `secretSources` values into hooks. Only the explicit `passEnv` allowlist can forward process variables.
- A third `hooks.output` value (`events-only`) for CI logs. Deferred; it can be added later as a new enum value without breaking anything.

## Capabilities

### New Capabilities
- `deploy-hooks`: Local lifecycle hooks around mutating operations. Covers hook discovery and resolution (directory, config commands, cross-platform execution), the hook set and its placement in each lifecycle, the environment contract (including the secret guarantees), output and event routing under machine modes, failure, timeout, and cancellation semantics, the `--skip-hooks` opt-out, and result-document reporting.

### Modified Capabilities
<!-- None: there are no existing main specs under openspec/specs/. -->

## Impact

- **Code**:
  - New package `pkg/hooks`, covering resolution, environment building, the process runner, and process-tree kill.
  - `pkg/config/types.go`: `Config.Hooks`.
  - `pkg/config/validator.go`, including a raw-content check that forbids `${VAR}` inside `hooks.commands`.
  - `pkg/takoapi/events/events.go`: new types and the new phase.
  - `pkg/engine/types.go`, `rollback.go`, `promote.go`, and `setup.go`: result fields and `BeforeMutation` request seams.
  - `pkg/engine/deploy.go`: surface the deployment ID.
  - `cmd/deploy.go`, `cmd/rollback.go`, `cmd/promote.go`, and `cmd/setup.go`: hook invocation and `--skip-hooks`.
  - `cmd/validate.go`: hook resolution findings in `ValidateResult`.
  - New `cmd/hooks.go`, the CLI wiring.
- **Generated or checked artifacts**: `schema/tako.schema.json` (with schema tests), `man/` (new flag), and `completions/` if regenerated flags appear there.
- **Docs**:
  - New `docs/HOOKS.md`.
  - `docs/CONFIGURATION.md` (`hooks:` block).
  - `docs/MACHINE-INTERFACE.md` (events, result fields, exit-code semantics for hooks).
  - `docs/CI-CD.md` (executable bit, `--skip-hooks`, `passEnv`).
  - README feature list.
- **takod**: no node-side change and no capability gate. Hooks are purely CLI-local.
- **Sibling changes**:
  - `add-project-wide-rollback`: `post-rollback` must report every rolled-back service in `TAKO_SERVICES`.
  - `add-command-aliases`: `TAKO_COMMAND` is always the canonical command name.
  - `add-environment-extends`: `hooks:` stays top-level.
