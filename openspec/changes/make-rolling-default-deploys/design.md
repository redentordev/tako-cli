# Design

## Context

See proposal.md (Why) for motivation and `specs/deploy-strategies/spec.md` for the behavior contract. This section records the code as it is observed today (O) and separates it from what is proposed (P).

**Strategy selection (O)**
- `validateService` fills an empty strategy with `recreate` for every non-run service (`pkg/config/validator.go:995-1003`). It then calls `validateDeployStrategy` (`validator.go:1542-1619`) and `validateServicePorts` (`validator.go:1629-1656`).
- The default is filled *after* replicas default to 1 (`validator.go:902-903`) and after health-check defaults are applied (`validator.go:980-993`).
- `deployplan.EffectiveDeployStrategy` duplicates the `recreate` default for unvalidated configs (`pkg/deployplan/revision_identity.go:29-35`).
- takod duplicates it a third time for empty request strategies (`pkg/takod/reconcile.go:665-676`).
- `rolling` is rejected when any of these hold: persistent, `maxUnavailable > 0`, `0 < maxSurge < replicas`, `gracePeriod` set, `promotion` set, `smokeTest` set (`validator.go:1553-1595`).
- Raw `ports` require `recreate` and at most one replica (`validator.go:1636-1641`).
- The `recreate` branch silently ignores `promotion`, `smokeTest`, `readiness` and `release` validation, apart from `gracePeriod` (`validator.go:1544-1551`). A naive flip of the default would therefore make some valid configs invalid.

**Strategy is part of identity (O)**
- The config hash includes the whole `DeployConfig` (`pkg/reconcile/config_hash.go:63,147`).
- The revision ID hashes `EffectiveDeployStrategy` (`pkg/deployplan/revision_identity.go:24`).
- `detectChanges` reports a hash mismatch only as "Service configuration changed" (`pkg/reconcile/reconcile.go:304-311`).
- `ActualService.DeployStrategy` is read from the `tako.deployStrategy` container label (`reconcile.go:229`, `config_hash.go:17`).

**Container naming (O)**
- `recreate` uses fixed slot names and aliases. `rolling` and `blue_green` use revision-scoped names (`pkg/deployer/takod_deploy.go:2491-2511`).
- Every strategy stamps `tako.revision` and `tako.deployStrategy` labels (`takod_deploy.go:1606-1615`).
- A recreate→rolling transition is therefore already handled. The new revision-scoped containers start next to the slot-named ones. The prune step then removes them by `KeepRevision` (`pkg/takod/reconcile.go:1082-1087`, `pkg/deployer/takod_deploy.go:1199-1250`).

**Reconcile on a node (O)**, in `ReconcileService` (`pkg/takod/reconcile.go:148-238`):
1. It removes old containers *before* pulling or starting anything (`reconcile.go:192`). For `recreate` this means every container with the project, environment and service labels (`reconcile.go:1066-1071`, `1082-1103`, `docker ps -a`). For `rolling` it means only the containers of the same revision.
2. It starts each container, connects its networks, and waits for health (`reconcile.go:209-230`).
3. On any failure it removes the containers it started (`cleanupStartedContainers`, `reconcile.go:1185-1195`) and returns the error. Nothing restores the old containers.

**Health gate (O)**
- `waitForContainerHealthy` returns as soon as the container is running if the `HealthSpec` has no target (`reconcile.go:1360-1404`).
- `buildTakodHealthSpec` returns nil when neither `healthCheck` nor `readiness` is set (`pkg/deployer/takod_deploy.go:1675-1685`).

**CLI rollout (O)**
- `deployServiceTakod` runs the phases preflight → build → release → reconcile, with reconcile running in parallel across nodes (`takod_deploy.go:449-509`, `runTakodNodeActions` at 1257).
- The engine runs the services one at a time and stops at the first failure (`pkg/engine/deploy.go:925-984`).
- Only after all services succeed does it reconcile proxy routes (`deploy.go:1014-1028`), apply removals (1032-1040) and prune stale revisions (1042-1052).
- A rolling failure therefore leaves the old revision routed. The failed containers were already removed on the node.

**rollbackOnFailure (O)**
- It is declared as `bool` with `omitempty` (`pkg/config/types.go:476`) and is only read by the `kind: run` rejection (`validator.go:1805`).
- `schema/tako.schema.json:1114` lists it with no description.

**Actual state (O)** uses `docker ps` without `-a` (`pkg/takod/actual.go:70`). Stopped containers are therefore invisible to plans and status.

**takod request decoding (O)** has no `DisallowUnknownFields`. An older takod silently ignores new request fields, so any new field must be gated by a capability (`pkg/takod/server.go:777-811`).

**Reporting (O)**
- Outcome actions are listed at `pkg/engine/types.go:190-197`.
- `PlanChange` is at `types.go:65-73` and `ServiceOutcome` at `types.go:141-149`.
- Event types are at `pkg/takoapi/events/events.go:57-65`.
- The history service document is at `pkg/takoapi/state.go:258-274`.
- The failure notification is sent at `pkg/engine/deploy.go:1156-1172`.
- `tako config` already prints the strategy source (`cmd/config.go:120`).

## Goals / Non-Goals

**Goals:**
- One function owns default selection, and validation never gets stricter for a config that omits `strategy`.
- A node-local, atomic retain → start → (finalize | restore) protocol for `recreate`, coordinated across nodes by the CLI.
- No new takod requirement for the rolling default itself, so CLI upgrades do not force node upgrades.

**Non-Goals:**
- Rewriting the rolling engine into per-replica batches.
- Persisting retained containers across deploys as a "previous revision cache". Retention lasts only for the duration of one service rollout.
- Proxy-level draining changes. The existing route-ack-then-graceful-stop ordering (`deploy.go:1030-1031`, `removeContainerIDs` at `reconcile.go:1135-1150`) is reused.

## Decisions

### D1. Default selection is "rolling if it would validate and is safe, else recreate"
**(P)** Add `config.DefaultDeployStrategy(service *ServiceConfig) string` and call it at `validator.go:997` in place of the hard-coded `recreate`. It returns `rolling` only when *all* of the following hold:
- the service is a long-running service: `!IsRun() && !IsJob()`
- `!Persistent`
- `Port > 0`
- `len(Ports) == 0`
- `len(Volumes) == 0`
- `Deploy.MaxUnavailable == 0`
- `Deploy.MaxSurge == 0 || Deploy.MaxSurge >= Replicas`
- `Deploy.GracePeriod == "" && Deploy.Promotion == "" && Deploy.SmokeTest == (DeploySmokeTestConfig{})`
- a trial `validateDeployStrategy` on a copy with `Strategy = rolling` returns nil. This also covers `readiness` and `release` errors that `recreate` never checked.

Otherwise the function returns `recreate`. The function sets `Deploy.StrategyDefaulted = true` (new field, `yaml:"-" json:"-"`, so it is excluded from the config hash and schema). `EffectiveDeployStrategy` (`revision_identity.go:30`) calls the same function for unvalidated configs. That keeps the revision ID consistent between validated and raw configs. takod's empty-strategy fallback stays `recreate`, because the CLI always sends an explicit strategy.

- *Why exclude `volumes`:* `rolling` runs two revisions against the same volume at the same time. The validator's own persistent-service message already treats volumes as the reason to keep `recreate` (`validator.go:1554`). SQLite-on-a-volume is the common failure mode. Users can opt in explicitly.
- *Why exclude portless services:* overlap brings no availability benefit when no traffic is served, and it risks duplicate singleton consumers (bot pollers, in-process schedulers). They keep `recreate`, but now with restore.
- *Confirmed (resolved):* volume-backed and portless services stay on `recreate` by default. Both can opt in with an explicit `deploy.strategy: rolling`.
- *Alternative rejected:* default to rolling only when a `healthCheck` is configured. Most simple apps omit `healthCheck`, so the change would rarely apply. D3 covers that case instead.
- *Alternative rejected:* make `blue_green` the default. It requires a proxy route and adds promotion semantics. `rolling` is the Kamal-equivalent behavior.

### D2. Explicit strategy is untouched
**(P)** Default selection runs only when `Strategy == ""`. All existing validation branches stay byte-for-byte the same for explicit values. Covered by the spec requirement "Explicit strategy is preserved".

### D3. Implicit TCP readiness for defaulted rolling only
**(P)** In `buildTakodHealthSpec` (`takod_deploy.go:1680-1685`), when the function would return nil, `Deploy.StrategyDefaulted` is set, and the strategy is `rolling`, it returns `HealthSpec{Port: service.Port, Interval: "2s", Timeout: "2s", Retries: 1, StartPeriod: "0s", WaitAttempts: implicitTCPReadinessAttempts}`. This uses the existing host-side TCP probe path (`reconcile.go:1402-1404`; test `TestReconcileServiceUsesHostSideTCPHealthWithoutDockerHealthCommand`). It needs no takod change and no capability.

Explicit `rolling` keeps the running-only gate, for compatibility.

**Budget (resolved):** `implicitTCPReadinessAttempts = 60` is a named constant in `pkg/deployer`. It is *not* derived from `deploymentHealthWaitAttempts` or the `healthCheck` defaults, so tuning either of those does not silently change the implicit probe. Each attempt is a 1 s sleep plus a dial with a 2 s timeout (`pkg/takod/reconcile.go:1364-1383`, `1498-1513`), so the wall-clock ceiling is between 60 s (port refused immediately) and about 180 s (every dial times out). User docs describe it as "about a minute"; the exact ceiling is not part of the spec.
- *Alternative rejected:* require a `healthCheck` for defaulted rolling. That would turn a silent default into a validation error and break the "validates today" rule.

### D4. Recreate retain/restore protocol on takod
**(P)** `ReconcileServiceRequest` gains `FailurePolicy string` with the values `""`/`remove` (legacy), `restore`, and `retain-failed`. The response gains `RetainedContainers []string` and `RestoreToken string`. When `FailurePolicy == restore` and the strategy is `recreate`, `ReconcileService` replaces the `removeServiceContainersForReconcile` call at `reconcile.go:192` with `retainServiceContainersForReconcile`. That function:
1. Lists the service containers (same filter as `reconcile.go:1088-1095`) and removes any already-retained leftovers. A container is a retained leftover when its current name equals `runtimeid.RetainedContainerName(project, env, service, slot)` recomputed from its own `tako.*` labels (D9). Names are never matched by suffix. This satisfies the "interrupted deploy leftovers" scenario.
2. Gracefully stops the current containers with the same `docker stop` semantics as `removeContainerIDs`. It does **not** `rm` them.
3. Runs `docker rename <name> <retained-name>`, where `<retained-name>` is `runtimeid.RetainedContainerName(...)` for that container's slot (D9). This frees the fixed slot name for the new container.

Next, the loop at `reconcile.go:209-230` runs unchanged, except for the failure branches. On failure, takod calls `cleanupStartedContainers` and then `restoreRetained`, which runs `docker rename <retained-name> <name>` and `docker start` for each container. takod then returns a typed error body `{error, restored: true|false, restoreError}`.

On success, takod keeps the retained containers and returns them along with the token. Two new endpoints act on that token:
- `POST /v1/services/finalize` (token) removes the retained containers.
- `POST /v1/services/restore` (token) gracefully removes the new containers, renames the retained ones back and starts them.

Both endpoints are idempotent and scoped by project, environment, service and revision. The token is the revision ID plus a request nonce, not a secret.

`retain-failed` (used for `rollbackOnFailure: false`) replaces `cleanupStartedContainers` with stop-without-rm. For `recreate` it still removes the old containers first (legacy order). For `rolling`, the failed revision-scoped containers are left stopped. They are removed by the next reconcile of the same revision (`removeServiceContainersMatchingRevision`, `reconcile.go:1073`) or by the prune of another revision (`reconcile.go:1082-1087`).

- *Feasibility (observed):*
  - A stopped container releases host-port bindings, so raw `ports` services can restart the new container on the same port.
  - Mesh upstream ports for `recreate` are slot-keyed, not revision-keyed (`meshUpstreamRevisionForStrategy`, `takod_deploy.go:1526`). They are rebound on restart.
  - Network aliases are per-endpoint and survive the rename and the stop.
  - Named volumes stay attached to the stopped container, but nothing holds open file handles. Persistent databases restart against the same data.
  - `docker ps` (actual state) ignores stopped retained containers (`actual.go:70`).
- *Alternative rejected:* re-create the previous revision from deployment history on failure. It needs the previous image, which may already be pruned, plus env, files and secrets re-resolution in the CLI. It is slower, and it fails in exactly the situations (registry outage, missing secrets) where a restore is most needed. It is kept as a possible future fallback when no retained container exists.
- *Alternative rejected:* start the new container under a temporary name before stopping the old one. For `recreate` that defeats the point (port and volume exclusivity) and is what `rolling` already does.

### D5. CLI coordinates restore across nodes, per service
**(P)** In `deployServiceTakod`, the `Reconcile` closure (`takod_deploy.go:499-508`) collects per-node results instead of returning on the first error. The outcome is resolved as follows:
- **All nodes succeed:** run `finalize` on every node, in parallel. A finalize error is logged as a warning, because the deploy did succeed. The leftovers are removed by the next deploy (D4 step 1).
- **Any node fails, policy `restore`:** run `restore` on every node that succeeded. Nodes that failed already restored themselves locally. All per-node restore results are aggregated into a `deployer.RecoveryReport{Status, Revision, Nodes[]}`, which `ReleaseRunFor`-style accessors expose to the engine.

Rolling needs no restore call. Its old revision is never touched before the proxy switch, so the report is `restored` with `Mode: "not-cut-over"`. Restore is scoped to one service (D6), matching the engine's stop-at-first-failure loop (`deploy.go:955`).

### D6. rollbackOnFailure semantics
**(P)** Change the field to `RollbackOnFailure *bool` (`types.go:476`). nil and `true` map to `restore`; `false` maps to `retain-failed`. Update the run-kind rejection (`validator.go:1805`) to `deploy.RollbackOnFailure != nil`.

Why default to `true`: the user-visible promise is "a failed deploy leaves the previous version up". The debug case is explicit opt-out.

Why `false` keeps failed containers instead of today's delete: deleting them destroys the only evidence. Today's error message embeds 50 log lines (`reconcile.go:1385-1389`), but that is lossy.

YAML compatibility: `rollbackOnFailure: true|false` still parses. Only the explicit `false` changes the hash once.

### D7. Capability gate `deploy.restore-on-failure-v1`
**(P)** Add `CapabilityDeployRestoreOnFailureV1 = "deploy.restore-on-failure-v1"` next to `pkg/takod/server.go:141` and advertise it in the capability list (`server.go:2544`).

The CLI preflights it in the `Preflight` closure (`takod_deploy.go:450-487`) with `preflightTakodCapability` (`takod_deploy.go:558`) whenever the derived policy is not `remove`. The policy is not `remove` when either:
- the strategy is `recreate` and the policy is `restore`, or
- `rollbackOnFailure` is explicitly `false`.

On a missing capability:
- **Explicit `rollbackOnFailure`:** return the `preflightTakodCapability` error, which already names the node and feature and recommends upgrade. This fails closed, per the project rule.
- **Defaulted:** if *any* assigned node lacks the capability, downgrade the policy to `remove` for this service on *all* of its nodes (no mixed per-node policies, because D5's cross-node restore needs every node to have retained containers). Emit one `events.TypeWarning` with "node(s) X[, Y] lack deploy.restore-on-failure-v1; previous revision of <service> cannot be restored on failure; run `tako upgrade servers`", and set `recovery.status: "unsupported"` should the rollout fail. With policy `remove` the CLI omits `FailurePolicy` from the request entirely.

*Why not fail closed for the default too (resolved, deliberate exception):* the project rule is to fail closed when a node lacks a needed capability. This fallback is a named exception to that rule. Every existing user upgrading the CLI before the nodes would otherwise be blocked from deploying at all. Because the CLI does not send `FailurePolicy` when downgraded, the node sees byte-for-byte today's request, so nothing gets less safe than it is today. Explicit `rollbackOnFailure` settings still fail closed.

`runTakodServiceRollout` currently skips `Preflight` unless `needsCapabilities` is true (`takod_deploy.go:448,523-528`). That predicate must include "failure policy needs capability".

### D8. Reporting
**(P)**
- **Plan:**
  - `PlanChange` gains `Strategy string` and `StrategySource string` (`explicit` or `default`), both `omitempty`, filled in `newDeployPlanDocument` (`types.go:220-252`). Runs and jobs leave them empty.
  - `detectChanges` (`reconcile.go:304-311`) prepends "Deploy strategy default changed: recreate → rolling" when `actual.DeployStrategy == recreate`, `desired.Deploy.StrategyDefaulted`, and the desired strategy is `rolling`.
  - `FormatPlanVerbose` shows the strategy column.
- **Result:**
  - `ServiceOutcome` gains `Strategy string` and `Recovery *RecoveryOutcome{Status, Revision, Mode, Nodes []{Server, Status, Error}}`.
  - Add `OutcomeRestored = "restored"`.
  - `DeployResult.Status` stays `failed`, and the exit code stays 1 (MACHINE-INTERFACE table, `docs/MACHINE-INTERFACE.md:470-476`).
- **Events:** `deploy.service.restore.started`, `deploy.service.restored`, `deploy.service.restore_failed` in `events.go` next to line 61.
- **Notification:** append `"recovery": <status>` to `Details` and a sentence to `Message` (`deploy.go:1158-1171`).
- **History:** `ServiceStateDocument` gains `deployStrategy` and `recovery` (`state.go:258-274`). It is written for the failed service in the failed deployment record (`deploy.go:950-955`).
- **Docs:** `docs/MACHINE-INTERFACE.md` documents the new fields and enum value. No new command is added, so `cmd/machine_coverage_test.go` is untouched.
- **Rollback and scale (resolved):** `tako rollback` and `tako scale` get the restore protocol automatically through `deployServiceTakod`, but this change does not add `recovery` to their result documents. `add-project-wide-rollback` carries it into its `services[]`; `scale` can follow later as an additive change.

### D9. Retained container names come from `runtimeid`
**(P)** Add `runtimeid.RetainedContainerName(project, env, service string, slot int) string` to `pkg/runtimeid/runtimeid.go`, built with the existing `compactName` helper (`runtimeid.go:13,23-26`). `compactName` bounds the result to ≤128 characters by construction (hashing the overflow), so the name always passes `isSafeContainerName` (`pkg/takod/reconcile.go:695-697`). The name embeds a fixed `prev` component so it can never collide with a live slot name or a revision-scoped name.

takod finds leftovers by reading each candidate container's `tako.project`, `tako.environment`, `tako.service` and slot labels, recomputing `RetainedContainerName`, and comparing it to the container's actual name. It never matches a `--prev` (or any other) suffix. There is no fallback naming scheme: if the computed name is somehow unsafe, the reconcile fails before stopping anything.

- *Alternative rejected:* `<name>--prev` with a `-p<short-revision>` fallback when too long. Two schemes double the matching logic, and suffix matching can catch user-named containers.

## Risks / Trade-offs

- **[Risk]** Double memory or CPU during rolling overlap on small VPS hosts → **Mitigation:** document in CONFIGURATION.md; `strategy: recreate` opt-out. The existing disk-floor preflight is unaffected.
- **[Risk]** The first deploy after the upgrade redeploys every eligible service (hash and revision change) → **Mitigation:** the plan reason makes it explicit. It goes through `rolling`, so there is no downtime. Release-notes callout.
- **[Risk]** The implicit TCP probe fails for apps that bind their port late or only on another interface (127.0.0.1) → **Mitigation:** the error names the probe and suggests `healthCheck` or `strategy: recreate`. Such apps were already unreachable through the proxy.
- **[Risk]** `StrategyDefaulted` (`json:"-"`) is lost when a `ServiceConfig` is JSON round-tripped (for example config materialization from actual state, `pkg/configmaterialize`) → **Mitigation:** loss only disables the implicit TCP probe and the plan reason. Both degrade to pre-change behavior. `DefaultDeployStrategy` is re-run on re-validation.
- **[Risk]** Restoring a `recreate` database after the new image already migrated data → **Mitigation:** documented limitation (proposal Out of scope). `rollbackOnFailure: false` opt-out.
- **[Risk]** A CLI crash between reconcile success and finalize leaves stopped retained containers (D9 names) → **Mitigation:** invisible to actual state, removed by the next reconcile of the service (D4 step 1) or by `tako cleanup` stopped-container pruning (`pkg/takod/cleanup.go:231-249`).
- **[Risk]** `tako logs` lists stopped containers (`pkg/takod/logs.go:101-102`, `ps -a`) and may show retained or failed containers → acceptable, useful for debugging. The sibling change `improve-logs-command` may filter by label.
- **[Trade-off]** Restore is per service. A multi-service deploy can end with mixed revisions across services. This is consistent with today's behavior and with `add-project-wide-rollback`.
- **[Trade-off]** `recreate` still has a downtime window: the old stop, then the new start, then on failure the restore start. The change makes failure recoverable, not zero-downtime.

## Migration Plan

1. Ship CLI and takod together. Nodes get the capability through `tako upgrade servers`.
2. The CLI against old nodes:
   - The rolling default works.
   - The recreate restore falls back with a warning.
   - An explicit `rollbackOnFailure` fails closed.
3. Users who want the old behavior add `deploy.strategy: recreate`. `tako config` shows `(default)` vs explicit.
4. Rolling the change back: an older CLI re-defaults to `recreate`, so the hash changes again and services redeploy once via recreate. Stopped retained containers left behind are removed by the older takod's `ps -a` sweep (`reconcile.go:1088-1103`) on the next deploy.
