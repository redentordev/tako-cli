# Proposal

## Why

`tako rollback` can only restore one service at a time: `--service` is a required flag (`cmd/rollback.go:39-40`) and the engine rejects an empty service (`pkg/engine/rollback.go:85-87`). After a bad release that touched several services, an operator has to work out which services changed and then run one rollback per service. Each run takes its own lease and writes its own history record, and each can fail on its own. `tako history` already tells users to run `tako rollback <deployment-id>` (`cmd/history.go:153-157`), but that command is rejected today because `--service` is missing. Kamal's `kamal rollback VERSION` restores the whole app in one step. Tako needs the same operation, with Tako's safety properties: one lease, preflight before mutation, typed machine output.

## What Changes

- `tako rollback` without `--service` performs a **project-wide rollback**. It restores every eligible long-running service in the environment to the state it had at a target deployment, as one coordinated operation under one local state lock and one remote lease set.
- With no positional argument, the target is the previous stable deployment record. A positional argument selects an explicit target. It may be an exact deployment ID, a unique deployment-ID prefix (the 10-character `displayId` that `tako history` prints), or a git commit (full or ≥7-character prefix). The existing positional argument stays the only way to name a target; no `--to` flag is added.
- Each service's target state is rebuilt from history. A deployment record contains only the services that operation changed (`pkg/engine/deploy.go:713`, `:972-983`), so for each service the newest stable record at or before the target is used. Services already at that state are reported as `unchanged` and are not touched.
- Eligibility rules:
  - `kind: run` steps and `kind: job` services are never rolled back.
  - `persistent: true` services are skipped unless `--include-persistent` is passed. When they are included, a data-compatibility warning is emitted.
  - Services added after the target are left running and reported `skipped`.
- `deploy.release` commands are **not** re-run by default. `--run-release` opts in. `kind: run` steps are never re-run.
- **Image preflight:** every target image is resolved before any mutation. Each image is either retained on a node (exact digest), rebuildable from a locally available target commit, or pulled from a registry. If any build-backed or shared-build image cannot be produced, the command fails before anything changes.
- Planning and confirmation:
  - `--plan-only` emits a `RollbackPlan` document (with a `hash`), or a table in human mode.
  - `--plan <file>` applies only if the freshly computed plan's hash matches the reviewed plan file; drift exits 2 with nothing mutated. This generalizes deploy's existing plan verification.
  - The plan's `warnings[]` states that `kind: job` services are not rolled back and keep the newer image.
  - Human mode asks for confirmation unless `-y/--yes` is given.
  - Machine mode without `--yes` emits `ConfirmationRequired` (`operation: "rollback"`) and exits 2.
  - A human-mode run in a non-interactive session (`TAKO_NONINTERACTIVE`, `CI`, no TTY) without `--yes` also exits 2, as `tako remove` does (unlike `tako deploy`, which exits 1 there).
- **Execution and failure:** services roll back in dependency order and stop at the first failure. Proxy state, takod state, and history are still reconciled for services that already changed. The result reports each service's outcome, and a partial rollback exits 1.
- One history record (`rolled_back`, or `failed` on partial failure), one local state entry, one `rollback_started` notification, and one `rollback_done` or `deploy_failed` notification are produced per operation.
- `RollbackResult` gains additive fields: `scope`, `services[]` (each optionally carrying the per-service `recovery` object from `make-rolling-default-deploys`), `error`. `service` becomes `omitempty`. The single-service form keeps every existing field and value. A new additive NDJSON event type is added: `deploy.service.skipped`.
- Build-backed services reuse a retained exact image before falling back to a git-worktree rebuild. This applies to both forms. Today every build-backed rollback rebuilds (`pkg/deployer/takod_deploy.go:370-384`).
- No **BREAKING** changes:
  - Every currently valid invocation (`--service` given) behaves as before, apart from the image-reuse optimization.
  - `tako rollback` without `--service` used to fail on the required-flag check. It now performs a project-wide rollback, and it is guarded by confirmation.
  - No `tako.yaml` changes.

## Out of Scope

- Rolling back data, volumes, or database schemas. Tako cannot know whether migrations are reversible.
- Re-running `kind: run` steps, and rolling back `kind: job` images or schedules (see design Future Work).
- Pre-pulling registry images before the first mutation (future work; if added, it becomes default behavior).
- Making single-service rollback skip `deploy.release` by default. That is a separate future change at the next breaking window, after one release of deprecation warnings.
- Recreating services that were removed from `tako.yaml` since the target, or removing services added since it.
- Changing the default single-service behavior. `tako rollback --service X` still runs `deploy.release`, has no prompt, and is a single call.
- Warm or instant revision switching (promote-style) for blue-green services. The rollback uses a normal rollout of the target image.
- A `--continue-on-error` mode, and pre/post-rollback local hooks (the sibling change `add-local-deploy-hooks` may add rollback hooks later).
- Command aliases (sibling change `add-command-aliases`).

## Capabilities

### New Capabilities
- `rollback`: selecting a rollback target from replicated deployment history and restoring one service or the whole environment to it. Covers target resolution, eligibility, image availability preflight, confirmation and plan-only flows, execution order, partial-failure semantics, history/notification bookkeeping, and the `RollbackResult` machine contract.

### Modified Capabilities
- None (no main specs exist yet).

## Impact

- **Code:**
  - `cmd/rollback.go`: flags, confirmation, plan-only.
  - `pkg/engine/rollback.go`: new project-wide path, target and snapshot resolution, result shape.
  - `pkg/deployer/deployer.go` (`RollbackToState`) and `pkg/deployer/takod_deploy.go`: skip-release option and retained-image reuse.
  - `pkg/takoapi/events/events.go`: new event type.
  - `cmd/engine.go`: confirmation document, and `verifyPlanFileMatches` generalized over plan types (deploy keeps its behavior).
- **takod:** no new endpoints. The preflight uses the existing `/v1/images/inspect`, which is gated on the existing `image.descriptor-v1` capability.
- **Docs:** `docs/MACHINE-INTERFACE.md`, `docs/CONFIGURATION.md` (rollback notes), `README.md` command table, `examples/01-simple-web/README.md`.
- **Generated:** `man/tako-rollback.1` (`make man`), and completions if they embed flags.
- **Tests:** `cmd/rollback_test.go`, new `pkg/engine/rollback_project_test.go`, `cmd/machine_coverage_test.go` (already lists `tako rollback`), `cmd/machine_output_test.go`.
