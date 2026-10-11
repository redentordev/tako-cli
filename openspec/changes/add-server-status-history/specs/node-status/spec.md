# Spec Delta

## Purpose

Every Tako node continuously records its own resource usage and operating-system health, keeps a short bounded history, and reports both without changing the OS. Operators can answer "what happened on this server recently" and "does this server need attention" from the CLI, machine output, doctor and Prometheus.

## ADDED Requirements

### Requirement: Native host metrics collection
takod SHALL collect host metrics in-process every `--metrics-interval`. The metrics are CPU, memory and swap, root and Docker data-root filesystem usage (bytes and inodes), load, uptime, network I/O and whole-device disk I/O.

#### Scenario: Docker data root on a separate filesystem
- **WHEN** `/var/lib/docker` is on a different filesystem from `/`
- **THEN** the sample contains `docker_disk` with its bytes and inode usage
- **AND** when both are on the same filesystem, `docker_disk` is absent

#### Scenario: Virtual interfaces excluded
- **WHEN** a node has `lo`, `docker0`, `veth*` and `br-*` interfaces plus `eth0`
- **THEN** network I/O counts only `eth0`

### Requirement: Native collection is on by default with a legacy fallback
Native collection SHALL be enabled by takod's flag default (`--metrics-interval 60s`), so that a binary-only upgrade enables it. With `--metrics-interval 0`, takod SHALL read the legacy agent file as before.

#### Scenario: Upgraded node collects without setup
- **WHEN** a node is upgraded with `tako upgrade servers` and its systemd unit was written by an older `tako setup`
- **THEN** takod collects host metrics natively every 60s
- **AND** the node advertises `node.metrics-history-v1`

#### Scenario: Native collection disabled
- **WHEN** takod runs with `--metrics-interval 0`
- **THEN** `GET /v1/metrics` returns the legacy agent's `current.json` as before
- **AND** the node does not advertise `node.metrics-history-v1`

### Requirement: Metrics document compatibility
`GET /v1/metrics` SHALL keep every existing field name and type of the `metrics` object. The fields `network` and `disk_io` SHALL be populated with deltas since the previous sample. New fields SHALL be additive only.

#### Scenario: Old CLI reads a native sample
- **WHEN** a CLI built before this change reads `/v1/metrics` from a node with native collection
- **THEN** it decodes `cpu_percent`, `memory`, `disk`, `uptime_seconds` and `load_average` without error

#### Scenario: Network and disk I/O are real
- **WHEN** `tako metrics` reads a node with native collection
- **THEN** the output includes network receive/transmit and disk read/write values for the last interval

#### Scenario: Immediate collection
- **WHEN** a client requests `/v1/metrics?collect=true`
- **THEN** takod takes a fresh sample (CPU over a 1-second window) without running any script
- **AND** that sample is not appended to history

### Requirement: Bounded on-node metrics history
takod SHALL append each collected sample to hourly JSON-lines files under `<data-dir>/metrics/history/`. Data older than `--metrics-retention` (default `168h`, maximum `744h`) SHALL be pruned. Total history size SHALL be capped at `--metrics-history-max-bytes` (default 64 MiB) by deleting the oldest files first. A failure to write history SHALL NOT stop collection or crash takod.

#### Scenario: Retention pruning
- **WHEN** history files older than the retention exist at an hour rollover
- **THEN** those files are deleted

#### Scenario: Size cap
- **WHEN** the history directory exceeds the byte cap
- **THEN** the oldest files are deleted until it is under the cap

#### Scenario: Torn last line after a crash
- **WHEN** takod stopped mid-write and the newest file ends with a partial line
- **THEN** history reads skip the partial line and return all complete samples

#### Scenario: Disk full
- **WHEN** appending a sample fails because the disk is full
- **THEN** the latest sample is still served from memory
- **AND** the `metrics-collector` condition becomes `warn` with reason `history-write-failed`

### Requirement: History query endpoint
takod SHALL serve `GET /v1/metrics/history` with `since` (a duration or RFC3339), an optional `until` and an optional `step`. The response SHALL contain min/avg/max buckets for CPU, memory, disk and load, rate buckets for network and disk I/O, the newest sample, gaps longer than three intervals, and boot times.

#### Scenario: Takod was down
- **WHEN** takod was stopped for 20 minutes within the requested range
- **THEN** the response lists a gap covering those 20 minutes and has no buckets for it

#### Scenario: Node rebooted
- **WHEN** the node rebooted within the range
- **THEN** the response lists the boot time in `boots`

### Requirement: History query bounds
When no `step` is given, the history endpoint SHALL choose the smallest standard step that yields at most 360 buckets. It SHALL reject a step below the collection interval, or one that yields more than 1440 buckets, with HTTP 400. A range longer than the retention SHALL be clamped and reported as truncated.

#### Scenario: Default step for 24 hours
- **WHEN** a client requests `since=24h` without a step
- **THEN** the response uses a step of `5m` and contains at most 288 buckets

#### Scenario: Step too small
- **WHEN** a client requests `step=10s` on a node collecting every 60s
- **THEN** takod responds with HTTP 400

#### Scenario: Range beyond retention
- **WHEN** a client requests `since=720h` on a node with 168h retention
- **THEN** the response covers the last 168h and has `truncated: true`

### Requirement: Metrics history in the CLI
`tako metrics --since <duration|RFC3339> [--step <duration>]` SHALL read the history of every selected server in parallel and show it per node: min, avg, max, last and a sparkline per metric, plus gaps and boots. In machine output modes it SHALL emit a `MetricsHistoryResult` document. `--since` combined with `--live` or `--once` SHALL be rejected as an invalid request (exit 2). Exit codes for failed reads SHALL match `MetricsResult`: exit 1 when all nodes fail, exit 6 on a partial read.

#### Scenario: History for an environment
- **WHEN** `tako metrics --since 24h -e production` runs against two servers
- **THEN** it prints a history block for each server with CPU, memory, disk, load and I/O rows

#### Scenario: JSON history
- **WHEN** `tako metrics --since 6h --output json` runs
- **THEN** stdout is one `MetricsHistoryResult` with a `history` document per node

#### Scenario: Mixed node versions
- **WHEN** one of two servers runs a takod without `node.metrics-history-v1`
- **THEN** that node's entry has an error naming `tako upgrade servers`
- **AND** the command exits 6 and still emits the other node's history

#### Scenario: Conflicting flags
- **WHEN** `tako metrics --since 1h --live` runs
- **THEN** the command fails with an invalid-request error and exit code 2

### Requirement: OS update and reboot reporting
takod SHALL periodically report:
- pending package updates and the security subset, using only the cached package lists (apt simulation or dnf/yum cache-only queries)
- the package-list age
- whether a reboot is required, and since when

takod SHALL NOT refresh package lists, install packages or reboot the node. When the package manager is unsupported, or a probe fails, the result SHALL be `unknown` or stale and SHALL NOT be an error.

#### Scenario: Security updates pending on Ubuntu
- **WHEN** the apt simulation lists 12 upgradable packages, 3 of them from a `-security` origin
- **THEN** node status reports `pendingUpdates: 12`, `securityUpdates: 3`, and the `os-updates` condition is `warn`

#### Scenario: Reboot required
- **WHEN** `/var/run/reboot-required` exists with mtime 2026-10-09T03:12Z
- **THEN** node status reports `rebootRequired: true` with `rebootRequiredSince` 2026-10-09T03:12Z
- **AND** the `reboot-required` condition is `warn`
- **AND** no reboot is performed

#### Scenario: No network access needed
- **WHEN** the OS probe runs
- **THEN** it does not run `apt-get update`, `dnf makecache` or any install command

#### Scenario: Probe fails mid-update
- **WHEN** the apt simulation fails while unattended-upgrades is rewriting package lists
- **THEN** the previous result is kept and marked `stale: true`

### Requirement: Scheduled node conditions
takod SHALL evaluate the read-only node conditions `os-updates`, `reboot-required`, `clock-sync`, `docker-daemon` and `metrics-collector` on their own schedules. It SHALL persist them across restarts and serve them at `GET /v1/node/status`. Each condition SHALL have a `type`, a `status` of `ok`, `warn`, `fail` or `unknown`, a `reason`, a `detail`, `since` and `checkedAt`. Condition probes SHALL NOT mutate the node.

#### Scenario: Clock loses sync
- **WHEN** `timedatectl` reports `NTPSynchronized=no` on two consecutive checks
- **THEN** the `clock-sync` condition is `fail` with `since` set to the first failing check

#### Scenario: Collector stalls
- **WHEN** no sample has been collected for three intervals
- **THEN** the `metrics-collector` condition is `fail`

#### Scenario: Status survives restart
- **WHEN** takod restarts while `reboot-required` has been `warn` since 2026-10-09
- **THEN** after the restart the condition keeps `since` 2026-10-09

### Requirement: Node conditions feed alerts without owning them
When the alert-source interface from `add-takod-health-monitoring` is available, takod SHALL publish each condition as a node-scoped subject `node/<type>`, opening:
- `reboot-required` after 24h in `warn`
- `os-updates` after 72h in `warn`
- `clock-sync`, `docker-daemon` and `metrics-collector` on `fail`

It SHALL send a recovery when a condition returns to `ok`. This change SHALL NOT define alert targets, routing or host-percentage thresholds.

#### Scenario: Reboot pending for a day
- **WHEN** `reboot-required` has been `warn` for 25 hours and node alerts are configured
- **THEN** an alert for `node/reboot-required` opens through the sibling's delivery

#### Scenario: Recovery
- **WHEN** the node is rebooted and `reboot-required` returns to `ok`
- **THEN** a recovery is published for `node/reboot-required`

### Requirement: Doctor reports node status
`tako doctor` SHALL add per-node checks read from `/v1/node/status`: OS security updates, reboot required, clock synchronized and metrics collector. It SHALL also report whether the legacy `tako-monitor` agent is still installed. Nodes without `node.status-v1` SHALL report these checks as `skip`, with remediation `tako upgrade servers`. `--skip-remote` SHALL skip them.

#### Scenario: Reboot pending in doctor
- **WHEN** a node reports `rebootRequired: true`
- **THEN** doctor records a `warn` check `Reboot required` with the since time and packages

#### Scenario: Old node
- **WHEN** a node's takod lacks `node.status-v1`
- **THEN** the node-status checks are `skip` with remediation `tako upgrade servers` and do not change the exit code

#### Scenario: Legacy agent still installed
- **WHEN** `tako-monitor.service` is still enabled on an upgraded node
- **THEN** doctor reports it with the detail that `tako setup` removes it, and the check does not fail

### Requirement: OS status in point-in-time metrics
`tako metrics` without `--since` SHALL show an OS line with pending and security update counts and the reboot-required state when the node provides status. `MetricsNodeSample` SHALL gain an additive `status` field holding the node status document.

#### Scenario: OS line
- **WHEN** a node reports 12 updates, 3 of them security, and a reboot required since 2026-10-09
- **THEN** `tako metrics` prints `OS: 12 updates (3 security), reboot required since 2026-10-09` for that node

### Requirement: Prometheus node series without a network listener
`tako prometheus` SHALL export:
- node condition series
- pending update series
- reboot-required and clock-sync series
- network and disk I/O byte counters
- Docker filesystem usage

takod SHALL NOT open a TCP or HTTP listener for metrics. When started with `--metrics-textfile-dir`, takod SHALL atomically write those node-local series to `tako_node.prom` in that directory after each collection.

#### Scenario: Textfile export
- **WHEN** takod runs with `--metrics-textfile-dir /var/lib/node_exporter/textfile`
- **THEN** after each collection `/var/lib/node_exporter/textfile/tako_node.prom` is replaced via a temporary file and rename

#### Scenario: No listener
- **WHEN** takod runs with any metrics flags
- **THEN** it listens only on its unix socket

#### Scenario: Reboot series
- **WHEN** a node requires a reboot
- **THEN** `tako prometheus` outputs `tako_node_reboot_required{server="<name>"} 1`
