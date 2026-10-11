# Resource Monitoring Guide

Tako reports system metrics, container metrics, metrics history and OS status
for every server. `takod` collects all of it on the node itself. The CLI reads
it through the node-local `takod` socket over SSH, so Docker, `/proc` and the
history files stay inside the node agent. takod never opens a network
listener.

## How collection works

takod samples host metrics natively every 60 seconds from `/proc` and
`statfs`. It needs no separate agent or extra packages. Each sample contains:

- **CPU**: utilization over the sampling window, and the CPU count
- **Memory**: total, used and available memory, plus swap
- **Disk**: usage and inode usage for `/`, and for the Docker data root when it
  is a separate filesystem
- **Network I/O**: bytes received and sent on host interfaces, excluding
  loopback, Docker bridges, `veth*` and the Tako mesh interface. This is
  reported as a per-interval delta, as a rate, and as a total since takod
  started.
- **Disk I/O**: bytes read and written on whole disk devices, excluding loop,
  RAM, zram and device-mapper devices. This uses the same delta, rate and total
  forms as network I/O.
- **Load average**: 1, 5 and 15 minutes
- **Uptime**

### takod flags

These flags belong to `takod run`. The defaults suit most servers, and no
`tako.yaml` change is needed.

| Flag | Default | Meaning |
|------|---------|---------|
| `--metrics-interval` | `60s` | Sampling interval, minimum `5s`. `0` turns native collection off. takod then serves the legacy agent's `/var/lib/tako/metrics/current.json` as before, and history is unavailable. |
| `--metrics-retention` | `168h` | How long history is kept on the node (`1h` to `744h`). |
| `--metrics-history-max-bytes` | `67108864` (64 MiB) | Hard size cap for history. The oldest hourly files are deleted first. |
| `--metrics-textfile-dir` | off | Writes `tako_node.prom` to this directory after each sample (see [Textfile export](#textfile-export)). |

Native collection is on by default through takod's own flag defaults.
`tako upgrade servers` enables it on existing nodes without re-running setup.
Nodes that collect natively advertise the `node.metrics-history-v1`
capability.

### History

After each sample, takod appends one line to an hourly JSON-lines file under
`/var/lib/tako/metrics/history/`. The directory has mode `0750` and the files
have mode `0640`. takod prunes files by retention and size cap at startup and
whenever the hour changes. A torn last line, for example after a power loss, is
skipped when the history is read.

## Commands Overview

### 1. `tako metrics` - System Metrics

**Basic Usage:**
```bash
# Latest sample from every server in the environment
tako metrics

# Take a fresh sample now instead of reading the latest one
tako metrics --once

# Continuous live updates (interactive only)
tako metrics --live

# One server
tako metrics --server prod-1
```

**Output Example:**
```
=== System Metrics ===
Timestamp: 2026-10-12 10:30:45

📊 prod (203.0.113.10)
─────────────────────────────────────────────────
CPU:    12.50% [██░░░░░░░░░░░░░░░░░░]
Memory: 25.00% (1024 MB / 4096 MB) [█████░░░░░░░░░░░░░░░]
Disk:   40.00% (40000 MB / 100000 MB) [████████░░░░░░░░░░░░]
Docker: 25.00% (50000 MB / 200000 MB) [█████░░░░░░░░░░░░░░░]
Net I/O: ↓ 17.1 KiB/s  ↑ 8.5 KiB/s
Disk I/O: read 17.1 KiB/s  write 34.1 KiB/s
Load:   0.50 (1m) / 0.40 (5m) / 0.30 (15m)
Uptime: 1d 1h 1m
OS: 12 updates (3 security), reboot required since 2026-10-09
Updated: 2026-10-12T10:30:00Z
```

The `Docker:` line appears only when the Docker data root is on its own
filesystem. The `OS:` line appears for nodes that report node status (see
[OS status](#os-status)). Nodes still served by the legacy agent show
per-interval I/O deltas instead of rates.

In machine output modes, the command emits a `MetricsResult`. Each node carries
the takod `/v1/metrics` document and, when available, a `status` field with
the node status document. A partial read exits with code 6. If every node
fails, the command exits with code 1.

### 2. `tako metrics --since` - Metrics History

```bash
tako metrics --since 24h               # default step: at most 360 buckets (5m here)
tako metrics --since 7d --step 1h
tako metrics --since 2026-10-11T00:00:00Z
tako metrics --since 6h --output json  # MetricsHistoryResult
```

For each node, the command prints a header with the range and step. It then
prints one row per metric: CPU %, memory %, disk % for `/`, Docker disk %
(when separate), load 1m, network rx/tx rate and disk read/write rate. Each
row shows min, avg, max and last values, followed by a 48-character sparkline
of the bucket averages. Under the rows, it lists gaps (periods longer than
three intervals with no samples) and boots.

```
📈 prod (203.0.113.10)
2026-10-11 10:30 → 2026-10-12 10:30 UTC, step 5m, 288 bucket(s)
─────────────────────────────────────────────────
                     min         avg         max        last  trend
CPU                 1.2%        8.4%       92.0%        7.5%  ▁▁▂▁▁▃▂▁▁▁▂▁▁▁▁▁▁▂▁▁▁▁█▂▁▁▁▁▁▁▂▁▁▁▁▁▁▁▂▁▁▁▁▁▂▁▁▁
...
Gap:  2026-10-12 03:00 → 2026-10-12 03:20 UTC (20m without samples)
Boot: 2026-10-12 03:20 UTC
```

Rules for `--since` and `--step`:

- `--since` accepts a duration (`24h`, `7d`) or an RFC3339 timestamp.
- `--step` must not be shorter than the node's sampling interval and must not
  produce more than 1440 buckets.
- A window that reaches further back than retention is clamped and marked as
  truncated.
- `--since` cannot be combined with `--live` or `--once`. That combination
  exits with code 2.
- Nodes are read in parallel and reported separately. Values are never merged
  across nodes.
- A node whose takod lacks `node.metrics-history-v1` gets an error that names
  `tako upgrade servers`. A partial read exits with code 6. If every node
  fails, the command exits with code 1.

### 3. `tako stats` - Container Statistics

View per-container resource usage collected by `takod` from local containers.
Tako containers use deterministic runtime names in the form
`tako_<project>_<environment>_<service>_<slot>_<hash>`. Filtering uses Tako's
Docker labels, so `--service web` does not depend on parsing those names.

**Basic Usage:**
```bash
# View container stats (current project only)
tako stats

# Continuous live updates
tako stats --live

# Filter by service
tako stats --service web

# Show all containers (not just current project)
tako stats --all
```

**Output Example:**
```
=== Container Statistics ===
Timestamp: 2024-01-15 10:35:22

📊 prod (203.0.113.10)
─────────────────────────────────────────────────────────────────────────────────────
CONTAINER                                   CPU %        MEMORY     MEM %           NET I/O      BLOCK I/O
─────────────────────────────────────────────────────────────────────────────────────
tako_nextjs_todos_prod_web_1_0f3a54...     2.45%    256MiB / 2GiB    12.5%    1.2MB / 850kB   10MB / 2MB
tako_nextjs_todos_prod_postgres_1_c20...   0.89%    128MiB / 2GiB     6.3%     450kB / 320kB    5MB / 8MB
```

**Metrics Per Container:**
- **CPU %**: CPU usage percentage
- **Memory**: Used / Limit
- **Mem %**: Memory usage percentage
- **Net I/O**: Network bytes received / transmitted
- **Block I/O**: Disk bytes read / written

---

### 4. `tako prometheus` - Prometheus Export

Export metrics in Prometheus exposition format.

```bash
tako prometheus
```

**Output Example:**
```
# HELP tako_cpu_usage_percent CPU usage percentage
# TYPE tako_cpu_usage_percent gauge
tako_cpu_usage_percent{server="prod",host="203.0.113.10"} 12.50

# HELP tako_node_reboot_required Whether the node requires a reboot
# TYPE tako_node_reboot_required gauge
tako_node_reboot_required{server="prod"} 1
```

**System Metrics:**
- `tako_cpu_usage_percent`
- `tako_memory_total_bytes`
- `tako_memory_used_bytes`
- `tako_memory_usage_percent`
- `tako_swap_total_bytes`
- `tako_swap_used_bytes`
- `tako_disk_total_bytes`
- `tako_disk_used_bytes`
- `tako_disk_usage_percent`
- `tako_network_receive_bytes`, `tako_network_transmit_bytes` (bytes in the last sampling interval)
- `tako_disk_read_bytes`, `tako_disk_write_bytes` (bytes in the last sampling interval)
- `tako_load_average_1m`
- `tako_load_average_5m`
- `tako_load_average_15m`
- `tako_uptime_seconds`

**Node Metrics** (only from nodes with native collection or node status):
- `tako_network_receive_bytes_total{server}`, `tako_network_transmit_bytes_total{server}`: counters since takod started
- `tako_disk_read_bytes_total{server}`, `tako_disk_written_bytes_total{server}`: counters since takod started
- `tako_docker_filesystem_used_percent{server}`: only when the Docker data root is a separate filesystem
- `tako_node_condition{server,type,status}`: `1` for the condition's current status and `0` for the other statuses (`ok`, `warn`, `fail`, `unknown`)
- `tako_node_pending_updates{server,kind="all"|"security"}`
- `tako_node_reboot_required{server}`
- `tako_node_clock_synchronized{server}`

Nodes still served by the legacy agent emit none of the node series.

**Container Metrics:**
- `tako_container_cpu_usage_percent`
- `tako_container_memory_used_bytes`
- `tako_container_memory_limit_bytes`
- `tako_container_memory_usage_percent`

**Scheduler Alert Metrics (when `notifications.alerts` is enabled):**
- `tako_scheduler_alert_owner_present`
- `tako_scheduler_alert_owner_reachable`
- `tako_scheduler_alert_policy_loaded`
- `tako_scheduler_alert_active_episodes`
- `tako_scheduler_alert_pending_deliveries`
- `tako_scheduler_alert_exhausted_deliveries`
- `tako_scheduler_alert_last_evaluation_timestamp_seconds`
- `tako_scheduler_alert_subject_active`
- `tako_scheduler_alert_subject_last_success_timestamp_seconds`

Generate these metrics from an independent observer. The owner node cannot
report its own outage. When the observer cannot query the node's alert
endpoint, the CLI emits `owner_reachable 0`. During a full cordon or drain,
ordinary node metrics remain available and `owner_present` is `0` for the
environment.

---

## OS status

takod also reports node conditions at `GET /v1/node/status` (capability
`node.status-v1`). It keeps them across restarts in
`/var/lib/tako/node-status.json`.

| Condition | Checked | Meaning |
|-----------|---------|---------|
| `os-updates` | every 6h (first run 2–5 min after start) | Pending package updates and the security subset, read from the **cached** package lists only (`apt-get -s upgrade`, or `dnf`/`yum --cacheonly`). `warn` while security updates are pending. |
| `reboot-required` | every 15m | `/var/run/reboot-required` (Debian/Ubuntu) or `needs-restarting -r` (Fedora/RHEL family). `warn` with the time since the reboot was first required, plus the packages that need it. |
| `clock-sync` | every 15m | `timedatectl` `NTPSynchronized`. `fail` after two consecutive unsynchronized checks. |
| `docker-daemon` | every 1m | Whether the Docker daemon answers. |
| `metrics-collector` | each sample | `fail` when no sample has been taken for three intervals, and `warn` when history writes fail. |

**Tako never installs updates or reboots a node.** The probes run with `nice`,
`ionice` and a 120-second timeout. They never run `apt-get update`,
`dnf makecache`, any install command or a reboot. When a package manager is
unsupported, the condition is `unknown`. When a probe fails, for example while
unattended-upgrades is rewriting the package lists, takod keeps the previous
result and marks it `stale`.

`tako doctor` reads the same document in its "Node Agent Health" section. It
adds checks for OS security updates (`warn` once they have been pending for
72h), reboot required (`warn`), clock synchronization (`fail`) and the metrics
collector. It also notes when the legacy `tako-monitor` agent is still
installed. Nodes without `node.status-v1` report these checks as `skip` with
the remediation `tako upgrade servers`.

---

## Textfile export

takod never opens a TCP or HTTP listener. To scrape the node-local series
directly from each server, start takod with `--metrics-textfile-dir`. Point it
at node_exporter's textfile collector directory:

```bash
# in the takod unit: ExecStart=... takod run ... --metrics-textfile-dir /var/lib/node_exporter/textfile
node_exporter --collector.textfile.directory=/var/lib/node_exporter/textfile
```

After each sample, takod writes `tako_node.prom` to a temporary file and
renames it into place. node_exporter therefore never reads a partially written
file. The file contains the node series listed above (I/O counters, Docker
filesystem usage, node conditions, pending updates, reboot and clock state).

## Integration with Prometheus

To collect the full `tako prometheus` output, including container and scheduler
series, publish it through the
[node_exporter textfile collector](https://github.com/prometheus/node_exporter#textfile-collector)
on an observer machine that can reach the Tako environment. Then scrape
node_exporter.

1. **Run node_exporter with the textfile collector**, pointed at a directory
   that the export job can write to:

```bash
sudo mkdir -p /var/lib/node_exporter/textfile
node_exporter --collector.textfile.directory=/var/lib/node_exporter/textfile
```

2. **Export metrics every minute with a cron job or systemd timer.** Write to a
   temporary file in the same directory and `mv` it into place, so
   node_exporter never reads a partially written file:

```bash
*/1 * * * * /usr/local/bin/tako prometheus > /var/lib/node_exporter/textfile/tako.prom.$$ && mv /var/lib/node_exporter/textfile/tako.prom.$$ /var/lib/node_exporter/textfile/tako.prom
```

   node_exporter only reads files that end in `.prom`, so it ignores the
   temporary `tako.prom.<pid>` file until the rename.

3. **Configure Prometheus** to scrape node_exporter (port 9100 by default):

<!-- tako:snippet skip: prometheus config -->
```yaml
scrape_configs:
  - job_name: 'tako'
    scrape_interval: 60s
    static_configs:
      - targets: ['observer.internal:9100']
```

---

## Legacy monitoring agent

Earlier releases installed a bash agent, `tako-monitor.service`, that wrote
`/var/lib/tako/metrics/current.json`. It is no longer installed:

- While native collection is on, takod ignores `current.json`. A leftover agent
  is harmless but unused.
- `tako setup` (setup version `1.2.3`) runs the `monitor-agent` step after
  `takod-service` in every mode, including converge. The step disables the
  agent and deletes its unit and script, but only after takod advertises
  `node.metrics-history-v1`. If takod does not confirm native collection, the
  agent stays in place and setup prints a warning.
- `tako doctor` notes when the agent is still installed.
- With `takod run --metrics-interval 0`, takod falls back to reading the
  legacy agent's file, if that file exists.

---

## Use Cases

### 1. Real-time Monitoring Dashboard

```bash
# Live system metrics
tako metrics --live

# Live container stats
tako stats --live
```

### 2. Debugging Performance Issues

```bash
# What happened overnight?
tako metrics --since 12h

# Check which containers are using most resources
tako stats

# Check system-level bottlenecks right now
tako metrics --once
```

### 3. Capacity Planning

```bash
# A week of hourly buckets per node
tako metrics --since 7d --step 1h

# Machine-readable history for analysis
tako metrics --since 7d --output json > history.json
```

### 4. Alerting

takod checks health and sends alerts from each node, so alerts keep working
when your laptop is off. Nodes need the `monitoring.health-v1` capability
(`tako upgrade servers`). A deploy pushes the policy from tako.yaml:

```yaml
notifications:
  slack: ${SLACK_WEBHOOK_URL}
  alerts:
    services: { alertAfter: 3, recoverAfter: 2 }   # damping defaults
    host: { disk: 90, inodes: 90, memory: 90, cpu: 95, cpuFor: 10m, load: 4, conditions: true }
    containers: { oom: true, crashLoop: 3, crashLoopWindow: 10m }
    certificates: { warnBefore: 336h, criticalBefore: 72h }

services:
  web:
    monitoring:
      enabled: true
      interval: 30s     # 10s..1h, default 60s
      timeout: 5s       # 1s..60s, shorter than interval
      webhook: https://hooks.example.com/web   # optional extra target for this service only
```

How it works:

- **Services**: `http` checks run on the node that serves the proxy route.
  They dial the local proxy with the service's domain, check the health path,
  and verify the certificate's hostname and validity. A `container` check runs
  on every node that has replicas. Checks are paused during deploys, for the
  start period after a deploy, and while maintenance mode is on.
- **Host**: checks disk and inode usage per filesystem, memory, sustained CPU
  and load per core. The values come from takod's native sample. If the
  sample is older than 5 minutes, the node raises `metrics_stale`.
- **Node conditions** (kind `node`, on by default with
  `host.conditions: true`):
  - `reboot_required` (warning) opens after 24h of `reboot-required`.
  - `security_updates_pending` (warning) opens after 72h of `os-updates`.
  - `clock_unsynchronized` (critical) opens on `clock-sync` `fail`.
  - `docker_unreachable` (critical) opens after 3 minutes of `docker-daemon`
    `fail`.

  Windows are measured from the condition's persisted `since`, so a restart
  does not reset them. A condition that returns to `ok` sends
  `node_condition_cleared`. `unknown` and stale results neither open nor
  recover an alert, and the `metrics-collector` condition is never alerted on
  separately (`metrics_stale` covers it).
- **Containers**: OOM kills are one-off events (one alert per 15 minutes,
  with a repeat count). Crash loops open an episode and recover after two
  quiet windows.
- **Certificates**: routed domains are scanned every 6 hours, starting 2
  minutes after takod starts.

Damping, escalation and flap control apply to every subject. An episode opens
after `alertAfter` failed checks and recovers after `recoverAfter` passing
checks. A rise in severity sends an escalation. A subject that changes state
more than 6 times in an hour sends one `suppressed` alert, then stays silent
until it has been stable for 30 minutes. Node-wide subjects (host, node and
certificates) alert once per target, even when several projects share the
node.

Every alert uses webhook payload schema v1 (`schemaVersion: 1`). It carries
`eventId`, `episodeId`, `transition` (opening, recovery, escalation, event,
suppressed), `event`, `project`, `environment`, `node`, `kind`, `subject`
(`service` is an alias), `severity`, `reasons`, `details`, `openedAt`,
`message` and `timestamp`. Payloads contain only codes, public names and
numbers. They never include response bodies, error text, headers or
environment values. Slack and Discord receive only `message`.

Use `tako monitor` to read the state:

```bash
tako monitor                  # KIND SUBJECT NODE STATE SEVERITY SINCE LAST CHECK REASONS
tako monitor --service web
tako monitor --watch          # refresh every 5s
tako monitor --output json    # MonitorResult; exit 6 when anything needs attention
tako monitor --local          # legacy outside checker running on this machine (node "external")
```

`tako prometheus` exports the following series:

- `tako_monitor_subject_active{project,environment,node,kind,subject,severity}`
- `tako_monitor_subject_last_observed_timestamp_seconds`
- `tako_monitor_pending_deliveries{project,environment,node}`
- `tako_monitor_exhausted_deliveries{project,environment,node}`
- `tako_host_metrics_stale{node}`

Earlier releases accepted a `monitoring.webhook` that is not public HTTPS.
Such a webhook now produces a deprecation warning and is never used.

Monitoring only reports. To let takod also restart unhealthy or crashed
containers on their own node, opt in to [Self-Healing](SELF-HEALING.md). Its
`gave_up`, `circuit_open` and `action_failed` alerts use kind `selfheal` and
the same targets.

---

## Disk Hygiene

takod agents with the `node.disk-hygiene-v1` capability (`tako upgrade
servers`) keep their own disk tidy, with no extra setup.

**What runs**

- **Daily maintenance** (`--hygiene-interval`, default 24h; when only the
  older `--build-cache-prune-interval` is set, that interval is used). It
  removes old Tako-built images beyond the newest 3 per service and
  environment (`--image-retention`), then prunes the build cache to
  `--build-cache-keep-storage`. Unattended runs keep any image younger
  than 24h.
- **Hourly pressure check** (`--hygiene-check-interval`, default 1h). It
  measures free space on the takod data directory (`root`) and the Docker
  data root (`docker`):
  - the soft threshold is `max(1.5 × minimum, min(10% of the filesystem, 50 GiB))`;
  - the hard threshold is `1.5 × --minimum-free-disk-bytes`.

  Below a threshold, a reclaim runs. Below soft, it removes dangling
  images, build cache over the budget, and old images. Below hard, it also
  shrinks the build cache to `--build-cache-pressure-keep-storage` (2GB)
  and removes exited Tako containers. Each step stops once free space is
  back above the soft threshold. A proactive reclaim that does not recover
  enough is not repeated at that level for 6 hours.

  These checks measure free bytes live, with the same probe that admits
  disk-using requests, so a reclaim can re-measure after each step. They
  do not read the native metrics sample. The host `disk` alert above
  covers percentage used from that sample; hygiene acts on the free-byte
  floor.
- **Reclaim before refusing.** When a node denies a request for low disk
  (HTTP 507), the CLI asks it to reclaim (`POST /v1/hygiene/reclaim`). The
  CLI then retries the request once, printing `Disk low on <server>:
  reclaimed X, retrying…` and emitting a `hygiene.reclaimed` event. A
  denied scheduler run (backup, job, certificate) also triggers a reclaim.

**What is never touched**

Hygiene never touches:

- volumes, backups or `/opt/<project>` files;
- running containers or container log files;
- non-Tako containers and images;
- images pulled with `image:`.

Old-image removal only applies to the (project, environment) scopes a deploy
or `tako cleanup` has recorded on the node. It skips a scope whose deploy
lease is held, or that has a restore-on-failure retention record. It never
removes an image that a current deployment, another environment or any
container references. If the deployment state cannot be read, nothing is
removed in that scope.

Rolling back past the retained images fails at the image preflight, with a
hint to redeploy that revision or raise `--image-retention`.

**Default log rotation.** New containers of services without a `logging`
block get `json-file` rotation (`max-size=20m`, `max-file=5`). This applies
unless the Docker default driver is not `json-file`, or `daemon.json` sets
`log-driver` or `log-opts` (or is unreadable). See
[Configuration](CONFIGURATION.md).

**Seeing what it did**

- `tako cleanup --dry-run` shows, per server, which old images would be
  removed and why the rest are kept (`in use`, `current`,
  `other environment`, `retention`, `recent`).
- `tako doctor` adds a "Disk Hygiene" check per server (id
  `disk-hygiene`). It warns on unrotated logs over 1 GiB, operator-managed
  logging, unreadable state, no recorded scope, or no run in 48h. It fails
  when a reclaim could not recover enough space.
- takod keeps a journal of the last 50 runs in
  `<data-dir>/hygiene/journal.json`. `GET /v1/hygiene` on the takod socket
  returns:
  - the policy and thresholds;
  - the recorded scopes;
  - the last runs;
  - containers that run without log rotation.
- Alerts use kind `hygiene` (node-wide):
  - `reclaim_insufficient` (critical, subject `root` or `docker`): a
    reclaim left free space below the hard threshold. Recovers with
    `resource_normal` above the soft threshold.
  - `state_unreadable` (warning, subject `image-gc`): image GC skipped a
    scope because its state could not be read. Recovers with
    `image_gc_resumed`.

**Changing the defaults.** `tako setup` writes no hygiene flags, so the
defaults apply. To override them, add a systemd drop-in that repeats the
unit's `ExecStart` with your flags:

```bash
sudo systemctl cat takod            # copy the current ExecStart line
sudo systemctl edit takod
```

```ini
[Service]
ExecStart=
ExecStart=<current ExecStart line> --hygiene-interval 12h --image-retention 5 --default-container-logging=off
```

```bash
sudo systemctl restart takod
```

`tako upgrade servers` rewrites `takod.service` but not drop-ins. After an
upgrade, check that the drop-in's `ExecStart` still matches the new unit.
`--hygiene-interval=0` disables the autonomous runs. Pressure checks are
part of those runs, so they stop too. The reclaim endpoint and
`tako cleanup` keep working.

---

## Comparison with Other Tools

| Feature | tako metrics | tako stats | tako prometheus |
|---------|-------------|-----------|-----------------|
| System CPU/RAM/Disk | ✅ | ❌ | ✅ |
| Network I/O | ✅ (rates) | ✅ (per container) | ✅ (counters) |
| Disk I/O | ✅ (rates) | ✅ (per container) | ✅ (counters) |
| Container metrics | ❌ | ✅ | ✅ |
| Historical data | ✅ (`--since`, on-node, 7 days by default) | ❌ | Depends on scraper |
| OS updates / reboot status | ✅ | ❌ | ✅ |
| Live updates | ✅ | ✅ | ❌ |
| Prometheus format | ❌ | ❌ | ✅ |
| Multi-server | ✅ | ✅ | ✅ |

---

## Performance Impact

Native collection runs inside takod:

- **CPU**: a few `/proc` reads and two `statfs` calls per interval
- **Disk**: about 300 bytes of history per sample, roughly 3 MiB per week at
  the default interval, capped at 64 MiB
- **OS probes**: cache-only package queries every 6 hours, run at low CPU and
  I/O priority
- **Network**: none; everything stays on the node until the CLI asks for it

---

## Troubleshooting

### Metrics not available

```bash
# Upgrade takod (enables native collection on existing nodes)
tako upgrade servers

# Check node runtime health, node status and the metrics collector
tako doctor

# Read metrics through takod
tako metrics --once --verbose
```

### History not available

`tako metrics --since` requires `node.metrics-history-v1`. Run
`tako upgrade servers`, and check that takod is not running with
`--metrics-interval 0`.

### Container stats empty

```bash
# Verify project containers through takod
tako ps

# Check all containers visible to the node agent
tako stats --all
```

### Prometheus format issues

```bash
# Validate output
tako prometheus | promtool check metrics
```

---

## Future Enhancements

Planned features:
- [ ] Metrics aggregation across multiple servers
- [ ] Grafana dashboard templates
- [ ] Custom metric collection plugins
