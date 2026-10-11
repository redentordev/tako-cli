# Tasks

## 1. Native collector (takod)

- [x] 1.1 Add `pkg/takod/hostmetrics` with pure parsers for `/proc/stat`, `/proc/meminfo`, `/proc/loadavg`, `/proc/uptime`, `/proc/net/dev` (excluding `lo`, `veth*`, `docker*`, `br-*` and the mesh interface) and `/proc/diskstats` (whole devices from `/sys/block`, excluding `loop*`, `ram*`, `zram*` and `dm-*`), plus `statfs` usage for `/` and the Docker data root, deduplicated by filesystem ID (design D1). The proc and sys roots are injectable for tests. Verify with fixture-file tests for each parser, including counter wrap and reset, which must produce no negative deltas.
- [x] 1.2 Add the `Sample` type and a converter to the legacy `/v1/metrics` schema: string percentages, `network` and `disk_io` deltas, and the additive fields from D2. Verify with a golden test that the legacy fields keep their names and types, and that the output decodes into `cmd.MetricsData` unchanged.
- [x] 1.3 Add a collector goroutine started from `Server.Run` when `--metrics-interval` > 0 (default `60s`), with `Latest()` in-process access and `CollectNow(ctx)` (a 1s CPU window, not appended to history). Wire the flags `--metrics-interval`, `--metrics-retention`, `--metrics-history-max-bytes` and `--metrics-textfile-dir` into `takod run`. Verify with a fake clock and a fake source: one sample per tick, `Latest` updates, and `CollectNow` respects context cancellation.
- [x] 1.4 Switch `ReadNodeMetrics`/`handleMetrics` to the native sample when enabled and keep the legacy file and script path when the interval is 0. Advertise `node.metrics-history-v1` only when native collection is enabled. Verify with handler tests for both modes, the `collect=true` path, and capability advertisement.

## 2. History store and query (takod)

- [x] 2.1 Add `pkg/takod/metrics_history.go`: hourly UTC JSON-lines segments, single-write `O_APPEND` appends, 0750 directories and 0640 files, pruning at startup and on hour rollover by retention and byte cap, oldest first (D3). Write failures set a collector warning and never panic. Verify with tests for append and reopen, a torn last line, retention pruning, the size cap, and a write failure (read-only directory).
- [x] 2.2 Implement the query: read only overlapping segments, sort by `t`, drop exact duplicates, choose the step from the nice-step list (at most 360 buckets), reject a step below the interval or more than 1440 buckets, clamp to retention with `truncated`, compute min/avg/max and rate buckets, gaps (more than 3× interval) and boots (uptime decreases) (D4). Verify with table tests: 24h gives 5m/288, explicit steps, a gap, a boot, a backward clock jump with overlap, and an empty range.
- [x] 2.3 Add the `GET /v1/metrics/history` route to `registeredRoutes` (read-only GET). Verify that the lifecycle-gate route-registry test covers it, and that it works on a cordoned enrolled node.

## 3. Node status (takod)

- [x] 3.1 Add the OS probe with injectable command execution and filesystem access: apt simulation parsing (with security-origin detection), list age, `reboot-required` plus `.pkgs`, dnf/yum `--cacheonly check-update` (exit 100) and `updateinfo --security`, `needs-restarting -r`, all with `nice`/`ionice` and a 120s timeout (D5). Verify with fixture outputs for Ubuntu, Debian, Fedora/Rocky, a missing tool, a timeout, and a parse failure that keeps the previous result as stale. Add a test asserting that the probe never invokes `update`, `makecache`, `install` or `reboot`.
- [x] 3.2 Add the condition evaluator and loop with per-probe schedules and jitter (`os-updates` 6h, first run 2–5 min after start; `reboot-required` and `clock-sync` 15m; `docker-daemon` 1m; `metrics-collector` on each tick), the `since` transitions, the two-strike `clock-sync` rule, and persistence to `<data-dir>/node-status.json`. Verify with a fake clock: transitions, preserved `since`, restart persistence, and the stale marking.
- [x] 3.3 Add `GET /v1/node/status` and the additive `status` in `/v1/metrics`, and advertise `node.status-v1`. Verify with handler tests, JSON goldens and a capability-list test.
- [x] 3.4 Alert-source wiring (`add-takod-health-monitoring` has landed first): register kind `node` (node-scoped; subjects are condition types; reasons and events per D6 and monitoring D13), report levels only after the D6 windows measured from the persisted `since`, report recoveries on `ok`, and report nothing for `unknown`/stale. Do not publish `metrics-collector`. Verify with alert tests on a fake clock: reboot opens after 24h; `os-updates` opens after 72h including across a restart at 70h; clock opens on `fail`; docker opens after 3m of `fail` and not after a 1-minute blip; recovery on `ok`; `unknown` neither opens nor recovers; no `metrics-collector` alert.
- [x] 3.5 Implement monitoring's `HostSampleSource` over the collector's latest sample (bytes and inodes for `/` and a separate Docker root, memory, CPU, load, sample time) and register it so monitoring prefers it whenever native collection is enabled. Verify: monitoring reads the native source with collection on and the legacy adapter with `--metrics-interval 0`; `metrics_stale` follows the native sample time.

## 4. Prometheus

- [x] 4.1 Add the new series in `cmd/prometheus.go` (D7) from the native sample and node status. Verify with exposition golden tests, including the absence of the new series for legacy-mode nodes.
- [x] 4.2 Add the textfile writer behind `--metrics-textfile-dir`: atomic temp file plus rename after each collection, node-local series only. Verify with a test for atomic replacement, and a test that takod opens no TCP listener with any metrics flag combination.

## 5. CLI

- [x] 5.1 Add `tako metrics --since/--step`: flag validation (exclusive with `--live`/`--once`, exit 2), parallel history reads with per-node capability checks, human rendering (rows, 48-character sparklines, gaps, boots), and the `MetricsHistoryResult` in `pkg/engine/metrics.go`. Verify with rendering goldens, a JSON golden, a mixed-version partial read (exit 6) and an all-failed read (exit 1).
- [x] 5.2 Add the OS line and network/disk I/O lines to point-in-time `tako metrics`, plus `MetricsNodeSample.status`. Update the "monitoring agent not installed" error text to reflect takod-native collection. Verify with goldens with and without status.
- [x] 5.3 Add the doctor checks from `/v1/node/status` (OS security updates, reboot required, clock synchronized, metrics collector, legacy agent) with `skip` on a missing capability and `--skip-remote` handling. Verify with fake-status table tests and the `DoctorResult` JSON golden.

## 6. Setup migration and cleanup

- [x] 6.1 Replace `InstallMonitoringAgent` with `RemoveLegacyMonitoringAgent`, guarded on takod advertising `node.metrics-history-v1`. Stop installing `bc` for monitoring. In `cmd/setup.go`, keep the step key `engine.SetupStepMonitorAgent` (`monitor-agent`), retitle it "Removing legacy monitoring agent", move it after `takod-service`, and drop its converge skip; bump `setup.CurrentVersion`. Verify with provisioner tests using a fake executor (removal commands, idempotent second run, guard-not-met leaves the agent in place and completes with a message) and a `SetupResult` JSON test showing `monitor-agent` after `takod-service` in fresh, reapply and converge modes.
- [x] 6.2 Confirm with `git grep` and a `go:embed` search that `pkg/monitoring/agent.sh`, `agent_v2.sh` and `tako-monitor.service` are unreferenced, then delete them.

## 7. Docs and verification

- [x] 7.1 Rewrite `docs/RESOURCE-MONITORING.md` per the `documentation-accuracy` delta. Update `docs/MACHINE-INTERFACE.md` (`MetricsHistoryResult`, `MetricsNodeSample.status`, `/v1/metrics/history`, `/v1/node/status`, the capabilities, exit codes) and the takod flag reference in `docs/CONFIGURATION.md`. Run the docs snippet tests.
- [x] 7.2 Run `go build ./...`, `go vet ./...` and `go test ./...` on Linux, with no new failures against master, and check that gofmt is clean. Run `openspec validate add-server-status-history --strict`.
- [ ] 7.3 Manual check on a live server, recorded in the PR (not performed here; listed in the PR for a reviewer with server access):
  - upgrade an existing node and confirm native samples and history accumulate;
  - `tako metrics --since 1h`;
  - `tako doctor` shows OS status;
  - re-run `tako setup` and confirm it removes the legacy agent;
  - touch `/var/run/reboot-required` and confirm the condition and the doctor `warn`.
