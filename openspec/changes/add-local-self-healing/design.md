# Design

## Context

See proposal.md for motivation and specs/self-healing/spec.md for the behavior contract. Observed facts that shape the approach:

- **takod cannot recreate a container on its own.** `ReconcileServiceRequest` (`pkg/takod/reconcile.go:37-82`) carries `EnvFileContent` and `RegistryAuths`; env content is written to a temp file only for the `docker run` and deleted (`prepareServiceEnvFile`, `:823-834`), and registry credentials are request-scoped and "never persisted (ADR 10)" (`:77-80`). Desired state stores only env *keys* (`DesiredService.EnvKeys`, `pkg/takodstate/state.go:102`). An existing container keeps its env in its Docker config, so `docker restart` and `docker start` work without secrets; `docker run` does not.
- **Docker health exists only for `healthCheck.command`.** `--health-cmd` is passed only when `HealthSpec.Command` is set (`reconcile.go:1300-1314`). `healthCheck.path` / TCP checks are executed by takod during the rollout only (`waitForContainerHealthy` `:1397`, `checkContainerReadiness` `:1454`, `checkContainerHTTPPath` `:1507`, `checkContainerTCPHealth` `:1546`). Docker never restarts an unhealthy container. `checkContainerReadiness` also runs the deploy smoke check, and for command checks it returns the last 50 log lines inside the error, so its error text must never reach a journal or alert.
- **Health monitoring does not probe individual containers.** `add-takod-health-monitoring` checks `http` monitors through the node's load-balanced proxy (one result per domain, not per replica) and samples every `tako.project` container's Docker state with one shared sampler (`ContainerObservations()`, its D7: status, restarting, exit code, OOM, restart count, `Health.Status`, restart policy, labels, start and finish times; every 30s). It is the source of Docker state and Docker health for this change, but path and TCP health per container must be probed here.
- **Docker already restarts crashes.** Default restart policy is `unless-stopped` (`reconcile.go:168-170`, validated by `isSafeRestartPolicy` `:749`). An exited container with `always`/`unless-stopped` that Docker is not restarting was, in practice, stopped by someone (`docker stop`) or by takod itself (retention, retain-failed). A container that exited with code 0 under `no` or `on-failure` finished on purpose: Docker does not restart it either.
- **Some stopped containers must stay stopped.** Restore-on-failure renames the previous containers to a retained name, sets `--restart no` and stops them (`retainServiceContainersForReconcile`, `pkg/takod/restore.go:331-423`); the record lives at `retainedStatePath` (`restore.go:161`). Renaming keeps the container ID. `retain-failed` stops failed new containers and keeps them for their logs (`stopFailedContainers`, `restore.go:478-489`). Blue-green keeps inactive containers labeled `tako.active=false` (`ParseActualState`, `pkg/takod/actual.go:125-160`).
- **Container revision labels are per-service hashes,** not the desired `RevisionID`: the CLI computes `deployplan.ServiceRevisionID(project, env, service, image, serviceConfig)` from the full service config (`pkg/deployplan/revision_identity.go:13`) and takod stamps `tako.revision` (`reconcile.go:1260-1262`). takod holds only the sanitized `DesiredService`, so it cannot recompute that hash; it can compare the desired `image` and the label it stamped. The container config hash is an explicit field list (`reconcile.SafeServiceConfigHash`, `pkg/reconcile/config_hash.go:106`), so a new config key changes container identity only if it is added there.
- **Per-environment mutual exclusion is the operation lease, but not on enrolled workers.** Lease files live at `<dataDir>/leases/<project>/<env>.json`, created with `O_EXCL` and serialized by `leaseMu` (`pkg/takod/lease.go:61-163`). Every mutating CLI operation acquires it on the target nodes; a held lease surfaces in the CLI as `LockedError` (exit 3, `pkg/engine/errors.go:44`). On an enrolled cluster the controller's backing lease is the lease file on the controller node (`renewLeaseTo`, `lease.go:200`); workers are protected by the durable worker fence (`control/worker-fence.json`, `activeWorkerOperation`, `fence.go:1089`), whose activation takes `workerFenceAdmissionMu` for writing while mutating handlers hold it for reading (`fence.go:89`, `lifecycle_gate.go:160-183`).
- **The snapshot locks are shared.** `recovery.AcquireMutationLock` and the maintenance barrier are shared flocks: they exclude snapshots and node upgrades, not other mutators.
- **Autonomous takod loops already follow a safety pattern.** The build-cache prune loop takes the maintenance barrier, skips while a controller worker operation is active, and runs under the snapshot mutation lock (`runBuildCachePruneLoop`, `pkg/takod/server.go:659-712`). The HTTP lifecycle gate additionally refuses workload mutations on non-schedulable nodes, with a deployment-deny latch, and while a volume restore transaction is pending (`enrolledLifecycleHandler`, `lifecycle_gate.go:100-217`; `ensureNoPendingRestoreTransactionsOnNode`, `backup.go:412`).
- **takod already writes autonomous events.** The certificate scheduler appends `cert.renew.*` events to the environment event log with `AppendStateEvent` (`certificate_scheduler.go:295-315`, `state.go:126-154`).
- **Desired state per environment is on every target node** at `desired/<project>/<env>/revision.json` (`state.go:281`), with per-service `Image`, `Replicas`, `Assignments` (`{slot, nodeId, node}`, `pkg/scheduler/sticky.go:10`), `Restart`, `Persistent`, `WorkloadKind`, `HealthCheck`, `Port`, `RemovalPending` (`pkg/takodstate/state.go:73-117`). It is written by `sanitizeDesiredService` (`state.go:806`) on every deploy, scale, start and stop. takod state documents are decoded without `DisallowUnknownFields`, so an older takod ignores a new field.
- **`tako stop` is scale-to-zero** (`cmd/stop.go:16-31`). **`tako maintenance`** runs a companion service `<service>-maintenance` (`cmd/maintenance.go:717`) and repoints the proxy; the real service's containers are left alone.
- **Capability gating** is `RequireTakodCapabilityContext` (`pkg/engine/takod.go:238`) against the list in `pkg/takod/server.go:2714`.

## Goals / Non-Goals

**Goals:**
- Restart a container that stays unhealthy, and start a container takod started earlier that crashed and that Docker will not restart, on the same node, without operator action.
- Never interfere with an operator or a Tako operation: deploys, rollbacks, promotions, scaling, maintenance, preview teardown, backups/restores, node upgrades and controller operations always win.
- Bounded behavior: no restart storms, an explicit give-up with an alert, and an audit trail that answers "what did takod do and why".
- Off by default; explicit opt-in per environment or service; persistent services need an explicit per-service opt-in.

**Non-Goals:**
- Moving or rescheduling workloads to other nodes, failover, or any change to `state.onUnreachableNode` (stays `block`).
- Recreating missing containers (needs request-scoped secrets; see D3 and resolved question 2).
- Changing desired state, deployment history or the proxy configuration.
- Domain-level health checks, Docker state sampling or alert delivery (owned by `add-takod-health-monitoring`).
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

**`selfHealing` is not part of container identity.** It is not added to `SafeServiceConfigHash` or `ServiceRevisionID`, so enabling, tuning or disabling healing never recreates a container. It is part of the desired-revision document hash, which only names the desired revision.

### D2. Converged record: what takod is allowed to touch

takod writes a node-local **converged record** per service at `<dataDir>/heal/<project>/<env>/services/<service>.json` = `{image, revision, containers: [{name, id}], restart, recordedAt}`, where `image` is the request's image ref and `revision` is the `tako.revision` label takod stamped:

- Written by `ReconcileService` after every container started and passed its readiness wait (the success return at `reconcile.go:256-271`), and by restore-on-failure *finalize* (the new containers become the record) and *restore* (the restored previous containers become the record).
- Cleared by `RemoveService`, `ReconcileService` with zero containers (scale to zero), every cleanup that removes the service's containers (`cleanup.go`), and preview teardown.
- Replaced before every reconcile attempt starts (written as `converging: true`), so a failed or interrupted rollout leaves a record that heal ignores (D4).

**Validity.** A record is valid only when it is not converging and its `image` equals the service's `image` in the current desired document. An action additionally requires the container's current ID, name and `tako.revision` label to all equal the record's. This covers a takod downgrade and re-upgrade, where an older takod deployed, scaled or retained containers without maintaining records: a new image invalidates the record, replaced containers no longer match by ID, and retained containers (renamed, same ID) no longer match by name.

Heal acts only on containers listed in a valid record. Retained containers, retain-failed containers, warming containers and anything started by hand are never in a record, so they are never touched. This is the core safety property and is what makes "exited container" safe to act on.

### D3. Actions

| Condition (container in valid record, service eligible) | Action | Default |
|---|---|---|
| Running, unhealthy for `unhealthyThreshold` consecutive observations and ≥ 60s, past grace | `docker restart <id>` (container's own stop timeout) | `restartUnhealthy: true` |
| Exited with a non-zero exit code or OOM-killed, restart policy `no` or `on-failure[:N]`, not restarting | `docker start <id>` | `startExited: true` |
| Exited with exit code 0, restart policy `no` or `on-failure[:N]` | none; status `completed` (the process finished on purpose) | — |
| Exited, restart policy `always`/`unless-stopped` | none; status `stopped` ("stopped outside Tako") | — |
| Created but never started, paused, or dead | none; status `stopped` | — |
| Docker is restarting it (`State.Restarting`) | none; status `docker-restarting` (crash loop is detected and alerted by health monitoring) | — |
| Listed in record but no longer exists | none; status `missing` with repair hint `tako deploy -e <env> --service <svc>` | — |
| Desired replicas on this node ≠ record size | none for the service; status `not-converged` | — |

**State source.** Container state and Docker health come from monitoring's shared sampler (`ContainerObservations()`); heal starts no inspect loop of its own. The sampler runs whenever any desired service on the node has a self-healing policy. Immediately before acting, under the lease (D6), heal runs one fresh `docker inspect` of that container and acts only if the condition, ID, name, revision label and restart policy still hold.

**Health observations:**
- `healthCheck.command` services: Docker's `Health.Status` from the sampler.
- `healthCheck.path` or TCP services: heal probes each recorded running container itself, reusing takod's `checkContainerHTTPHealth`/`checkContainerTCPHealth` with a `HealthSpec` built from the desired `healthCheck` and `port` exactly as the reconcile request builds it. The deploy smoke check is not run. The probe runs every `healthCheck.interval` (default 30s, minimum 10s) with `healthCheck.timeout`, at most 4 probes in parallel per node.
- An observation older than 3 × its interval counts as "unknown", which resets nothing and never triggers an action. A service with no health source never gets unhealthy restarts.
- Reasons are enum codes (`docker_unhealthy`, `http_status`, `http_timeout`, `connect_failed`, `exited_nonzero`, `oom_killed`) plus numeric values such as the HTTP status or exit code. Error strings and container logs are never recorded or sent.

Grace: no action on a container until `max(healthCheck.startPeriod, 2m)` after its start (`State.StartedAt`) and until 5m after its converged record was written. Recreating missing containers is out of scope (Context: secrets are request-scoped); the status names the exact repair command instead.

### D4. Eligibility gate (checked every tick, and again under the lease right before acting)

An environment is skipped while any of these holds on the node: an unexpired operation lease for `(project, env)` held by anyone else; `activeWorkerOperation` (controller worker fence); the node-upgrade lifecycle exclusion cannot be taken; a pending volume restore transaction (`ensureNoPendingRestoreTransactionsOnNode`); the node lifecycle is not `schedulable` or a deployment-deny latch exists (enrolled nodes); a node-wide or environment pause is active (D7); no desired-state document exists.

A service is skipped when: it has no effective enabled policy; `WorkloadKind` is `job` or `run`; `RemovalPending`; no assignment targets this node or desired replicas is 0; a retained-state file exists for it (`retainedStatePath`); a backup or restore of one of its volumes holds the backup directory lock; a `<service>-maintenance` container exists for the environment; its converged record is missing, converging or invalid (D2); it is `gave_up` (D5) or paused.

A container is skipped when it is not in the valid record, its ID, name or revision label differ from the record, its labels say `tako.active=false`, or it is in its grace period.

### D5. Backoff, give-up, circuit breaker

- **Per container cooldown:** after an action, no further action on that container for 2m, doubling per action (2m, 4m, 8m…) capped at 30m. A container healthy for a full `window` resets its count and cooldown.
- **Give-up:** when a service's containers on the node accumulate `maxRestarts` actions within `window`, the service becomes `gave_up` on that node: no more actions, one `heal.gave_up` state event, and a `gave_up` level opening through the health-monitoring alert-source API (D8). It clears automatically when a new valid converged record with a different revision is written (a deploy) or with `tako heal reset`, which also recovers the alert.
- **Serial actions:** at most one heal action at a time per node; after restarting one replica of a service, the next replica of the same service waits until the first is healthy again or its cooldown ends.
- **Node circuit breaker:** if more than half of the node's eligible services (minimum 2) need an action in the same tick, or more than 6 actions ran on the node within 10m, healing on the node pauses for 30m with one `heal.circuit_open` event and a `circuit_open` alert on subject `@node`. Mass failure points to the host, the network or a shared dependency, where restarts make things worse.

### D6. Coordination with deploys: the lease and the worker fence

Before acting, the heal loop acquires the environment's operation lease on its own node through the existing `AcquireLease` (`lease.go:61`) with operation `self-heal`, who `takod/<node>`, a fresh ID and TTL 5m. On an enrolled node it also holds `workerFenceAdmissionMu` for reading from the gate re-check until the action returns, and re-checks `activeWorkerOperation` under it, so a controller operation cannot activate the worker fence between the check and the action; fence activation waits at most for the action's bound. It then re-runs the eligibility gate (D4) and the fresh inspect (D3), performs one `docker restart`/`docker start` (bounded by a 3m context), records the outcome and releases both. It never waits for health while holding them; the outcome is evaluated on later ticks. The loop also holds the maintenance barrier and the snapshot mutation lock for the action, exactly like the build-cache prune loop; those shared locks only exclude snapshots and node upgrades.

A CLI operation that meets a lease whose `operation` is `self-heal` waits and retries for up to 60s before reporting `LockedError`. Leases held by anything else keep today's behavior. On the controller node of an enrolled cluster the local lease file is the controller's backing lease, so a controller operation meets `self-heal` like any other holder and the same retry rule applies.

### D7. Pause, resume, reset

`PUT /v1/heal` (authenticated, fenced; registered as a lifecycle-safe control-plane path like `/v1/alerts` so it works on cordoned nodes) accepts `pause` (`project`, optional `environment`, optional `service`, `until`), `resume` and `reset` (`project`, `environment`, `service`; clears `gave_up`, counters and cooldowns). Pauses expire; default 1h, maximum 7d, `--until-resume` for indefinite. State lives in `<dataDir>/heal/<project>/<env>/control.json`, written atomically.

### D8. Audit trail and alerts

- **Heal journal:** `<dataDir>/heal/<project>/<env>/journal.jsonl`, one JSON line per action or decision change (`restarted`, `started`, `failed`, `gave_up`, `reset`, `paused`, `resumed`, `circuit_open`, `circuit_closed`, `skipped_stopped`, `completed`, `missing`), capped at the last 1000 lines (rotated by rewrite). Fields: time, node, service, container, revision, reason code and numbers (D3), action result code.
- **State events:** the same actions (not repeated skips) are appended to the environment event log with `AppendStateEvent`, types `heal.restarted`, `heal.started`, `heal.failed`, `heal.gave_up`, `heal.reset`, `heal.paused`, `heal.resumed`, `heal.circuit_open`, `heal.circuit_closed`, registered in `pkg/takoapi/events/events.go`. Node-wide breaker events go to every environment on the node that has a healed service. Dotted names are used only for state events.
- **Not deployment history:** heal actions do not create deployments, change desired state or appear in `tako history`; they change no revision.
- **Alerts** use kind `selfheal` of the health-monitoring alert-source API (its design D13). The kind is scope-scoped and is subscribed by every scope that has an effective policy and any notification target:
  - `gave_up`: critical level on the service subject; events `heal_gave_up`/`heal_resumed`.
  - `circuit_open`: critical level on subject `@node`; events `heal_circuit_open`/`heal_circuit_closed`; deduplicated across scopes by monitoring's target ledger.
  - `action_failed`: warning event on the service subject (a `docker restart`/`start` that errored); event `heal_action_failed`, with monitoring's event cooldown.

  Successful restarts and starts are never delivered as alerts; they are journal entries and state events only.

### D9. CLI and machine interface

- `tako heal status [-e env] [--service s] [--server n]` queries `GET /v1/heal?project=&environment=` on every target node and shows per node and service: effective policy, state (`ok`, `watching`, `cooldown`, `gave_up`, `paused`, `not-converged`, `disabled`), per-container state (`healthy`, `unhealthy n/N`, `stopped`, `completed`, `missing`, `docker-restarting`), and the last 10 journal entries. `--output json` returns `HealStatus`.
- `tako heal pause|resume [-e env] [--service s] [--for 1h|--until-resume]` and `tako heal reset -e env --service s` call `PUT /v1/heal` on every target node (or `--server`) and return `HealChangeResult` with per-node outcomes.
- `tako ps` rows gain an additive `heal` object (`state`, `lastAction`, `lastActionAt`) when the node advertises `node.self-healing-v1` and the service has a policy; the text STATUS cell appends `heal:gave_up`/`heal:paused` only for those two states.
- Exit codes follow the existing classes: unsupported node → exit 2 with the upgrade message; lease conflicts on pause/reset are not possible (they take no lease); node unreachable → existing transport class.

### D10. Capability and rollout

takod advertises `node.self-healing-v1` (`CapabilityNodeSelfHealingV1`). `tako deploy` (and rollback/promote/scale/start/stop, which rewrite desired state) fails before any mutation when any service's effective policy is enabled and a target node lacks it, with `takod does not support self-healing (missing capability node.self-healing-v1); upgrade the node agent` (exit 2). Older takod ignores the unknown `selfHealing` desired-state field, but the gate prevents silently unprotected services. Rollout order: `tako upgrade servers`, then enable in config.

**Downgrade.** A downgraded takod ignores `selfHealing` and the `heal/` tree and never heals. After a re-upgrade, records invalidated by the older takod's changes are ignored (D2) until the next successful reconcile.

### D11. Dependencies

- **`add-takod-health-monitoring`** (required, lands first): the shared container sampler and the alert-source API with kind `selfheal`. A node that advertises `node.self-healing-v1` always advertises `monitoring.health-v1`.
- **`add-disk-hygiene-defaults`** (lands before this change): its hard reclaim never removes containers listed in a converged record, nor exited replicas of the current desired revision, so a stopped replica waiting for `startExited` is never garbage-collected.
- **`add-server-status-history`**: independent.

## Risks / Trade-offs

- **Fighting a deploy.** Mitigated by the lease and the worker-fence read lock (D6), the gate re-check and fresh inspect under them, converged records that never include in-flight containers and are validated against the desired image (D2), and the post-deploy grace. A deploy that starts during a 1-3s restart waits at most 60s.
- **Restarting stateful services.** A database restart during crash recovery or a long migration can make things worse. Persistent services require an explicit per-service opt-in, the grace period honors `startPeriod`, and `restartUnhealthy` can be disabled while keeping `startExited`.
- **Restart storms / thundering herd.** Serial actions, per-container exponential cooldown, give-up per service and the node circuit breaker bound the worst case to 6 actions per 10 minutes per node.
- **Undoing deliberate manual stops.** Avoided for the default restart policies by never starting `always`/`unless-stopped` containers, and for every policy by never starting a container that exited with code 0 (D3). With `on-failure`/`no`, a manual `docker kill` (non-zero exit) is undone; documented, and `tako heal pause` or `tako stop` are the supported ways to keep it down.
- **False unhealthy from the checker.** Stale observations count as unknown, never as failures; thresholds require both N consecutive failures and 60s. The per-container probe uses the same code the rollout uses, so a container that passed its deploy readiness is judged the same way.
- **Leaking secrets through reasons.** Rollout readiness errors can contain container logs. Heal records and sends only reason codes and numbers (D3).
- **Clock and restart of takod.** Counters, cooldowns and gave-up state persist in `<dataDir>/heal/…`, so restarting takod does not reset the budget.
- **Data dir growth.** Journal capped at 1000 lines per environment; records are tiny.

## Migration Plan

No config migration; nothing changes until a config enables `selfHealing` and is deployed. Enabling it rewrites desired state but recreates no container (D1). Existing deployments get converged records on their next reconcile of the service; until then heal reports `not-converged` and does nothing. Rollback of the feature: remove `selfHealing` from config and deploy, or `tako heal pause --until-resume`.

## Resolved questions

1. **Default on or off?** **Off (opt-in).** *Rationale: automatic restarts of production containers must be a deliberate choice; revisit after field experience.*
2. **Recreate missing containers by cloning a sibling replica's Docker config?** **No.** *Rationale: it would copy env secrets through takod memory and cannot reproduce request-scoped registry auth; report `missing` with the repair command.*
3. **Heal on cordoned nodes?** **No, only on `schedulable` nodes.** *Rationale: cordon and drain are operator-driven and interact with placement-apply cleanup.*
4. **Environment lease or a separate heal lock?** **The existing lease, plus the worker-fence read lock on enrolled nodes.** *Rationale: the lease is the mutex every Tako mutation honors; on workers the controller's operations are fenced, not leased, so the fence lock is what excludes them.*
5. **Grace when no `startPeriod` is set?** **Fixed 2m minimum, plus 5m after a converged record is written.** *Rationale: covers slow starts without a new key.*
6. **Alert on every successful restart?** **No, changed from an `info` alert.** *Rationale: the alert-source API carries only warning and critical; successful actions are journal entries and state events, and alerts are for `gave_up`, `circuit_open` and failed actions.*
7. **Expose heal events in `tako logs`/`tako history`?** **No.** *Rationale: `tako history` stays deployment-only; `tako heal status` and the state event log show heal actions.*
8. **Apply to previews?** **Yes, inherited from the base like any key.** *Rationale: preview teardown takes the lease and clears converged records, so it is safe.*
9. **Where do path and TCP health observations come from?** **A per-container probe in the heal loop, reusing the rollout helpers without the smoke check.** *Rationale: monitoring checks domains through the load-balanced proxy, which cannot say which replica is hung.*
10. **Start a container that exited with code 0 under `restart: no`?** **No; report it as `completed`.** *Rationale: Docker treats exit 0 as intended completion under these policies, and restarting it would undo deliberate one-shot behaviour.*

## Implementation notes

Deviations from the plan above, each the safest equivalent found against the code:

- **Pure logic lives in `pkg/selfheal`** (records, control state, journal, engine with an injected clock), so it is unit-tested on every platform; `pkg/takod/heal*.go` is only glue. Task 2.1's `pkg/takod/heal_record.go` is `pkg/selfheal/record.go` plus `pkg/takod/heal_records.go`.
- **Record IDs come from the `docker run -d` output** (the full container ID), not an extra `docker inspect`, so reconcile issues no new Docker calls. A restore rebuilds the record from the converging record's `previous` block, matching the retained short IDs by prefix; without it the record is cleared. A warm-only blue-green reconcile leaves the record untouched; a failed rollout stays converging; finalize keeps the new record.
- **Backup detection** uses running `tako.role=scheduled-backup|backup-verification` containers (from the sampler, and a fresh `docker ps` right before acting) instead of probing the backup directory lock: probing a flock could fail a starting backup.
- **The backup/restore barrier is taken with `TryAcquire`**; a busy barrier skips the tick instead of queueing behind a restore.
- **The loop always runs** but idles (no sampling, no probes, no Docker calls) while no desired service on the node has a policy; `active` gates the shared sampler.
- **`GET /v1/heal` returns the last cached evaluation** and never evaluates, so a status read cannot consume the engine's journal-once decisions. Its default journal length is 20 (`--journal` sets 0-1000) instead of 10.
- **The breaker alert** is reported per scope with subject `@node` (kind `selfheal` is scope-scoped); the target ledger deduplicates it across scopes. The `selfheal` source is registered by `initSelfHealing`, not with the builtin sources.
- **Give-up** triggers on the tick where an action is needed and the budget is spent, even inside a cooldown, so a crash loop gives up promptly. An HTTP/TCP restart re-probes once under the lease and proceeds only on a definite failure.
- **`PUT /v1/heal` is lifecycle-safe but fence-validated** (like `/v1/alerts`). `tako heal pause|resume|reset` therefore takes the environment's operation leases (operation `heal-<action>`) so enrolled workers accept the fenced request, after a capability preflight on every node; D9's "they take no lease" does not hold on enrolled clusters. Pausing while a deploy holds the lease reports the lock (exit 3); healing is blocked during that deploy anyway.
- **`reset` without `--service`** resets every service of the environment and also closes the node circuit breaker.
- **Restart policies** are normalized (`on-failure:N` is `on-failure`, empty is `no`).
- **The capability gate** is in `Deployer.PreflightServiceMutationCapabilities`, which every desired-state-writing command (deploy, rollback, project rollback, promote, scale/start/stop, run, placement apply) already calls, rather than a new `RequireTakodCapabilityContext` call site.
- **The self-heal lease wait** is in `internal/state` lease acquisition (60s, retry every 2s, fresh request ID per attempt, context-aware). The API SDK's `stateclient` keeps its current behaviour.
- **Journal-once state survives tracker expiry.** Recording a journal-once decision (`missing`, `completed`, `skipped_stopped`) marks its tracker seen, so the loop's 10-minute tracker expiry keeps it while the condition lasts; a missing container is journaled once, not every tick (which would rewrite and fsync the journal every 15s and push real decisions out of the 1000-line cap within hours).
- **Failures must be fresh.** A running container counts as `unhealthy` only while its last health observation is fresh (within 3 × the health interval: the probe interval for HTTP/TCP, the sampler interval for Docker health). Older failures show as `unknown` and never trigger an action.
- **Task 6.3** (manual verification on a test server) is not automated here; the behaviours it lists are covered by the takod tests with a fake Docker and real probes against `httptest` servers.
