# Proposal

## Why

A plain `tako deploy` of a web service today causes downtime and, worse, can leave the service down. The implicit strategy is `recreate`. That strategy stops and removes the old containers before it starts new ones. If the new container fails its health check, takod deletes it and does not bring the old revision back. `deploy.rollbackOnFailure` is accepted in tako.yaml, but nothing acts on it. Operators who switch from Kamal expect the default to be health-gated and zero-downtime, with a failed deploy leaving the previous version serving. Tako already ships a working `rolling` engine, so the gap is in the defaults and in recovery, not in missing machinery.

## What Changes

- **BEHAVIOR CHANGE (implicit default):** when `deploy.strategy` is omitted, Tako picks `rolling` for any stateless, traffic-serving service that can legally use it. It still picks `recreate` for persistent services, services with raw host `ports`, services with `volumes`, portless workers, services whose `deploy` settings are only valid under `recreate`, and `kind: job` / `kind: run` (which have no rollout strategy). The selection rules are deterministic and are listed in the spec.
- An explicit `deploy.strategy` (`recreate`, `rolling`, `blue_green`) keeps exactly its current meaning. No tako.yaml that validates today stops validating: Tako picks `rolling` as a default only when the service would pass rolling validation unchanged.
- A defaulted-`rolling` service with no `healthCheck` and no `deploy.readiness` gets an implicit TCP readiness probe on `service.port` before traffic shifts. Explicit `rolling` keeps today's running-only gate.
- `recreate` keeps the previous revision restorable. takod stops the old containers and renames them instead of removing them, starts and health-checks the new ones, and removes the retained containers only after every node for that service succeeds. On failure on any node, Tako restores the retained containers on every node for that service and reports the deploy as failed, with a typed per-service recovery outcome and events.
- `deploy.rollbackOnFailure` becomes a tri-state (`*bool`) and defaults to `true`. `false` disables restore (`recreate`) and keeps failed new containers stopped but not removed for debugging (both strategies). The old revision still serves under `rolling`, because traffic never shifted.
- New takod capability `deploy.restore-on-failure-v1` gates the retain/restore/finalize protocol. Behavior with an older takod: if `rollbackOnFailure` is set explicitly (either `true` on `recreate`, or `false` on any strategy), the deploy fails closed with an upgrade message. If it is only the default, the service falls back to legacy remove-then-start on *all* of its nodes (if any one node lacks the capability), with a warning event naming the nodes and `tako upgrade servers`. This fallback is a deliberate exception to the project's fail-closed rule for missing capabilities: the CLI does not send `FailurePolicy` in that case, so the old node receives exactly the request it receives today and nothing becomes less safe.
- `deploy --plan-only` (human and JSON), the `DeployResult` document, deploy events, notifications, and deployment history record the effective strategy, whether it was defaulted, and any restore outcome. The plan reason "Deploy strategy default changed: recreate → rolling" appears on the first deploy after the upgrade.
- **JSON documents (additive, not breaking):** `PlanChange` gains `strategy`, `strategySource`. `ServiceOutcome` gains `strategy`, `recovery`. A new `ServiceOutcome.action` value `restored` is added. New event types `deploy.service.restore.started`, `deploy.service.restored`, `deploy.service.restore_failed`. Consumers that treat unknown enum values as opaque are unaffected. The deploy plan hash changes for every plan, so a plan produced by an older CLI and applied with this CLI reports plan drift (exit 2). Regenerate the plan.
- **One-time redeploy:** the effective strategy is part of the service config hash and revision ID (`pkg/reconcile/config_hash.go:147`, `pkg/deployplan/revision_identity.go:24`). The first deploy after upgrading therefore reconciles every service whose default flips. That redeploy happens through `rolling`, without downtime. Services that explicitly wrote `rollbackOnFailure: false` also hash differently once.
- Migration: to keep the old behavior, set `deploy.strategy: recreate` explicitly. `tako config` already prints `deploy.strategy: <value> (default)` vs explicit (`cmd/config.go:120`).

## Out of scope

- Rolling with `maxUnavailable > 0` or partial-surge batches (`maxSurge < replicas`). The current full-revision rolling engine is unchanged.
- Changing `blue_green` semantics, manual promotion, or `gracePeriod`.
- Reverting *other* services that already succeeded in the same multi-service deploy. Restore is scoped to the service that failed. Project-wide rollback is the sibling change `add-project-wide-rollback`.
- Rolling or zero-downtime for persistent services, raw-port services, or volume-backed services. Those require explicit opt-in or stay `recreate`.
- Data-level rollback. Restoring a `recreate` service restarts the previous containers against the current volume contents. Migrations already applied by `deploy.release` or by the new container are not undone.
- Automatic rollback after a deploy has succeeded (for example, post-deploy error-rate monitoring).
- Local deploy hooks firing on restore (sibling `add-local-deploy-hooks` may consume the new events).
- Surfacing `recovery` in `tako rollback` and `tako scale` result documents. The sibling `add-project-wide-rollback` carries it into its `services[]`; `scale` can follow later.

## Capabilities

### New Capabilities
- `deploy-strategies`: how Tako selects a service's deploy strategy (explicit vs default), the health/readiness gate that guards traffic cutover, failure handling and previous-revision restore (`rollbackOnFailure`), the takod capability gate for restore, and how strategy and recovery are reported in plans, results, events, notifications, and history.

### Modified Capabilities
<!-- None: openspec/specs/ has no existing capabilities. -->

## Impact

- **Config:** `pkg/config/validator.go` (default selection at ~997, `validateDeployStrategy` 1542, `validateServicePorts` 1629, run-kind check 1805), `pkg/config/types.go` (`DeployConfig.RollbackOnFailure` → `*bool`, new `StrategyDefaulted` non-serialized marker), `pkg/deployplan/revision_identity.go` (`EffectiveDeployStrategy` default).
- **takod:** `pkg/takod/reconcile.go` (`ReconcileService` 148–238, `removeServiceContainersForReconcile` 1066, `cleanupStartedContainers` 1185), new finalize/restore endpoints in `pkg/takod/server.go`, new capability constant.
- **CLI deployer/engine:** `pkg/deployer/takod_deploy.go` (`deployServiceTakod` 386, `deployServiceToTakodNode` ~1490–1549, `buildTakodHealthSpec` 1675), `pkg/engine/deploy.go` (service loop 925–984, failure path 1130–1180), `pkg/engine/types.go` (`PlanChange`, `ServiceOutcome`, outcome constants), `pkg/takoapi/events/events.go`, `pkg/takoapi/state.go` (`ServiceStateDocument`), `pkg/reconcile/reconcile.go` (`detectChanges` reason).
- **Docs and generated artifacts:** `docs/CONFIGURATION.md` (new deploy-strategy section; raw-ports note at 1433), `docs/ORCHESTRATION-MODEL.md` (Deployment Strategies, 347–372), `docs/MACHINE-INTERFACE.md` (new fields/events), `schema/tako.schema.json` (deploy description at 1096, `rollbackOnFailure` at 1114), README feature line.
- **Tests:** config default-selection matrix, takod reconcile restore/finalize tests with the fake docker runner, deployer capability-gate tests, engine result/event tests, schema sync test.
- No new CLI commands or flags. No change to exit codes. A failed-and-restored deploy still exits 1.
