# Tasks

## 1. Config and desired state

- [ ] 1.1 Add `SelfHealingConfig` (`enabled`, `restartUnhealthy`, `startExited`, `unhealthyThreshold`, `maxRestarts`, `window`) to `EnvironmentConfig` and `ServiceConfig` (`pkg/config/types.go`); verify strict decode rejects unknown keys in YAML and JSON
- [ ] 1.2 Validation in `pkg/config/validator.go`: ranges (threshold 1-20, maxRestarts 1-20, window 5m-24h), reject on `kind: job`/`kind: run`; table tests
- [ ] 1.3 Implement `EffectiveSelfHealing(env, service)` (D1: service → environment → defaults; environment `enabled` ignored for persistent services); table tests including persistent opt-in and `extends` inheritance
- [ ] 1.4 Add `SelfHealing` to `takodstate.DesiredService` and `takoapi.DesiredServiceDocument`, filled by `sanitizeDesiredService` (`pkg/takodstate/state.go:806`) and omitted when disabled; verify round trip and omission
- [ ] 1.5 Add `selfHealing` to `schema/tako.schema.json` (environment and service) with schema tests

## 2. takod: records, journal, control state

- [ ] 2.1 New `pkg/takod/heal_record.go`: converged record read/write (`<dataDir>/heal/<project>/<env>/services/<service>.json`, atomic, 0600), `converging` marker, clear; unit tests
- [ ] 2.2 Write `converging` at the start of `ReconcileService` and the final record at its success return (`reconcile.go:163-271`); write records on restore-on-failure finalize and restore (`restore.go`); clear on `RemoveService`, zero-container reconcile, environment/project cleanup (`cleanup.go`) and preview teardown. Tests with a fake docker runner for success, failure (record stays converging), retain-failed, finalize, restore, scale to zero
- [ ] 2.3 Heal journal (`journal.jsonl`, capped 1000 lines, redacted reasons) and control state (`control.json`: pauses with expiry, gave-up, counters, cooldowns, circuit breaker); persistence across takod restart test

## 3. takod: heal loop

- [ ] 3.1 New `pkg/takod/heal.go`: loop every 15s started from `Server.Run` (`server.go:424-436`) only when at least one desired service on the node has a policy; context-cancel clean shutdown
- [ ] 3.2 Eligibility gate (D4) as a pure function over a snapshot (lease file, worker fence, lifecycle/deny latch, restore journals, retained state, backup lock, maintenance companion, desired assignments, record state, pause, gave-up) with a table test per skip reason
- [ ] 3.3 Container classification (D3) from `docker inspect` (`State.Status`, `State.Restarting`, `State.StartedAt`, `HostConfig.RestartPolicy`, labels, health) plus health observations from `add-takod-health-monitoring` (stale → unknown); tests for each row of the D3 table
- [ ] 3.4 Action executor: acquire lease (`AcquireLease`, operation `self-heal`, TTL 5m) under the maintenance barrier and mutation lock, re-check the gate, one `docker restart`/`docker start` with a 3m timeout, release; tests that a held foreign lease, a gate change under the lease, and a docker error each lead to no action or a recorded failure
- [ ] 3.5 Backoff, give-up, serial execution and circuit breaker (D5); tests with a fake clock for cooldown doubling, reset after a healthy window, give-up at `maxRestarts`, give-up cleared by a new revision, breaker trip on mass failure and on action rate
- [ ] 3.6 Events and alerts: register `heal.*` types in `pkg/takoapi/events/events.go`; append via `AppendStateEvent`; raise `gave_up`/`circuit_open`/`failed` (and `info` restarts) through the health-monitoring alert-source API; tests
- [ ] 3.7 Routes: `GET /v1/heal` (status) and `PUT /v1/heal` (pause/resume/reset) in `registeredRoutes` (`lifecycle_gate.go:25-41`); add `PUT /v1/heal` to `lifecycleSafeMutationPaths` with fence validation like `/v1/alerts`; lifecycle-gate route test
- [ ] 3.8 Advertise `CapabilityLocalSelfHealingV1 = "heal.local-v1"` (`server.go:2714`); update capability-list tests

## 4. CLI and engine

- [ ] 4.1 Capability gate in every desired-state-writing command when any service policy is enabled (`RequireTakodCapabilityContext`, `pkg/engine/takod.go:238`); test that deploy fails with exit 2 before mutation against an old agent
- [ ] 4.2 Lease acquisition: when the holder's operation is `self-heal`, wait and retry up to 60s before `LockedError`; tests for self-heal holder released in time, held too long, and a non-heal holder unchanged
- [ ] 4.3 `pkg/takodclient` heal client and `pkg/engine/heal.go` (status fan-out to target nodes, pause/resume/reset)
- [ ] 4.4 `cmd/heal.go`: `status`, `pause` (`--service`, `--for`, `--until-resume`, `--server`), `resume`, `reset`; text and `--output json` (`HealStatus`, `HealChangeResult`); golden tests
- [ ] 4.5 `tako ps`: additive `heal` object and `heal:gave_up`/`heal:paused` status note (`cmd/ps.go:182`); tests with and without the capability
- [ ] 4.6 Regenerate man pages and completions

## 5. Docs

- [ ] 5.1 New `docs/SELF-HEALING.md`: what it does and does not do, eligibility, defaults, persistent services, pausing during incidents, reading `tako heal status`, interaction with `restart` policies
- [ ] 5.2 `docs/CONFIGURATION.md` (`selfHealing` keys), `docs/MACHINE-INTERFACE.md` (`HealStatus`, `HealChangeResult`, `heal.*` events, `ps.heal`), `docs/ORCHESTRATION-MODEL.md` (node-local only, no failover), cross-link from `docs/RESOURCE-MONITORING.md`

## 6. Verification

- [ ] 6.1 `go build ./...`, `go vet ./...`, full `go test ./...` on Linux with no new failures against master; gofmt clean
- [ ] 6.2 `openspec validate add-local-self-healing --strict`
- [ ] 6.3 Manual on a test server: hung HTTP service restarted once; crash-looping service gives up and alerts; deploy during a restart waits and succeeds; `docker stop` on an `unless-stopped` container is reported, not undone; `tako heal pause` stops all actions
