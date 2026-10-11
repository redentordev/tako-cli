# Resource Monitoring Guide

Tako CLI provides resource monitoring for both system-level and container-level
metrics. The CLI reads these values through the node-local `takod` socket; Docker
and metrics-file access stay inside the node agent.

## Commands Overview

### 1. `tako metrics` - System Metrics

View system-level resource usage (CPU, RAM, Disk, Network, Disk I/O).

**Basic Usage:**
```bash
# View current metrics
tako metrics --once

# Continuous live updates
tako metrics --live

# Monitor specific server
tako metrics --server prod-1
```

**Output Example:**
```
=== System Metrics ===
Timestamp: 2024-01-15 10:30:45

📊 prod (203.0.113.10)
─────────────────────────────────────────────────
CPU:    67.20% [█████████████░░░░░░░]
Memory: 71.70% (2739 MB / 3820 MB) [██████████████░░░░░░]
Disk:   53% (19295 MB / 38123 MB) [██████████░░░░░░░░░░]
Net I/O: ↓ 15.23 MB  ↑ 8.45 MB (since last check)
Disk I/O: ⬇ 120.5 MB  ⬆ 45.2 MB (since last check)
Load:   3.06 (1m) / 2.89 (5m) / 3.34 (15m)
Uptime: 20h 8m
Updated: 2024-01-15T10:30:45Z
```

**Metrics Collected:**
- **CPU Usage**: Percentage of CPU utilization
- **Memory**: Total, used, available memory + swap
- **Disk**: Total, used, available disk space
- **Network I/O**: Bytes received/transmitted (delta since last check)
- **Disk I/O**: Bytes read/written (delta since last check)
- **Load Average**: 1, 5, and 15-minute load averages
- **Uptime**: System uptime in human-readable format

---

### 2. `tako stats` - Container Statistics

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

### 3. `tako prometheus` - Prometheus Export

Export metrics in Prometheus exposition format for scraping.

**Basic Usage:**
```bash
# Export metrics
tako prometheus
```

**Output Example:**
```
# HELP tako_cpu_usage_percent CPU usage percentage
# TYPE tako_cpu_usage_percent gauge
tako_cpu_usage_percent{server="prod",host="203.0.113.10"} 67.20

# HELP tako_memory_total_bytes Total memory in bytes
# TYPE tako_memory_total_bytes gauge
tako_memory_total_bytes{server="prod",host="203.0.113.10"} 4006387712

# HELP tako_container_cpu_usage_percent Container CPU usage percentage
# TYPE tako_container_cpu_usage_percent gauge
tako_container_cpu_usage_percent{server="prod",host="203.0.113.10",container="tako_nextjs_todos_prod_web_1_0f3a540c3d"} 2.45
```

**Metrics Exported:**

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
- `tako_network_receive_bytes` (counter)
- `tako_network_transmit_bytes` (counter)
- `tako_disk_read_bytes` (counter)
- `tako_disk_write_bytes` (counter)
- `tako_load_average_1m`
- `tako_load_average_5m`
- `tako_load_average_15m`
- `tako_uptime_seconds` (counter)

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
report its own outage; the CLI emits `owner_reachable 0` when that observer
cannot query the node's alert endpoint. During a full cordon or drain, ordinary
node metrics remain available and `owner_present` is `0` for the environment.

---

## Integration with Prometheus

### Setup Prometheus Scraping

Prometheus cannot read a metrics file directly. Publish `tako prometheus`
output through the
[node_exporter textfile collector](https://github.com/prometheus/node_exporter#textfile-collector)
on an observer machine that can reach the Tako environment, then scrape
node_exporter.

1. **Run node_exporter with the textfile collector** pointed at a directory
   the export job can write to:

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

   node_exporter only reads files ending in `.prom`, so the temporary
   `tako.prom.<pid>` name is ignored until the rename.

3. **Configure Prometheus** to scrape node_exporter (port 9100 by default):

<!-- tako:snippet skip: prometheus config -->
```yaml
scrape_configs:
  - job_name: 'tako'
    scrape_interval: 60s
    static_configs:
      - targets: ['observer.internal:9100']
```

If you prefer a dedicated endpoint instead of node_exporter, serve the exported
file with a real HTTP server (any static file server works). A bare `nc` loop
does not send HTTP response headers, so Prometheus cannot scrape it.

---

## Monitoring Agent

The monitoring agent runs on each server and collects host metrics every 60
seconds. `takod` exposes those metrics plus credential-free scheduler alert
status through `/v1/metrics`, exposes alert status independently through
`/v1/alerts`, and exposes container stats through `/v1/stats`.

### Agent Installation

The agent is automatically installed when you run:
```bash
tako setup
```

### Manual Agent Management

```bash
# Start agent
sudo systemctl start tako-monitor

# Stop agent
sudo systemctl stop tako-monitor

# Check status
sudo systemctl status tako-monitor

# View agent logs
sudo journalctl -u tako-monitor -f
```

### Agent Configuration

Agent settings are in `/usr/local/bin/tako-monitor.sh`:

```bash
# Collection interval (default: 60 seconds)
MONITOR_INTERVAL=60
```

To change the interval:
```bash
# Edit the service file
sudo systemctl edit tako-monitor

# Add:
[Service]
Environment="MONITOR_INTERVAL=30"

# Restart
sudo systemctl restart tako-monitor
```

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
# Check which containers are using most resources
tako stats

# Check system-level bottlenecks
tako metrics --once
```

### 3. Capacity Planning

```bash
# Export metrics for analysis
tako prometheus > metrics-$(date +%Y%m%d-%H%M%S).txt

# Or continuous collection
while true; do
  tako prometheus >> metrics-history.txt
  sleep 300  # Every 5 minutes
done
```

### 4. Alerting

takod checks health and sends alerts from each node, so alerts keep working
when your laptop is off. Nodes need the `monitoring.health-v1` capability
(`tako upgrade servers`). Deploy pushes the policy from tako.yaml:

```yaml
notifications:
  slack: ${SLACK_WEBHOOK_URL}
  alerts:
    services: { alertAfter: 3, recoverAfter: 2 }   # damping defaults
    host: { disk: 90, inodes: 90, memory: 90, cpu: 95, cpuFor: 10m, load: 4 }
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
  They dial the local proxy with the service's domain, check the
  health path, and verify the certificate's hostname and validity. A
  `container` check runs on every node that has replicas. Checks are
  paused during deploys and for the start period after a deploy, and while
  maintenance mode is on.
- **Host**: disk and inode usage per filesystem, memory, sustained CPU, and
  load per core, taken from the node's metrics sample. If the sample is
  older than 5 minutes, the node raises `metrics_stale`.
- **Containers**: OOM kills are one-off events (one alert per 15 minutes,
  with a repeat count). Crash loops open an episode and recover after two
  quiet windows.
- **Certificates**: routed domains are scanned every 6 hours, starting 2
  minutes after takod starts.

Damping, escalation and flap control apply to every subject. An episode
opens after `alertAfter` failed checks and recovers after `recoverAfter`
passing checks. A rise in severity sends an escalation. A subject that
changes state more than 6 times in an hour sends one `suppressed` alert,
then stays silent until it has been stable for 30 minutes. Node-wide
subjects (host and certificates) alert once per target, even when several
projects share the node.

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

A `monitoring.webhook` that is not public HTTPS was accepted by earlier
releases. Now it produces a deprecation warning and is never used.

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
| Network I/O | ✅ | ❌ | ✅ |
| Disk I/O | ✅ | ✅ | ✅ |
| Container metrics | ❌ | ✅ | ✅ |
| Historical data | ❌ | ❌ | Depends on scraper |
| Live updates | ✅ | ✅ | ❌ |
| Prometheus format | ❌ | ❌ | ✅ |
| Multi-server | ✅ | ✅ | ✅ |

---

## Performance Impact

The monitoring agent has minimal performance impact:

- **CPU**: < 0.1% average
- **Memory**: ~ 5-10 MB
- **Disk I/O**: Minimal (only writes once per interval)
- **Network**: None (metrics stored locally)

---

## Troubleshooting

### Metrics not available

```bash
# Reinstall or refresh takod and the monitoring agent
tako setup

# Check node runtime health
tako doctor

# Read metrics through takod
tako metrics --once --verbose
```

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
- [ ] Built-in HTTP server for Prometheus scraping
- [ ] Metrics aggregation across multiple servers
- [ ] Metrics retention and historical queries
- [ ] Grafana dashboard templates
- [ ] Custom metric collection plugins
