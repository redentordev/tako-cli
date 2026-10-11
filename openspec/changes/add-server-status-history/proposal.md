# Proposal

## Why

Tako can show what a server looks like right now, but not what it looked like an hour ago, and it says nothing about the operating system underneath:

- **No history.** The bash agent `tako-monitor.sh` (`pkg/provisioner/provisioner.go:1678-1797`) overwrites `/var/lib/tako/metrics/current.json` every 60s. "Was the disk filling up overnight?" or "did CPU spike during the deploy?" cannot be answered. `docs/RESOURCE-MONITORING.md:384` lists "Metrics retention and historical queries" as planned.
- **The docs over-promise.** `docs/RESOURCE-MONITORING.md` (lines 11, 36, 46-47, 92, 134-135, 323-324) documents network and disk I/O and `tako_network_*` counters, but the installed script collects neither. The CLI already has `Network` and `DiskIO` fields (`cmd/metrics.go:35-36`) that are always empty.
- **Dead code.** `pkg/monitoring/agent.sh`, `agent_v2.sh` and `tako-monitor.service` are not embedded or referenced by any Go code. The real agent is an inline string in the provisioner and needs `bc` installed (`provisioner.go:1818-1822`).
- **No OS status.** `tako setup` turns on unattended-upgrades (apt only, no automatic reboot; `provisioner.go:1531-1541`). Nothing reports whether updates are pending, whether security updates are stuck, or whether the kernel is waiting for a reboot.
- **Doctor only runs when you run it.** Node checks in `tako doctor` (`cmd/doctor.go:394-517`) are CLI-side SSH probes. A node that loses clock sync, or whose metrics collector stops, goes unnoticed until someone looks.

## What Changes

- **Native collector in takod.** takod collects host metrics in Go every 60s: CPU, memory/swap, root and Docker data-root filesystem (bytes and inodes), load, uptime, network I/O and disk I/O. It needs no bash script and no `bc`.
  - `GET /v1/metrics` keeps its document shape. The `network` and `disk_io` fields are now filled in, and new fields are additive.
  - `collect=true` takes a fresh sample instead of running the script.
  - It is on by default through takod's own flag default, so `tako upgrade servers` (binary-only) enables it on existing nodes. `--metrics-interval 0` turns it off and falls back to reading the legacy agent's file.
- **Bounded history on each node.** Samples are appended to hourly JSON-lines files under `/var/lib/tako/metrics/history/`. Retention defaults to 7 days, with a hard size cap of 64 MiB. A new read-only endpoint `GET /v1/metrics/history?since=…&step=…` returns min/avg/max buckets, gaps and boot times.
- **`tako metrics --since 24h [--step 5m]`.** Shows a per-node history summary (min/avg/max/last plus sparklines) and returns a new `MetricsHistoryResult` document in machine modes. Nodes are queried in parallel and reported per node; nothing is merged across nodes.
- **OS status, reporting only.** takod periodically records:
  - pending package updates and how many are security updates, using cached package lists only (`apt-get -s` or `dnf --cacheonly`); it never runs `apt-get update`;
  - the package-list age;
  - reboot-required, from `/var/run/reboot-required` or `needs-restarting -r`;
  - clock sync, from `timedatectl`.

  These are surfaced in `tako metrics`, `tako doctor` and `tako prometheus`. Tako never installs updates and never reboots.
- **Node conditions.** takod evaluates a fixed set of read-only node checks on a schedule: `os-updates`, `reboot-required`, `clock-sync`, `docker-daemon` and `metrics-collector`. Each produces a condition (`ok`, `warn`, `fail` or `unknown`) exposed at `GET /v1/node/status`. Four of them are published through the alert-source API of `add-takod-health-monitoring` as kind `node` (`reboot_required` after 24h, `security_updates_pending` after 72h, `clock_unsynchronized`, `docker_unreachable`), with windows measured from the persisted `since`. A stalled collector is alerted only by monitoring's `metrics_stale`. This change does not define alert targets, routing or host thresholds.
- **Native samples for monitoring.** The collector implements monitoring's host-sample interface, so host threshold alerts (including inodes and a separate Docker data root) read native samples instead of the legacy `current.json`.
- **`tako doctor`** gains checks backed by `/v1/node/status`: OS updates, reboot required, clock sync, collector freshness, and a note when the legacy agent is still installed. On nodes without the capability these checks are reported as `skip` with an upgrade hint.
- **Prometheus.** `tako prometheus` gains node-condition, OS-status and real network/disk I/O series. takod also gains an opt-in `--metrics-textfile-dir` that writes a `.prom` file atomically for node_exporter's textfile collector. takod does **not** open a network listener; it stays socket-only.
- **Setup and cleanup.**
  - `tako setup` no longer installs the bash agent and removes an existing `tako-monitor` service and script. The step keeps its `SetupResult` key `monitor-agent`, moves after `takod-service` (it needs takod's capability), and runs in every setup mode.
  - The dead files in `pkg/monitoring/` are deleted.
  - `docs/RESOURCE-MONITORING.md` is rewritten to describe what is actually collected.
- **Capabilities:** `node.metrics-history-v1` covers the history endpoint and the native collector. `node.status-v1` covers `/v1/node/status`. When a node lacks a capability, the new CLI features fail closed per node with a `tako upgrade servers` hint.
- No **BREAKING** changes. `/v1/metrics` and `MetricsResult` only gain fields, `tako metrics` without `--since` behaves as today, and exit codes keep their meaning.

## Out of Scope

- Automatic OS updates beyond what `tako setup` already configures, automatic reboots, and a `tako server reboot` command (design D8).
- Alert thresholds for CPU, memory, disk and inodes, alert targets, routing and delivery. These are owned by `add-takod-health-monitoring`, which consumes this change's samples and conditions.
- Container metrics history. `tako stats` stays point-in-time.
- Disk cleanup actions. These are owned by `add-disk-hygiene-defaults`, which may read the Docker data-root usage this change collects.
- Restarting or recreating containers. That is owned by `add-local-self-healing`.
- A takod TCP or HTTP listener, or remote scraping.
- Cross-node aggregation, such as cluster-wide averages.

## Capabilities

### New Capabilities
- `node-status`: native host metrics collection, bounded on-node history and its query API, `tako metrics --since`, OS update and reboot-required reporting, scheduled read-only node conditions, doctor and Prometheus integration, and capability gating.

### Modified Capabilities
- `server-setup`: setup stops installing the legacy bash monitoring agent and removes it when present.
- `documentation-accuracy`: the resource-monitoring docs describe only the metrics that are actually collected.

## Impact

- **takod:**
  - New `pkg/takod/hostmetrics/`: the `/proc` and `statfs` readers and the sample type.
  - New `pkg/takod/metrics_history.go`: the append-only store, pruning and query/downsampling.
  - New `pkg/takod/node_status.go`: the OS probe, the conditions and their schedule.
  - `pkg/takod/metrics.go`: native read path with legacy fallback.
  - `pkg/takod/server.go`: collector and status loops, new flags, and capabilities.
  - `pkg/takod/lifecycle_gate.go`: route registry.
  - takod `run` flag wiring.
- **CLI:**
  - `cmd/metrics.go`: `--since`, `--step`, and the OS line.
  - `cmd/doctor.go`: node-status checks.
  - `cmd/prometheus.go`: new series.
  - `pkg/engine/metrics.go`: the `MetricsHistoryResult` and additive `status` on `MetricsNodeSample`.
  - `pkg/takodclient`: endpoints and capability constants.
- **Provisioner:** `InstallMonitoringAgent` is replaced by legacy-agent removal, `bc` is no longer installed, `setup.CurrentVersion` is bumped, and `cmd/setup.go` moves the `monitor-agent` step after `takod-service` without the converge skip.
- **Dependencies:** lands after `add-takod-health-monitoring` (alert-source API, `HostSampleSource`; contract in its design D13) and registers kind `node` in the same change. Independent of `add-disk-hygiene-defaults`; lands before `add-local-self-healing`. Owns the metric claims in `docs/RESOURCE-MONITORING.md`.
- **Deleted:** `pkg/monitoring/agent.sh`, `agent_v2.sh` and `tako-monitor.service`, after a grep confirms they are unreferenced.
- **Docs:** `docs/RESOURCE-MONITORING.md` (rewrite), `docs/MACHINE-INTERFACE.md` (the history result, node status, capabilities), and the takod flags reference in `docs/CONFIGURATION.md`.
- **Tests:**
  - collector parsers with fixture `/proc` files;
  - the history store (append, prune, size cap, torn last line, clock jumps, downsampling);
  - OS probe parsers (apt and dnf fixtures);
  - condition evaluation;
  - the CLI history rendering and JSON golden;
  - doctor checks with fake status;
  - capability fallbacks;
  - setup legacy removal.
