# Proposal

## Why

Once a deploy finishes, nothing in Tako looks after the running containers:

- **Unhealthy containers stay unhealthy.** Docker marks a container `unhealthy` when its `--health-cmd` fails (`pkg/takod/reconcile.go:1300-1314`), but Docker never restarts an unhealthy container. Services that use `healthCheck.path` have no Docker health check at all: takod probes the path only during the rollout (`waitForContainerHealthy`, `reconcile.go:1397`; `checkContainerReadiness`, `:1454`). A hung process that still holds its port keeps receiving traffic until someone notices.
- **Containers Docker will not restart stay down.** Docker's restart policy (default `unless-stopped`, `reconcile.go:168-170`) covers crashes, but a service configured with `restart: on-failure:N` or `no` stays exited once Docker gives up.
- **Nothing records or explains what happened.** There is no node-local record of what takod last started for a service, no audit trail of automatic actions, and no CLI view of "why is this replica down".

Operators currently find out from users, then run `tako deploy` or `docker restart` by hand. The sibling change `add-takod-health-monitoring` makes takod *notice* problems and alert. This change lets takod *fix* the safe subset of them on the same node, and stop and alert when fixing does not help.

## What Changes

- **Opt-in node-local self-healing in takod.** A new takod loop (`heal`), off unless a service enables it, acts only on containers of the node it runs on. It never moves, creates or removes workloads across nodes, never changes desired state, and keeps `state.onUnreachableNode: block` semantics.
- **Restart unhealthy containers.** After N consecutive failed health observations (default 3, and unhealthy for at least 60s), past the container's start grace period, takod runs `docker restart` on that one container. Health observations come from Docker's own health status and from the per-container check results produced by `add-takod-health-monitoring`.
- **Start exited containers Docker will not restart.** A container that takod itself last started for the service's current revision, whose restart policy is `no` or `on-failure[:N]`, and that is exited, is started again with `docker start`. Containers with `always`/`unless-stopped` that are exited are treated as stopped on purpose and only reported. Missing containers are only reported: takod cannot recreate them because env content and registry credentials are request-scoped and never persisted on nodes (ADR 10); the report names the repair command.
- **Crash-loop protection and give-up.** Per-container exponential cooldown between actions; after `maxRestarts` actions within `window` (defaults 3 in 30m) the service is marked `gave_up` on that node, takod stops acting on it and raises an alert through the `add-takod-health-monitoring` alert API. Healing resumes after a new revision is deployed or `tako heal reset`. A node-wide circuit breaker pauses all healing when too many services fail at once (suspected node-level problem).
- **Strict eligibility.** takod skips an environment or service while a deploy/rollback/promote/scale/maintenance/preview-teardown lease is held, while a controller worker fence or node upgrade is active, while a restore-on-failure retention or backup/restore transaction is pending, while a maintenance companion (`<service>-maintenance`) runs, during the post-deploy grace period, and for `kind: job`/`kind: run` services, scaled-to-zero or `tako stop`ped services, removal-pending services, inactive blue-green (warming) containers, retained and retain-failed containers, and containers takod did not record as converged. Persistent services are healed only when the service enables it explicitly.
- **Config.** New `selfHealing` block at environment level (defaults for its services) and per service (`enabled`, `restartUnhealthy`, `startExited`, `unhealthyThreshold`, `maxRestarts`, `window`). The effective per-service policy is written into desired state at deploy, so it takes effect with the next deploy, scale, start or stop.
- **Coordination with deploys.** Each heal action takes the environment's operation lease on that node for its duration (seconds, operation `self-heal`, short TTL). The CLI waits up to 60s and retries when it meets a lease held by `self-heal`, instead of failing with exit 3.
- **Audit trail and CLI.** Every action and decision is appended to a node-local heal journal and to the environment's state event log (`heal.*` event types). New `tako heal status|pause|resume|reset` commands; `tako ps` gains an additive per-service `heal` field and column note.
- **Machine interface.** New result documents `HealStatus` and `HealChangeResult`, new state event types, additive `heal` field on `ps` rows. No existing field changes meaning; exit codes unchanged.
- **Capability gating.** takod advertises `heal.local-v1`. Deploying a config that enables self-healing to a node without it fails closed before any mutation with the standard upgrade message; `tako heal` commands require it.

No **BREAKING** changes: self-healing is off unless configured.

## Capabilities

### New Capabilities
- `self-healing`: node-local, opt-in automatic restart of unhealthy containers and restart of exited containers Docker will not restart, with eligibility rules, backoff, give-up, audit trail and CLI.

### Modified Capabilities
- None. Lease behavior gains a bounded retry against `self-heal` holders, specified inside `self-healing`.

## Impact

- **Config:** `pkg/config/types.go` (`SelfHealingConfig` on `EnvironmentConfig` and `ServiceConfig`), `pkg/config/validator.go`, `pkg/config/environment_extends.go` (inheritance is the normal merge), `schema/tako.schema.json`.
- **Desired state:** `pkg/takodstate/state.go` (`DesiredService.SelfHealing`, `sanitizeDesiredService` at `:806`), `pkg/takoapi/state.go` (`DesiredServiceDocument`).
- **takod:** new `pkg/takod/heal.go` (loop, eligibility, actions), `pkg/takod/heal_record.go` (converged records, journal, pause/gave-up state); hooks in `reconcile.go` (`ReconcileService` success writes the converged record), `restore.go` (finalize/restore update it), `cleanup.go` and `RemoveService` (clear it); `server.go` (start loop, capability, `/v1/heal` route), `lifecycle_gate.go` (route registration; `PUT /v1/heal` is a lifecycle-safe control-plane path like `/v1/alerts`).
- **Events:** `pkg/takoapi/events/events.go` (`heal.*` types).
- **CLI/engine:** new `cmd/heal.go`, `pkg/engine/heal.go`, `pkg/takodclient` heal client; `cmd/ps.go` additive field; deploy capability gate (`RequireTakodCapabilityContext`, `pkg/engine/takod.go:238`); lease acquisition retry for `self-heal` holders (`pkg/engine` lease helpers).
- **Docs:** new `docs/SELF-HEALING.md`; `docs/CONFIGURATION.md`, `docs/MACHINE-INTERFACE.md`, `docs/ORCHESTRATION-MODEL.md` (what self-healing is not), `docs/RESOURCE-MONITORING.md` cross-link.
- **Depends on:** `add-takod-health-monitoring` for per-container check observations (HTTP/TCP path checks) and the alert-source API. Without it, only Docker `--health-cmd` health is available and give-up alerts cannot be delivered; implementation order is health monitoring first.
- **Independent of:** `add-disk-hygiene-defaults`, `add-server-status-history`.
