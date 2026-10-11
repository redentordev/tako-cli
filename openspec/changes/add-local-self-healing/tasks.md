# Tasks

Prerequisite: `add-takod-health-monitoring` has landed (shared container sampler `ContainerObservations()`, alert-source API with kind `selfheal` reserved in its D13), and `add-disk-hygiene-defaults` has landed (its reclaim honours converged records). This change lands last.

## 1. Config and desired state

- [x] 1.1 Add `SelfHealingConfig` (`enabled`, `restartUnhealthy`, `startExited`, `unhealthyThreshold`, `maxRestarts`, `window`) to `EnvironmentConfig` and `ServiceConfig` (`pkg/config/types.go`); verify strict decode rejects unknown keys in YAML and JSON
- [x] 1.2 Validation in `pkg/config/validator.go`: ranges (threshold 1-20, maxRestarts 1-20, window 5m-24h), reject on `kind: job`/`kind: run`; table tests
- [x] 1.3 Implement `EffectiveSelfHealing(env, service)` (D1: service → environment → defaults; environment `enabled` ignored for persistent services); table tests including persistent opt-in and `extends` inheritance
- [x] 1.4 Add `SelfHealing` to `takodstate.DesiredService` and `takoapi.DesiredServiceDocument`, filled by `sanitizeDesiredService` (`pkg/takodstate/state.go:806`) and omitted when disabled; verify round trip and omission
- [x] 1.5 Keep `selfHealing` out of `reconcile.SafeServiceConfigHash` and `deployplan.ServiceRevisionID`; add a test that toggling and tuning the block leaves both unchanged, and a deployer test that a selfHealing-only change plans no container update
- [x] 1.6 Add `selfHealing` to `schema/tako.schema.json` (environment and service) with schema tests

## 2. takod: records, journal, control state

- [x] 2.1 New `pkg/takod/heal_record.go`: converged record `{image, revision, containers[{name,id}], restart, recordedAt}` read/write (`<dataDir>/heal/<project>/<env>/services/<service>.json`, atomic, 0600), `converging` marker, clear, and the validity rule (not converging, image equals desired image); unit tests including a stale record after a simulated older-agent deploy (image differs) and after a retention rename (name differs)
- [x] 2.2 Write `converging` at the start of `ReconcileService` and the final record at its success return (`reconcile.go:163-271`); write records on restore-on-failure finalize and restore (`restore.go`); clear on `RemoveService`, zero-container reconcile, environment/project cleanup (`cleanup.go`) and preview teardown. Tests with a fake docker runner for success, failure (record stays converging), retain-failed, finalize, restore, scale to zero
- [x] 2.3 Heal journal (`journal.jsonl`, capped 1000 lines, reason codes and numbers only) and control state (`control.json`: pauses with expiry, gave-up, counters, cooldowns, circuit breaker); persistence across takod restart test; a test that a health error containing log text never reaches the journal

## 3. takod: heal loop

- [x] 3.1 New `pkg/takod/heal.go`: loop every 15s started from `Server.Run` (`server.go:424-436`) only when at least one desired service on the node has a policy, reading container state from monitoring's `ContainerObservations()` (no own inspect loop); context-cancel clean shutdown
- [x] 3.2 Per-container health probe for `healthCheck.path`/TCP services with `restartUnhealthy`: build the `HealthSpec` from the desired `healthCheck` and `port` as the reconcile request does, call `checkContainerHTTPHealth`/`checkContainerTCPHealth` (no smoke check) every `healthCheck.interval` (default 30s, min 10s), at most 4 in parallel; map errors to reason codes. Tests with an `httptest` server per replica: one hung replica of two, timeout, connect refused, non-2xx
- [x] 3.3 Eligibility gate (D4) as a pure function over a snapshot (lease file, worker fence, lifecycle/deny latch, restore journals, retained state, backup lock, maintenance companion, desired assignments, record state and validity, pause, gave-up) with a table test per skip reason
- [x] 3.4 Container classification (D3) from the sampler observation (status, restarting, exit code, OOM, `StartedAt`, restart policy, labels, health) plus probe results (stale → unknown); tests for each row of the D3 table, including exit 0 under `no` → `completed`, exit 1 under `on-failure` → start, created → `stopped`
- [x] 3.5 Action executor: acquire lease (`AcquireLease`, operation `self-heal`, TTL 5m) under the maintenance barrier and mutation lock; on enrolled nodes take `workerFenceAdmissionMu.RLock` and re-check `activeWorkerOperation`; re-check the gate; one fresh `docker inspect` confirming condition, ID, name, revision label and restart policy; one `docker restart`/`docker start` with a 3m timeout; release. Tests that a held foreign lease, a gate change under the lease, an active worker fence, a changed inspect result and a docker error each lead to no action or a recorded failure; a test that fence activation blocks until the action returns
- [x] 3.6 Backoff, give-up, serial execution and circuit breaker (D5); tests with a fake clock for cooldown doubling, reset after a healthy window, give-up at `maxRestarts`, give-up cleared by a new revision, breaker trip on mass failure and on action rate
- [x] 3.7 Events and alerts: register `heal.*` types (including `heal.circuit_closed`) in `pkg/takoapi/events/events.go`; append via `AppendStateEvent`; report kind `selfheal` through the alert-source API: `gave_up` and `circuit_open` (`@node`) as critical levels with recovery, `action_failed` as a warning event; no delivery for successful actions. Tests for each, including recovery on reset and on a new revision, and no delivery after a successful restart
- [x] 3.8 Routes: `GET /v1/heal` (status) and `PUT /v1/heal` (pause/resume/reset) in `registeredRoutes` (`lifecycle_gate.go:25-41`); add `PUT /v1/heal` to `lifecycleSafeMutationPaths` with fence validation like `/v1/alerts`; lifecycle-gate route test
- [x] 3.9 Advertise `CapabilityNodeSelfHealingV1 = "node.self-healing-v1"` (`server.go:2714`); update capability-list tests

## 4. CLI and engine

- [x] 4.1 Capability gate in every desired-state-writing command when any service policy is enabled (`RequireTakodCapabilityContext`, `pkg/engine/takod.go:238`); test that deploy fails with exit 2 before mutation against an old agent, naming `node.self-healing-v1`
- [x] 4.2 Lease acquisition: when the holder's operation is `self-heal`, wait and retry up to 60s before `LockedError`; tests for self-heal holder released in time, held too long, and a non-heal holder unchanged
- [x] 4.3 `pkg/takodclient` heal client and `pkg/engine/heal.go` (status fan-out to target nodes, pause/resume/reset)
- [x] 4.4 `cmd/heal.go`: `status`, `pause` (`--service`, `--for`, `--until-resume`, `--server`), `resume`, `reset`; text and `--output json` (`HealStatus`, `HealChangeResult`); golden tests
- [x] 4.5 `tako ps`: additive `heal` object and `heal:gave_up`/`heal:paused` status note (`cmd/ps.go:182`); tests with and without the capability
- [x] 4.6 Regenerate man pages and completions

## 5. Docs

- [ ] 5.1 New `docs/SELF-HEALING.md`: what it does and does not do, eligibility, defaults, persistent services, exit 0 vs crash, pausing during incidents, reading `tako heal status`, interaction with `restart` policies, alerts (kind `selfheal`)
- [ ] 5.2 `docs/CONFIGURATION.md` (`selfHealing` keys), `docs/MACHINE-INTERFACE.md` (`HealStatus`, `HealChangeResult`, `heal.*` events, `ps.heal`, `selfheal` alert kind), `docs/ORCHESTRATION-MODEL.md` (node-local only, no failover), cross-link from `docs/RESOURCE-MONITORING.md`

## 6. Verification

- [ ] 6.1 `go build ./...`, `go vet ./...`, full `go test ./...` on Linux with no new failures against master; gofmt clean
- [ ] 6.2 `openspec validate add-local-self-healing --strict`
- [ ] 6.3 Manual on a test server: hung HTTP replica restarted once while its sibling is untouched; crash-looping service gives up and alerts; deploy during a restart waits and succeeds; `docker stop` on an `unless-stopped` container is reported, not undone; an exit-0 `restart: no` worker stays `completed`; `tako heal pause` stops all actions
