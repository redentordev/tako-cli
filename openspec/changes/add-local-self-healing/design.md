# Design

## Context

See proposal.md for motivation and specs/self-healing/spec.md for the behavior contract. Observed facts that shape the approach:

- **takod cannot recreate a container on its own.** `ReconcileServiceRequest` (`pkg/takod/reconcile.go:37-82`) carries `EnvFileContent` and `RegistryAuths`; env content is written to a temp file only for the `docker run` and deleted (`prepareServiceEnvFile`, `:823-834`), and registry credentials are request-scoped and "never persisted (ADR 10)" (`:77-80`). Desired state stores only env *keys* (`DesiredService.EnvKeys`, `pkg/takodstate/state.go:102`). An existing container keeps its env in its Docker config, so `docker restart` and `docker start` work without secrets; `docker run` does not.
- **Docker health exists only for `healthCheck.command`.** `--health-cmd` is passed only when `HealthSpec.Command` is set (`reconcile.go:1300-1314`). `healthCheck.path` / TCP checks are executed by takod during the rollout only (`waitForContainerHealthy` `:1397`, `checkContainerReadiness` `:1454`, `checkContainerHTTPPath` `:1507`, `checkContainerTCPHealth` `:1546`). Docker never restarts an unhealthy container.
- **Docker already restarts crashes.** Default restart policy is `unless-stopped` (`reconcile.go:168-170`, validated by `isSafeRestartPolicy` `:749`). An exited container with `always`/`unless-stopped` that Docker is not restarting was, in practice, stopped by someone (`docker stop`) or by takod itself (retention, retain-failed).
- **Some stopped containers must stay stopped.** Restore-on-failure renames the previous containers to a retained name, sets `--restart no` and stops them (`retainServiceContainersForReconcile`, `pkg/takod/restore.go:331-423`); the record lives at `retainedStatePath` (`restore.go:161`). `retain-failed` stops failed new containers and keeps them for their logs (`stopFailedContainers`, `restore.go:478-489`). Blue-green keeps inactive containers labeled `tako.active=false` (`ParseActualState`, `pkg/takod/actual.go:125-160`).
- **Container revision labels are per-service hashes,** not the desired `RevisionID`: the CLI sends `takodServiceRevisionID(project, env, service, image, service)` (`pkg/deployer/takod_deploy.go:577`) and takod stamps `tako.revision` (`reconcile.go:1260-1262`).
- **Per-environment mutual exclusion is the operation lease.** Lease files live at `<dataDir>/leases/<project>/<env>.json`, created with `O_EXCL` and serialized by `leaseMu` (`pkg/takod/lease.go:61-163`). Every mutating CLI operation (deploy, rollback, promote, scale/start/stop, maintenance, preview teardown) acquires it on the target nodes; a held lease surfaces in the CLI as `LockedError` (exit 3, `pkg/engine/errors.go:44`).
- **Autonomous takod loops already follow a safety pattern.** The build-cache prune loop takes the maintenance barrier, skips while a controller worker operation is active, and runs under the snapshot mutation lock (`runBuildCachePruneLoop`, `pkg/takod/server.go:659-712`; `activeWorkerOperation`, `fence.go:1089`). The HTTP lifecycle gate additionally refuses workload mutations on non-schedulable nodes, with a deployment-deny latch, and while a volume restore transaction is pending (`enrolledLifecycleHandler`, `lifecycle_gate.go:100-217`; `ensureNoPendingRestoreTransactionsOnNode`, `backup.go:412`).
- **takod already writes autonomous events.** The certificate scheduler appends `cert.renew.*` events to the environment event log with `AppendStateEvent` (`certificate_scheduler.go:295-315`, `state.go:126-154`).
- **Desired state per environment is on every target node** at `desired/<project>/<env>/revision.json` (`state.go:281`), with per-service `Replicas`, `Assignments` (`{slot, nodeId, node}`, `pkg/scheduler/sticky.go:10`), `Restart`, `Persistent`, `WorkloadKind`, `HealthCheck`, `RemovalPending` (`pkg/takodstate/state.go:73-117`). It is written by `sanitizeDesiredService` (`state.go:806`) on every deploy, scale, start and stop.
- **`tako stop` is scale-to-zero** (`cmd/stop.go:16-31`). **`tako maintenance`** runs a companion service `<service>-maintenance` (`cmd/maintenance.go:717`) and repoints the proxy; the real service's containers are left alone.
- **Capability gating** is `RequireTakodCapabilityContext` (`pkg/engine/takod.go:238`) against the list in `pkg/takod/server.go:2714`.

## Goals / Non-Goals

**Goals:**
- Restart a container that stays unhealthy, and start a container takod started earlier that has exited and that Docker will not restart, on the same node, without operator action.
- Never interfere with an operator or a Tako operation: deploys, rollbacks, promotions, scaling, maintenance, preview teardown, backups/restores, node upgrades and controller operations always win.
- Bounded behavior: no restart storms, an explicit give-up with an alert, and an audit trail that answers "what did takod do and why".
- Off by default; explicit opt-in per environment or service; persistent services need an explicit per-service opt-in.

**Non-Goals:**
- Moving or rescheduling workloads to other nodes, failover, or any change to `state.onUnreachableNode` (stays `block`).
- Recreating missing containers (needs request-scoped secrets; see D3 and open question 2).
- Changing desired state, deployment history or the proxy configuration.
- Running health checks or delivering alerts (owned by `add-takod-health-monitoring`).
- Restarting the proxy, takod itself, jobs or one-off `run` workloads.

## Decisions

### D1. Config shape and effective policy

```yaml
environments:
  production:
    selfHealing:              # defaults for this environment's services
      enabled: true
    services:
      web:
        selfHealing:          # per-service override (all keys optional)
          restartUnhealthy: true
          unhealthyThreshold: 3
      db:
        persistent: true
        selfHealing:
          enabled: true       # persistent services need this explicitly
          restartUnhealthy: false
          startExited: true
```

Keys (both levels): `enabled` (bool), `restartUnhealthy` (bool, default true), `startExited` (bool, default true), `unhealthyThreshold` (int 1-20, default 3), `maxRestarts` (int 1-20, default 3), `window` (duration 5m-24h, default 30m). The effective policy per service is: service keys, else environment keys, else defaults; `enabled` defaults to false. For a `persistent: true` service the environment-level `enabled` is ignored and only the service's own `enabled: true` turns healing on. `selfHealing` on a `kind: job` or `kind: run` service is a validation error. Unknown keys are rejected by strict decode. `extends` inherits the block with normal merge rules, so previews inherit it from their base.

The effective policy is resolved at deploy time and written into desired state as `DesiredService.SelfHealing` (`enabled`, `restartUnhealthy`, `startExited`, `unhealthyThreshold`, `maxRestarts`, `windowSeconds`), omitted when disabled. takod never reads `tako.yaml`; changing the policy takes effect with the next desired-state write (deploy, rollback, promote, scale, start, stop). This keeps one source of truth and avoids a new policy push API.

### D2. Converged record: what takod is allowed to touch

takod writes a node-local **converged record** per service at `<dataDir>/heal/<project>/<env>/services/<service>.json` = `{revision, containers: [{name, id}], restart, recordedAt}`:

- Written by `ReconcileService` after every container started and passed its readiness wait (the success return at `reconcile.go:256-271`), and by restore-on-failure *finalize* (the new containers become the record) and *restore* (the restored previous containers become the record).
- Cleared by `RemoveService`, `ReconcileService` with zero containers (scale to zero), every cleanup that removes the service's containers (`cleanup.go`), and preview teardown.
- Replaced before every reconcile attempt starts (written as `converging: true`), so a failed or interrupted rollout leaves a record that heal ignores (D4).

Heal acts only on containers listed in a non-converging record whose `id` still matches. Retained containers, retain-failed containers, warming containers and anything started by hand are never in a record, so they are never touched. This is the core safety property and is what makes "exited container" safe to act on.

### D3. Actions

| Condition (container in record, service eligible) | Action | Default |
|---|---|---|
| Running, unhealthy for `unhealthyThreshold` consecutive observations and ≥ 60s, past grace | `docker restart <id>` (container's own stop timeout) | `restartUnhealthy: true` |
| Exited/created, restart policy `no` or `on-failure[:N]`, not restarting | `docker start <id>` | `startExited: true` |
| Exited, restart policy `always`/`unless-stopped` | none; status `stopped` ("stopped outside Tako") | — |
| Docker is restarting it (`State.Restarting`) | none; status `docker-restarting` (crash loop is detected and alerted by health monitoring) | — |
| Listed in record but no longer exists | none; status `missing` with repair hint `tako deploy -e <env> --service <svc>` | — |
| Desired replicas on this node ≠ record size | none for the service; status `not-converged` | — |

Health observations: Docker's `(unhealthy)` state (`ParseContainerHealth`, `actual.go:211`) for `healthCheck.command` services; per-container observations from `add-takod-health-monitoring` for `healthCheck.path`/TCP services. An observation older than 3 × the check interval counts as "unknown", which resets nothing and never triggers an action. A service with no health source never gets unhealthy restarts.

Grace: no action on a container until `max(healthCheck.startPeriod, 2m)` after its start (`State.StartedAt`) and until 5m after its converged record was written. Recreating missing containers is out of scope (D-context: secrets are request-scoped); the status names the exact repair command instead.

### D4. Eligibility gate (checked every tick, and again under the lease right before acting)

An environment is skipped while any of these holds on the node: an unexpired operation lease for `(project, env)` held by anyone else; `activeWorkerOperation` (controller worker fence); the node-upgrade lifecycle exclusion cannot be taken; a pending volume restore transaction (`ensureNoPendingRestoreTransactionsOnNode`); the node lifecycle is not `schedulable` or a deployment-deny latch exists (enrolled nodes); a node-wide or environment pause is active (D7); no desired-state document exists.

A service is skipped when: it has no effective enabled policy; `WorkloadKind` is `job` or `run`; `RemovalPending`; no assignment targets this node or desired replicas is 0; a retained-state file exists for it (`retainedStatePath`); a backup or restore of one of its volumes holds the backup directory lock; a `<service>-maintenance` container exists for the environment; its converged record is missing or `converging`; it is `gave_up` (D5) or paused.

A container is skipped when it is not in the converged record, its labels say `tako.active=false`, or it is in its grace period.

### D5. Backoff, give-up, circuit breaker

- **Per container cooldown:** after an action, no further action on that container for 2m, doubling per action (2m, 4m, 8m…) capped at 30m. A container healthy for a full `window` resets its count and cooldown.
- **Give-up:** when a service's containers on the node accumulate `maxRestarts` actions within `window`, the service becomes `gave_up` on that node: no more actions, one `heal.gave_up` event and one alert through the health-monitoring alert API. It clears automatically when a new converged record with a different revision is written (a deploy) or with `tako heal reset`.
- **Serial actions:** at most one heal action at a time per node; after restarting one replica of a service, the next replica of the same service waits until the first is healthy again or its cooldown ends.
- **Node circuit breaker:** if more than half of the node's eligible services (minimum 2) need an action in the same tick, or more than 6 actions ran on the node within 10m, healing on the node pauses for 30m with one `heal.circuit_open` event and alert. Mass failure points to the host, the network or a shared dependency, where restarts make things worse.

### D6. Coordination with deploys: the lease

Before acting, the heal loop acquires the environment's operation lease on its own node through the existing `AcquireLease` (`lease.go:61`) with operation `self-heal`, who `takod/<node>`, a fresh ID and TTL 5m, re-runs the eligibility gate (D4) under it, performs one `docker restart`/`docker start` (bounded by a 3m context), records the outcome and releases the lease. It never waits for health while holding the lease; the outcome is evaluated on later ticks. The loop also holds the maintenance barrier and the snapshot mutation lock for the action, exactly like the build-cache prune loop.

A CLI operation that meets a lease whose `operation` is `self-heal` waits and retries for up to 60s before reporting `LockedError`. Leases held by anything else keep today's behavior. On enrolled clusters the node-local lease file is the same mutex the controller's backing lease uses (`renewLeaseTo`, `lease.go:200`), so a controller deploy sees `self-heal` like any other holder; the retry rule covers it.

### D7. Pause, resume, reset

`PUT /v1/heal` (authenticated, fenced; registered as a lifecycle-safe control-plane path like `/v1/alerts` so it works on cordoned nodes) accepts `pause` (`project`, optional `environment`, optional `service`, `until`), `resume` and `reset` (`project`, `environment`, `service`; clears `gave_up`, counters and cooldowns). Pauses expire; default 1h, maximum 7d, `--until-resume` for indefinite. State lives in `<dataDir>/heal/<project>/<env>/control.json`, written atomically.

### D8. Audit trail

- **Heal journal:** `<dataDir>/heal/<project>/<env>/journal.jsonl`, one JSON line per action or decision change (`restarted`, `started`, `failed`, `gave_up`, `reset`, `paused`, `resumed`, `circuit_open`, `circuit_closed`, `skipped_stopped`, `missing`), capped at the last 1000 lines (rotated by rewrite). Fields: time, node, service, container, revision, reason (redacted), action result.
- **State events:** the same actions (not repeated skips) are appended to the environment event log with `AppendStateEvent`, types `heal.restarted`, `heal.started`, `heal.failed`, `heal.gave_up`, `heal.reset`, `heal.paused`, `heal.resumed`, `heal.circuit_open`, registered in `pkg/takoapi/events/events.go`.
- **Not deployment history:** heal actions do not create deployments, change desired state or appear in `tako history`; they change no revision.
- **Alerts:** `heal.gave_up`, `heal.circuit_open` and `heal.failed` (an action that errored) go through the health-monitoring alert-source API with severity `critical`/`warning`; successful restarts are `info` and alerting on them is controlled by that change's routing (default off).

### D9. CLI and machine interface

- `tako heal status [-e env] [--service s] [--server n]` queries `GET /v1/heal?project=&environment=` on every target node and shows per node and service: effective policy, state (`ok`, `watching`, `cooldown`, `gave_up`, `paused`, `not-converged`, `disabled`), per-container state (`healthy`, `unhealthy n/N`, `stopped`, `missing`, `docker-restarting`), and the last 10 journal entries. `--output json` returns `HealStatus`.
- `tako heal pause|resume [-e env] [--service s] [--for 1h|--until-resume]` and `tako heal reset -e env --service s` call `PUT /v1/heal` on every target node (or `--server`) and return `HealChangeResult` with per-node outcomes.
- `tako ps` rows gain an additive `heal` object (`state`, `lastAction`, `lastActionAt`) when the node advertises `heal.local-v1` and the service has a policy; the text STATUS cell appends `heal:gave_up`/`heal:paused` only for those two states.
- Exit codes follow the existing classes: unsupported node → exit 2 with the upgrade message; lease conflicts on pause/reset are not possible (they take no lease); node unreachable → existing transport class.

### D10. Capability and rollout

takod advertises `heal.local-v1`. `tako deploy` (and rollback/promote/scale/start/stop, which rewrite desired state) fails before any mutation when any service's effective policy is enabled and a target node lacks it, with `takod does not support self-healing (missing capability heal.local-v1); upgrade the node agent` (exit 2). Older takod ignores the unknown `selfHealing` desired-state field, but the gate prevents silently unprotected services. Rollout order: `tako upgrade servers`, then enable in config.

## Risks / Trade-offs

- **Fighting a deploy.** Mitigated by the lease (D6), the gate re-check under the lease, converged records that never include in-flight containers (D2), and the post-deploy grace. A deploy that starts during a 1-3s restart waits at most 60s.
- **Restarting stateful services.** A database restart during crash recovery or a long migration can make things worse. Persistent services require an explicit per-service opt-in, the grace period honors `startPeriod`, and `restartUnhealthy` can be disabled while keeping `startExited`.
- **Restart storms / thundering herd.** Serial actions, per-container exponential cooldown, give-up per service and the node circuit breaker bound the worst case to 6 actions per 10 minutes per node.
- **Undoing deliberate manual stops.** Avoided for the default restart policies by never starting `always`/`unless-stopped` containers (D3). With `on-failure`/`no`, a manual `docker stop` is undone; documented, and `tako heal pause` or `tako stop` are the supported ways to keep it down.
- **False unhealthy from the checker.** Stale observations count as unknown, never as failures; thresholds require both N consecutive failures and 60s.
- **Clock and restart of takod.** Counters, cooldowns and gave-up state persist in `<dataDir>/heal/…`, so restarting takod does not reset the budget.
- **Data dir growth.** Journal capped at 1000 lines per environment; records are tiny.

## Migration Plan

No config migration; nothing changes until a config enables `selfHealing` and is deployed. Existing deployments get converged records on their next deploy; until then heal reports `not-converged` and does nothing. Rollback of the feature: remove `selfHealing` from config and deploy, or `tako heal pause --until-resume`.

## Open Questions

1. **Default on or off?** *Recommendation: off (opt-in) in this release.* Revisit making `restartUnhealthy` default-on for non-persistent services after field experience.
2. **Recreate missing containers by cloning a sibling replica's Docker config (`docker inspect` → `docker run`)?** *Recommendation: no.* It would copy env secrets through takod memory, cannot reproduce request-scoped registry auth for a re-pull, and must re-derive aliases, slots and published ports; report `missing` with the repair command instead.
3. **Heal on cordoned nodes?** *Recommendation: no — only on `schedulable` nodes.* Cordon/drain is an operator-driven transition; a restart there can interact with placement-apply cleanup (`withPlacementCleanupOnly`, `lifecycle_gate.go:201-206`).
4. **Should heal take the environment lease, or a separate heal lock?** *Recommendation: the existing lease* (D6) — it is the only mutex every Tako mutation already honors; a new lock would need every operation to learn it.
5. **Where should the per-service grace come from when no `startPeriod` is set?** *Recommendation: fixed 2m minimum*, plus 5m after a converged record is written.
6. **Alert on every successful restart?** *Recommendation: emit the `info` alert, routed off by default* in the health-monitoring alert routing; always alert on `gave_up`, `circuit_open` and failed actions.
7. **Expose heal events in `tako logs`/`tako history`?** *Recommendation: no;* keep `tako history` deployment-only and surface heal via `tako heal status` and the state event log.
8. **Apply to previews?** *Recommendation: yes, inherited from the base like any key;* preview teardown takes the lease and clears converged records, so it is safe.
