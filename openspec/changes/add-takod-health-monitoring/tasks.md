# Tasks

## 1. Alert core and source API (takod)

- [ ] 1.1 Extract the episode, delivery, credential-generation, retry and SSRF-client logic of `pkg/takod/alerts.go` into an internal `alertCore`. Keep `AlertManager` (jobs and backups) on top of it with an unchanged persisted format. Verify: the existing `alerts_test.go` passes unchanged, and a new golden test pins the `scheduler-alerts` state and config documents byte-for-byte.
- [ ] 1.2 Add `pkg/takod/alert_sources.go` with `AlertSourceSpec`, `LevelObservation`, `EventObservation`, `Damping`, and the `SourceAlertManager` methods `RegisterSource`, `ReportLevel`, `ReportEvent`, `Subscribed` and `Status` (design D1). Storage goes under `<dataDir>/alerts/{config,credentials,state}` with its own schema version and strict loader. Verify with tests: unknown kind, reason or detail key rejected; an unsubscribed scope dropped; detail value pattern enforced; load and round-trip persisted.
- [ ] 1.3 Implement damping (`OpenAfter`, `OpenFor`, `RecoverAfter`), severity and `escalation`, event cooldown with `repeats`, staleness, and flap control (6 transitions per hour, one `suppressed` delivery, release after 30 minutes stable) (D2). Verify with table tests on a fake clock for each transition, including recovery fan-out only to providers that received the opening.
- [ ] 1.4 Add the payload schema v1 builder used by both managers (D3). Add `job_alert` and `backup_stale` constants to `pkg/notification`. Verify: a golden test for one job payload (old keys unchanged plus new keys), one service payload and one event payload; Slack and Discord bodies still carry only `text`/`content`; a test that no payload field contains a seeded error string.
- [ ] 1.5 Add the node-global host fan-out and the target-fingerprint dedupe ledger (D6). Verify: two scopes with the same webhook produce one delivery; different webhooks produce two; the ledger survives a restart.

## 2. Policy delivery

- [ ] 2.1 Extend `pkg/config` types: `notifications.alerts.{services,host,containers,certificates}` and `monitoring.{timeout,alertAfter,recoverAfter}`. Validator: bounds, `monitoring.webhook` required only without notification targets (relaxes `validator.go:1126-1128`), every target validated by `alerttarget.NormalizeAndValidate`, at most 200 monitors per scope. Update `schema/tako.schema.json`. Verify with validator table tests, including a private webhook rejected with the field path.
- [ ] 2.2 Extend `takod.AlertConfigRequest` with `Monitors`, `Host`, `Containers`, `Certificates` and `Targets.Services`. Update `normalizeAlertConfigRequest`, `validateAlertConfigRequest` and `alertPolicyEnabled`, and route the new sections to `SourceAlertManager` in `handleAlerts`. Make sure the zero policy removes both trees. Verify with handler tests: an old-shape request is unchanged; a new-sections-only request is enabled; zero removes both; targets never appear in the GET output.
- [ ] 2.3 Update `lifecycle_gate.go` to validate the extended request for enrolled nodes. Verify with the existing fence tests plus one new-section request.
- [ ] 2.4 `pkg/deployer/alerts.go`: build the extended request, assign the http `CheckerNode` deterministically (D4), and extend `PreflightAlertPolicy` to require `monitoring.health-v1` when new sections are present (D11). Verify: checker assignment is stable across server order; preflight fails with exit 2 naming the server, with no mutation; a jobs-only config on an old node still passes.
- [ ] 2.5 Add `CapabilityMonitoringHealthV1 = "monitoring.health-v1"` to takod capabilities and to the capability list tests.
- [ ] 2.6 `cleanup.go`: remove the `alerts/` tree for a destroyed environment or preview, alongside the existing alert removal (`server.go:1359,1388`). Verify with a cleanup test.

## 3. Sources

- [ ] 3.1 `monitor_checks.go`: per-scope scheduler with jitter and 4-way concurrency. The http check dials `127.0.0.1:443` with SNI and Host set to the domain, reads at most 64 KiB, follows no redirects, treats 2xx as passing (and 401 as passing with basic auth), and classifies `errorClass` (D5). Verify against an `httptest` TLS server with a fake dialer: 200, 500, timeout, TLS mismatch, redirect, basic auth.
- [ ] 3.2 Container check from the local desired revision assignments and `GatherActualState`, with reasons `replicas_missing` and `unhealthy`. Verify with fake docker output: missing replica, unhealthy container, a global placement, and no local assignment (no report).
- [ ] 3.3 Pauses: lease held, post-lease grace `max(startPeriod, 30s)`, maintenance proxy config present, `RemovalPending`. Verify that a simulated deploy outage under the lease produces no opening.
- [ ] 3.4 `host_alerts.go`: parse `current.json` (string percents), the thresholds and `cpuFor`, `load/nproc`, and `metrics_stale` after 5 minutes; skip other reasons while stale. Verify with fixture files for each reason, a malformed file and a stale file.
- [ ] 3.5 `container_alerts.go`: batched `docker inspect` sampling, a `container_oom` event with cooldown, `crash_loop` level detection and recovery, and the read-only `ContainerObservations()` accessor for `add-local-self-healing` (D7). Verify with fake inspect sequences: an OOM restart, repeated OOM within the cooldown, a restart loop, and a restarting status for 3 samples.
- [ ] 3.6 `cert_expiry.go`: takod store plus a symlink-safe walk of the Caddy store with a 1 MiB cap, later `NotAfter` wins, ownership through route manifests, warning, escalation to critical, expired and renewed (D8). Verify with generated certificates in a temp store: each threshold, a duplicate across stores, an unowned domain ignored, a symlink not followed.
- [ ] 3.7 Start the source loops from `Server.Run` after `waitForSchedulerStartup`, and stop them with `runCtx`. Verify with the server start and stop test.

## 4. Status, CLI and metrics

- [ ] 4.1 Extend the `GET /v1/alerts` status and `/v1/metrics` `alerts[]` with the additive `sources` object (D10). Verify with a JSON golden test and a no-targets assertion.
- [ ] 4.2 Rewrite `cmd/monitor.go`: parallel status read with a 10s timeout per node, table output, `--service`, `--watch`, hidden deprecated `--once`, exit codes 0/2/4/6, and `MonitorResult` for `--output json` (D9). Move `monitor` to the "Full contract" row and update `cmd/machine_coverage_test.go`. Verify with command tests using fake node clients: all ok, one alerting (exit 6), one node unreachable (exit 6), all unreachable (exit 4), no capability (exit 2), and `--watch` with JSON (exit 2).
- [ ] 4.3 `--local`: fix `pkg/monitoring/monitor.go` to honor per-service intervals, use real server names (`external` for public checks), add damping with opening and recovery, use payload v1, and deliver through `alerttarget` validation and the SSRF-safe transport. Verify: an interval test with a fake clock, an opening and recovery sequence, a private target refused, and no hardcoded `"prod"`.
- [ ] 4.4 `cmd/prometheus.go`: the `tako_monitor_*` and `tako_host_metrics_stale` series. Verify with exposition golden tests.

## 5. Docs and generated files

- [ ] 5.1 `docs/RESOURCE-MONITORING.md`: remove the network and disk I/O claims, replace the hand-written alert script with `notifications.alerts.host`, describe service checks, container, certificate and host alerts and `tako monitor`, and remove "Alerting rules and notifications" from Planned. `docs/CONFIGURATION.md`: the new config keys with snippet markers. `docs/MACHINE-INTERFACE.md`: `MonitorResult`, the payload schema v1, the `sources` status fields, the coverage table and exit codes.
- [ ] 5.2 Regenerate the man pages and completions for `tako monitor`. Run the docs snippet harness.

## 6. Verification

- [ ] 6.1 `go build ./...`, `go vet ./...` and gofmt are clean on Linux. Full `go test ./...` shows no new failures against the master baseline.
- [ ] 6.2 Run `openspec validate add-takod-health-monitoring --strict`.
- [ ] 6.3 Manual check on a live server:
  - Deploy with `web.monitoring` and default host alerts. Stop the `web` container by hand and confirm one opening, then one recovery after a redeploy.
  - Fill the disk with `fallocate` past 90% and confirm a `high_disk` alert.
  - Run `tako monitor` and `tako monitor --output json`.
  - Downgrade takod and confirm it starts with job alerts intact.
