# Spec Delta

## Purpose

Lets operators restore an environment, or a single service, to the runtime state recorded at an earlier deployment. The restore is safe, previewable, and machine-readable, and it never mutates anything before every target is known to be restorable.

## ADDED Requirements

### Requirement: Project-wide rollback when no service is named
`tako rollback` invoked without `--service` SHALL perform a project-wide rollback of the selected environment. It SHALL NOT fail with a missing-flag error. `tako rollback --service NAME` SHALL keep its existing single-service behavior.

#### Scenario: No service flag selects project scope
- **WHEN** the operator runs `tako rollback -e production -y`
- **THEN** the command plans and applies a rollback covering every eligible service in `production`
- **AND** the result document reports `scope: "project"`

#### Scenario: Service flag keeps single-service scope
- **WHEN** the operator runs `tako rollback --service web`
- **THEN** only `web` is rolled back, without a confirmation prompt, as before this change
- **AND** the result document reports `scope: "service"` and `service: "web"`

### Requirement: Default project rollback target
With no positional argument, a project-wide rollback SHALL target the newest deployment record with status `success` or `rolled_back` that is strictly older than the newest record in the freshest reachable mesh history, regardless of that newest record's status.

#### Scenario: Previous stable deployment after a good deploy
- **WHEN** history is D1 (success) then D2 (success), and no target is given
- **THEN** the target is D1

#### Scenario: Failed newest deploy
- **WHEN** history is D1 (success), D2 (success), then D3 (failed), and no target is given
- **THEN** the target is D2

#### Scenario: No earlier stable record
- **WHEN** the history holds only one stable record and nothing older
- **THEN** the command fails with an invalid-request error (exit 2) stating that no previous stable deployment exists, and nothing is mutated

### Requirement: Explicit rollback target resolution
A positional target SHALL resolve, in order: exact deployment ID; unique deployment-ID prefix; git commit (full hash or prefix of 7+ characters) matching the newest stable record with that commit. Ambiguous or unknown values and non-stable targets SHALL fail with exit 2 before any mutation.

#### Scenario: Display ID from history is accepted
- **WHEN** `tako history` shows display ID `1767225600` and the operator runs `tako rollback 1767225600 -y`
- **AND** exactly one deployment ID starts with that prefix
- **THEN** that deployment is the target

#### Scenario: Ambiguous prefix
- **WHEN** two deployment IDs share the given prefix
- **THEN** the command exits 2 and lists the matching deployment IDs

#### Scenario: Git commit target
- **WHEN** the operator runs `tako rollback abc1234 -y` and the newest stable record with commit prefix `abc1234` is D5
- **THEN** D5 is the target

#### Scenario: Failed deployment cannot be a target
- **WHEN** the given ID names a deployment with status `failed`
- **THEN** the command exits 2 stating that the deployment is not a stable rollback target

### Requirement: Per-service target state reconstruction
For each service, the target state SHALL be its entry in the newest stable record at or before the target. If no such record exists but newer records do, the service is new (skipped) unless history has been pruned to its retention limit; then planning SHALL fail with exit 2. A service absent from all retained history SHALL be `unchanged`.

#### Scenario: Service unchanged since an older deploy
- **WHEN** D1 deployed `web` and `worker`, D2 deployed only `web`, D3 deployed only `worker`, and the target is D2
- **THEN** `web` targets its D2 state and `worker` targets its D1 state

#### Scenario: Pre-target state pruned from history
- **WHEN** history is at its retention limit and `api` appears only in records newer than the target
- **THEN** the plan fails with exit 2 naming `api` and suggesting per-service rollback with `tako rollback --service`

#### Scenario: Service added after target with full history
- **WHEN** history is below its retention limit and `search` appears only in records newer than the target
- **THEN** `search` is reported `skipped` with reason `not present at target`

### Requirement: Service eligibility for project rollback
Project rollback SHALL roll back only long-running services in the current config. It SHALL skip `kind: run` and `kind: job` services, services not present at the target, and `persistent: true` services unless `--include-persistent` is given. Each skipped service SHALL appear in the plan and result with action `skipped` and a reason.

#### Scenario: Run and job services are skipped
- **WHEN** the environment has `web` (long-running), `migrate` (`kind: run`) and `nightly` (`kind: job`)
- **THEN** only `web` is considered for rollback
- **AND** `migrate` and `nightly` are reported `skipped` with reasons `kind: run` and `kind: job`
- **AND** the plan's `warnings` include an entry stating that job `nightly` is not rolled back and keeps running the newer image

#### Scenario: Persistent service excluded by default
- **WHEN** `postgres` has `persistent: true` and its target image differs from its current image
- **AND** `--include-persistent` is not given
- **THEN** `postgres` is reported `skipped` with reason `persistent` and is not touched

#### Scenario: Persistent service included on request
- **WHEN** the same rollback runs with `--include-persistent`
- **THEN** `postgres` is rolled back
- **AND** a `warning` event and a plan warning state that volume data is not rolled back and may be incompatible with the older image

#### Scenario: Service added after the target
- **WHEN** `search` first appears in a record newer than the target
- **THEN** `search` is reported `skipped` with reason `not present at target` and keeps running unchanged

### Requirement: Unchanged services are not redeployed
A service whose current image, replica count, and operator-file content hash already equal its target state SHALL be reported `unchanged`. It SHALL NOT be redeployed, restarted, or recorded in the rollback history entry.

#### Scenario: Only changed services are touched
- **WHEN** the target differs from the current state only for `web`
- **THEN** only `web` is redeployed, and every other eligible service is reported `unchanged`

#### Scenario: Nothing to roll back
- **WHEN** every eligible service already matches the target
- **THEN** the command exits 0 with status `up_to_date`, makes no runtime mutation, and writes no history record

### Requirement: Image availability preflight
Before any mutation, each service to roll back SHALL get an image source: `retained` (exact image on a same-platform node), `rebuild` (build-backed, target commit in the local repo), or `pull` (registry `image:`). If any service has no source, the command SHALL fail and mutate nothing.

#### Scenario: Missing image blocks the whole rollback
- **WHEN** `web`'s target image is retained on a node but `api`'s build-backed target image was removed by `tako cleanup` and its commit is not in the local repository
- **THEN** the command fails before touching `web` or `api`
- **AND** the result reports status `failed`, every planned service as `not_attempted`, and an error naming `api` and its image

#### Scenario: Retained image preferred over rebuild
- **WHEN** a build-backed service's exact target image is retained on a node with matching content
- **THEN** the rollback transfers or reuses that image and does not rebuild from git

#### Scenario: Older node agent fails closed
- **WHEN** a target node's takod does not advertise image descriptor support
- **THEN** the command exits 2 with a message to upgrade the node agent, and nothing is mutated

### Requirement: Release and run steps during rollback
A project-wide rollback SHALL NOT run any service's `deploy.release` command unless `--run-release` is given, and SHALL never execute `kind: run` steps. Single-service rollback SHALL keep running `deploy.release` as it does today.

#### Scenario: Release skipped by default
- **WHEN** `web` defines `deploy.release` and a project-wide rollback restores `web`
- **THEN** no release container runs and no `deploy.release.*` events are emitted for `web`

#### Scenario: Release opt-in
- **WHEN** the same rollback runs with `--run-release`
- **THEN** `web`'s release command runs once, from the target image, before `web` is activated

#### Scenario: Run fingerprints unaffected
- **WHEN** a project-wide rollback completes and the operator later runs `tako deploy`
- **THEN** `kind: run` completion is decided only from deploy-recorded run entries, because the rollback record contains no `kind: run` entries

### Requirement: Single coordinated operation
A project-wide rollback SHALL hold one local state lock and one remote lease set across all mutation-eligible nodes for its entire duration. It SHALL roll services back sequentially in the same dependency order `tako deploy` uses.

#### Scenario: Concurrent operation blocked
- **WHEN** another deploy or rollback holds the environment's lease
- **THEN** the project rollback exits 3 before planning any mutation

#### Scenario: Dependency order
- **WHEN** `web` depends on `api` and both need rollback
- **THEN** `api` is rolled back before `web`

### Requirement: Partial failure semantics
Project rollback SHALL stop at the first service failure and mark the remaining planned services `not_attempted`. It SHALL then reconcile proxy routes, takod state, and history for services already rolled back, report every service's outcome, and exit 1.

#### Scenario: Second service fails
- **WHEN** `api` rolls back successfully and `web` then fails its health check
- **THEN** `worker` (planned after `web`) is `not_attempted`
- **AND** proxy routes and takod state reflect `api`'s rolled-back revision
- **AND** the result has status `failed`, `api` as `rolled_back`, `web` as `failed` with its error, and the command exits 1

### Requirement: Plan-only and confirmation
`--plan-only` SHALL compute and print the rollback plan without mutating anything. Human mode without `-y/--yes` SHALL prompt after showing the plan. When prompting is not allowed (non-interactive session such as `TAKO_NONINTERACTIVE` or `CI`), human mode without `--yes` SHALL fail with a confirmation-required error and exit 2. Machine mode without `--yes` SHALL emit `ConfirmationRequired` with `operation: "rollback"` and the plan, and exit 2. Single-service rollback SHALL NOT prompt.

#### Scenario: Plan-only in machine mode
- **WHEN** the operator runs `tako rollback --plan-only --output json`
- **THEN** stdout contains one `RollbackPlan` document with the target, per-service actions, image sources, and warnings
- **AND** no lease is held after exit and nothing is mutated

#### Scenario: Machine mode requires approval
- **WHEN** the operator runs `tako rollback --output json` without `--yes`
- **THEN** a `ConfirmationRequired` document with `operation: "rollback"` is emitted and the command exits 2

#### Scenario: Human declines
- **WHEN** a human answers `n` at the prompt
- **THEN** nothing is mutated and the command exits 0 with a cancellation message

#### Scenario: Non-interactive human mode without approval
- **WHEN** the operator runs `tako rollback` in text mode with `CI=true` and without `--yes`
- **THEN** nothing is mutated and the command exits 2 with a confirmation-required error

### Requirement: Reviewed plan verification
`tako rollback --plan <file>` SHALL accept a `RollbackPlan` document previously emitted by `--plan-only`. Before any mutation, and while holding the lock and leases, Tako SHALL compute a fresh plan and compare its `hash` with the reviewed plan's hash. On mismatch, or if the file is unreadable or not a `RollbackPlan`, the command SHALL exit 2 and mutate nothing. A matching plan SHALL still require `--yes` or a confirmation prompt. `--plan` SHALL be rejected together with `--service`.

#### Scenario: Plan-only output carries a hash
- **WHEN** the operator runs `tako rollback --plan-only --output json`
- **THEN** the `RollbackPlan` document contains a non-empty `hash`
- **AND** running `--plan-only` again against unchanged history and runtime state yields the same `hash`

#### Scenario: Reviewed plan applies
- **WHEN** the operator saves `tako rollback --plan-only --output json` to `plan.json` and nothing changes before running `tako rollback --plan plan.json -y`
- **THEN** the rollback applies the reviewed plan

#### Scenario: Plan drift is refused
- **WHEN** a new deployment is recorded after `plan.json` was produced and the operator runs `tako rollback --plan plan.json -y`
- **THEN** the command exits 2 with a plan-drift error telling the operator to re-run `--plan-only`
- **AND** nothing is mutated

#### Scenario: Deploy plan file rejected
- **WHEN** the operator passes a `DeployPlan` document to `tako rollback --plan`
- **THEN** the command exits 2 stating the file is not a rollback plan, and nothing is mutated

#### Scenario: Plan flag with service rejected
- **WHEN** the operator runs `tako rollback --service web --plan plan.json`
- **THEN** the command exits 2 before planning

### Requirement: Rollback result document
`RollbackResult` SHALL keep all existing fields for the single-service form. It SHALL add `scope`, `services` (each with `name`, `action`, `fromImage`, `toImage`, `replicas`, `sourceDeploymentId`, `imageSource`, `reason`, `error`, and, when the service's rollout failed and restore-on-failure handled it, `recovery`) and `error`. In project form, `service` SHALL be omitted.

#### Scenario: Project result shape
- **WHEN** a project rollback to D2 restores `api` and leaves `worker` unchanged
- **THEN** the document has `kind: "RollbackResult"`, `scope: "project"`, `deploymentId: "D2"`, `status: "rolled_back"`, and `services` entries for `api` (`rolled_back`) and `worker` (`unchanged`)

#### Scenario: Single-service result backward compatible
- **WHEN** `tako rollback --service web --output json` succeeds
- **THEN** the document contains `service`, `deploymentId`, `version`, `status`, `startedAt`, and `durationSeconds` with the same meaning as before, plus `scope: "service"`

#### Scenario: Secrets never emitted
- **WHEN** any rollback result, plan, or event is produced
- **THEN** service environment values appear only as redacted keys, never as values

### Requirement: Rollback events
Project rollback SHALL emit `deploy.started`, per-service `deploy.service.started`, `deploy.service.reconciled` or `deploy.service.failed`, a new `deploy.service.skipped` per skipped or unchanged service, and terminal `deploy.succeeded` or `deploy.failed`. Each SHALL carry `data.operation: "rollback"` and `data.targetDeploymentId`.

#### Scenario: NDJSON stream
- **WHEN** the operator runs `tako rollback -y --events ndjson`
- **THEN** every event carries `data.operation: "rollback"`
- **AND** the stream ends with a `result` event whose `data.result` is the `RollbackResult`

### Requirement: Rollback bookkeeping
A project rollback that changes at least one service SHALL write one history record. The record has status `rolled_back`, or `failed` on partial failure, and contains only services actually rolled back. The rollback SHALL also produce one local state entry, one `rollback_started` notification, and one `rollback_done` or `deploy_failed` notification.

#### Scenario: One history entry
- **WHEN** a project rollback restores `api` and `web`
- **THEN** `tako history` shows exactly one new record with status `rolled_back`, containing `api` and `web`, with a message naming the target deployment

#### Scenario: Subsequent default rollback rolls forward
- **WHEN** the newest record is that project rollback and the operator runs `tako rollback -y` again
- **THEN** the target is the stable record immediately preceding the rollback record (the deployment that was rolled away from)
