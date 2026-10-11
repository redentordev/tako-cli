# Design

## Context

See proposal.md for motivation and `specs/node-status/spec.md` for the behavior contract. Current state:

- **Collection.**
  - `tako setup` installs `/usr/local/bin/tako-monitor.sh` and the `tako-monitor.service` unit, which runs as root with `Restart=always` (`pkg/provisioner/provisioner.go:1665-1870`, called from `cmd/setup.go:303,340`).
  - The script rewrites `/var/lib/tako/metrics/current.json` every 60s with the fields `timestamp`, `cpu_percent` (string), `memory{total_mb,used_mb,available_mb,percent,swap_total_mb,swap_used_mb}`, `disk{total_mb,used_mb,available_mb,percent}` for `/` only, `uptime_seconds` and `load_average{1min,5min,15min}`.
  - It uses `bc` for floating point.
  - It does not collect network or disk I/O.
- **takod read path.**
  - `ReadNodeMetrics` (`pkg/takod/metrics.go:23-46`) optionally runs `tako-monitor.sh once` and returns the file verbatim in `MetricsResponse.metrics`.
  - `handleMetrics` (`server.go:2618-2645`) adds `alerts`.
- **CLI.**
  - `tako metrics` (`cmd/metrics.go`) fans out over the environment's servers in parallel (`collectMetricsOnceWith`, `:224-268`) and decodes into `MetricsData`. Its `Network` and `DiskIO` fields (`:35-36`) are never populated.
  - In machine modes it emits `MetricsResult` (`pkg/engine/metrics.go:25-35`): all nodes failed exits 1, a partial read exits 6.
  - `--live` is interactive-only.
  - `tako prometheus` (`cmd/prometheus.go`) renders the same samples as a text exposition.
- **Doctor.**
  - Node checks are SSH-side probes from the CLI: takod `/healthz`, mesh status, and root disk percent from `df -P` with warn at 85% and fail at 95% (`cmd/doctor.go:394-517`).
  - The other checks cover the Docker runtime and the agent version.
- **takod process.**
  - It listens only on a unix socket (`server.go:391`), mode 0660 with the takod access group. The CLI reaches it over SSH.
  - It runs as root (`provisioner.go:1043`).
  - Background loops start in `Run` (`server.go:424-436`).
  - GET routes are classified read-only by the lifecycle gate (`lifecycle_gate.go:69-75`).
  - New routes must be added to `registeredRoutes` (`lifecycle_gate.go:25-42`).
- **Upgrade.**
  - `tako upgrade servers` replaces the binary only (`provisioner.go:269-480`). The systemd unit and its flags are written only by `tako setup` (`buildTakodSystemdUnit`, `provisioner.go:1033-1055`).
  - Any new behavior that must reach upgraded nodes therefore has to be the default of a takod flag, not a unit flag.
- **Capability checks.** `takodclient.RequireCapability` returns `*CapabilityRequiredError`, and callers turn it into a `tako upgrade servers` hint (see `pkg/engine/logs.go:583-591`).
- **Siblings.**
  - `add-takod-health-monitoring` owns the alert-source interface and the host threshold alerts: CPU, memory, disk and inode percentages, and certificate expiry.
  - `add-local-self-healing` owns container restarts.
  - `add-disk-hygiene-defaults` owns cleanup actions.

## Goals / Non-Goals

**Goals**
- A single source of host samples inside takod that siblings and the CLI share.
- Answer "what happened on this node in the last N hours" with bounded, predictable disk use.
- Report OS update and reboot state truthfully and cheaply, without changing the OS.
- Evaluate node health continuously with read-only checks, so problems are visible without anyone running `doctor`.
- Make the monitoring docs true.

**Non-Goals**
- A time-series database, cross-node aggregation, or long retention (more than 31 days).
- Alert policy for host thresholds, which belongs to the sibling.
- Any OS mutation: updates, reboots, package installs.
- Exposing takod on the network.

## Decisions

### D1. takod collects natively in Go; the bash agent is retired

- **Choice:** a `hostmetrics` package reads `/proc/stat`, `/proc/meminfo`, `/proc/loadavg`, `/proc/uptime`, `/proc/net/dev` and `/proc/diskstats`, plus `statfs` on `/` and on the Docker data root (`--docker-data-root`, already a takod flag). A collector goroutine samples every `--metrics-interval`, which defaults to `60s`.
- **Why native:**
  - one process and one schema;
  - no `bc` dependency;
  - typed values for history;
  - I/O deltas are easy to compute;
  - siblings can read the latest sample in process instead of parsing a file;
  - the bash agent is the reason the docs and the CLI already disagree.
- **Default on, without changing the unit.** The flag default is 60s, so binary-only upgrades enable it. `--metrics-interval 0` disables native collection. `ReadNodeMetrics` then keeps today's behavior: it reads `current.json` and runs the script for `collect=true`. That leaves an escape hatch.
- **What is collected:**

  | Area | Values |
  |---|---|
  | CPU | Busy share from `/proc/stat` deltas, using the same formula as the script: busy = total − idle − iowait. |
  | Memory | `MemTotal − MemAvailable`, plus swap. |
  | Filesystems | `/` and the Docker data root: bytes and inodes, reported once when both are on the same device (same `statfs` `Fsid`, or the same `st_dev`). |
  | Network | Sum of interfaces excluding `lo`, `veth*`, `docker*`, `br-*` and the Tako mesh interface, as a delta and a rate per second. |
  | Disk I/O | Devices listed in `/sys/block` excluding `loop*`, `ram*`, `zram*` and `dm-*`, with sectors × 512 as a delta and a rate per second. |

- **`collect=true`** takes an immediate sample: CPU uses a 1s delta window bounded by the request context. It does not append to history.
- **Legacy agent coexistence.** An upgraded node may still run `tako-monitor.service`. takod ignores `current.json` while native collection is on. takod does not touch systemd units.
  - **Rationale:** takod mutating systemd units is a new authority with upgrade-rollback consequences. The leftover agent costs a few milliseconds of CPU per minute.
  - `tako doctor` reports it, and re-running `tako setup` removes it (D9).

### D2. `/v1/metrics` document compatibility

- **Shape kept:** the response keeps `collected`, `metrics` and `alerts`. The `metrics` object keeps every legacy field with the same names and types, so `cpu_percent`, `memory.percent` and `disk.percent` stay strings. Old CLIs parse it unchanged.
- **Now populated:** `network{rx_bytes,tx_bytes,rx_mb,tx_mb}` and `disk_io{read_sectors,write_sectors,read_mb,write_mb}`, which the CLI already declares. Byte and sector counts are deltas since the previous sample, matching the documented "since last check" meaning.
- **Additive fields:**
  - `source: "takod"` (absent in legacy mode)
  - `interval_seconds`
  - `disk.inodes_percent`
  - `docker_disk{path,total_mb,used_mb,available_mb,percent,inodes_percent}`, present when the Docker data root is on a different filesystem
  - `network.rx_bytes_per_sec`, `network.tx_bytes_per_sec`, `disk_io.read_bytes_per_sec`, `disk_io.write_bytes_per_sec`
- **Optional `status`:** the response gains an optional top-level `status` object: the node status summary from D5, the same document as `/v1/node/status`. It is omitted when the node status is not yet evaluated.

### D3. History store: hourly JSON-lines segments

- **Layout:** `<data-dir>/metrics/history/YYYYMMDDTHH.jsonl`, in UTC by sample time. Each line is a compact numeric record:

  ```json
  {"t":1767225600,"cpu":12.4,"mu":812,"mt":3920,"su":0,"st":2047,"du":20480,"dt":81920,"di":3.1,"ddu":40960,"ddt":163840,"ddi":1.2,"l1":0.41,"l5":0.38,"l15":0.30,"rx":12345,"tx":6789,"rd":4096,"wr":81920,"up":86400}
  ```

  That is roughly 220 bytes per line, about 2.2 MiB for 7 days at a 60s interval.
- **Why not a binary ring buffer:**
  - Appends with `O_APPEND` plus one write per line are crash-tolerant: a torn last line is skipped on read.
  - Files are human-inspectable.
  - Pruning is deleting old files by name.
  - A schema change only adds keys.
  - The volumes are small enough that format efficiency does not matter.
- **Retention:**
  - `--metrics-retention` defaults to `168h`; the minimum is `1h` and the maximum `744h` (31 days).
  - `--metrics-history-max-bytes` defaults to 64 MiB. When over the cap, the oldest whole files are deleted first.
  - Pruning runs at startup and on each hour rollover.
  - Directories are created 0750 and files written 0640, owned by root with the takod group.
- **Write failure:** if a sample cannot be written (for example the disk is full), the collector keeps serving the in-memory latest sample. It also marks the `metrics-collector` condition `warn` with reason `history-write-failed`, and retries on the next tick. Disk-full never crashes takod.
- **Clock behavior:** records are stored with wall-clock `t`.
  - Queries sort by `t` and drop exact duplicates.
  - A backwards clock jump can produce overlapping records. They are kept, and the buckets average them. Correctness here is best-effort and documented.
  - The `clock-sync` condition (D5) makes skew visible.

### D4. History query API and downsampling

- **Endpoint:** `GET /v1/metrics/history`.
- **Parameters:**
  - `since`: a Go duration, or an RFC3339 timestamp.
  - `until`: optional RFC3339; defaults to now.
  - `step`: optional Go duration.
- **Limits:**
  - The range is clamped to the retention, and a clamped range is reported in `truncated: true`.
  - `step` defaults to the smallest "nice" step from {1m, 2m, 5m, 10m, 15m, 30m, 1h, 2h, 6h} that gives at most 360 buckets.
  - An explicit step below the collector interval, or one that gives more than 1440 buckets, returns HTTP 400.
- **Response:**
  - `node`, `intervalSeconds`, `retentionSeconds`, `from`, `to`, `step`
  - `buckets[]`: `{t, n, cpu{min,avg,max}, mem{min,avg,max}` (percent), `disk{…}` (percent of `/`), `dockerDisk{…}` (when separate), `load1{…}`, `rxRate{avg,max}`, `txRate{…}`, `readRate{…}`, `writeRate{…}}`. `n` is the sample count; empty buckets are omitted.
  - `last`: the newest raw sample, converted to percentages.
  - `gaps[]`: `{from,to}` periods longer than 3× the interval with no samples.
  - `boots[]`: timestamps where `up` decreased between consecutive samples.
- **Reads:** the reader streams only the segment files that overlap the range. With 744h of data the query is bounded by about 45k lines.

### D5. Node status: OS probe and conditions

- **Conditions:** takod keeps an in-memory, persisted (`<data-dir>/node-status.json`) list of `NodeCondition{type, status: ok|warn|fail|unknown, reason, detail, since, checkedAt}`. `since` is when the current status began.
- **Endpoint:** `GET /v1/node/status` returns `{node, evaluatedAt, os{…}, conditions[]}`. Probes run in one loop with per-probe schedules and jitter.

| Condition | Probe | Schedule | Status rules |
|---|---|---|---|
| `os-updates` | **apt:** `apt-get -s -o Debug::NoLocking=1 upgrade` with `LANG=C`, `nice -n 10` and `ionice -c3`. Count `Inst` lines; a line is security when its origin contains `-security`. List age is the mtime of `/var/lib/apt/periodic/update-success-stamp`, else of `/var/lib/apt/lists`. **dnf/yum:** `--cacheonly check-update` (exit 100 means updates) and `--cacheonly updateinfo list --security` for the security count. 120s timeout. | First run 2–5 min after start, then every 6h | `warn` when security updates are pending; `ok` otherwise. `unknown` when the package manager is unsupported or the probe fails; the previous result is kept with `stale: true`. |
| `reboot-required` | **apt:** stat `/var/run/reboot-required`; packages from `reboot-required.pkgs`; `since` is the file mtime. **dnf:** `needs-restarting -r` (exit 1 means required). | 15m | `warn` when required; `unknown` when the tool is missing. |
| `clock-sync` | `timedatectl show -p NTPSynchronized --value` | 15m | `fail` after 2 consecutive `no` results; `unknown` without timedatectl. |
| `docker-daemon` | Ping over the Docker API takod already uses | 1m | `fail` when the ping fails. |
| `metrics-collector` | Internal: age of the newest sample, plus the history write result | Each collector tick and each status read | `fail` when there is no sample for 3× the interval; `warn` when the history write failed. Absent when native collection is disabled. |

- **OS block:** `os{packageManager, pendingUpdates, securityUpdates, securityPackages[≤20], listsAge, rebootRequired, rebootRequiredSince, rebootPackages[≤20], checkedAt, stale}`. Package names are not secret, and the list is capped to keep documents small.
- **Never done:**
  - `apt-get update` or `dnf makecache`: network access, plus lock contention with unattended-upgrades.
  - Installing packages.
  - Rebooting.

  Package-list freshness relies on the periodic refresh that `tako setup` configures (`APT::Periodic::Update-Package-Lists "1"`). It is reported as `listsAge`, so stale data is visible rather than hidden.
- **Lock behavior:** `Debug::NoLocking` avoids blocking on `/var/lib/dpkg/lock` while unattended-upgrades runs. If the lists are being rewritten mid-read, parsing may fail; the probe then keeps the previous result and marks it stale.
- **Not included:** checks that need the operator's workstation (local build inputs, DNS from outside, firewall reachability), anything mutating, and thresholds on percentages. Those stay in CLI `doctor` or belong to siblings.

### D6. Alert publication through the sibling

- This change does not add alert policy, targets or delivery.
- When `add-takod-health-monitoring` is present, its alert-source interface receives each condition transition as a node-scoped subject `node/<type>`. Opening rules:
  - `reboot-required` opens after it has been `warn` for 24h.
  - `os-updates` opens after `warn` (security updates pending) for 72h, which means unattended-upgrades is not working.
  - `clock-sync` and `docker-daemon` open immediately on `fail`.
  - `metrics-collector` opens on `fail`.

  These persistence windows are part of this change's condition contract (`since`). The sibling applies routing and delivery.
- The sibling's host-threshold evaluator reads `hostmetrics.Latest()`, the in-process latest sample, rather than `current.json`.
- **Landing order:**
  - **This change first:** conditions are visible in `/v1/node/status`, `tako metrics`, `tako doctor` and `tako prometheus`, and the alert wiring task is deferred to the sibling.
  - **Sibling first:** it reads the legacy file, and this change switches it to `Latest()`.

### D7. CLI surfaces

**`tako metrics --since <dur|RFC3339> [--step <dur>] [--server S]`**
- Mutually exclusive with `--live` and `--once`; combining them is an invalid request, exit 2.
- The command queries `/v1/metrics/history` on each server in parallel, reusing `collectMetricsOnceWith`.
- **Human output, per node:**
  - a header line with node, range and step;
  - rows for CPU %, memory %, disk % (`/`), Docker disk % when separate, load 1m, net rx/tx rate, and disk read/write rate. Each row shows min, avg, max, last and a fixed-width (48-character) sparkline of bucket averages;
  - gaps and boots listed under the rows.
- **Machine output:** a new `MetricsHistoryResult`:
  - `{apiVersion, kind, project, environment, server?, since, until, step, nodes[{server, host, history?, error?}], error?}`;
  - `history` is the takod document from D4, verbatim.
- **Exit codes:** the same rules as `MetricsResult`. All nodes failing exits 1, a partial read exits 6.
- A node without `node.metrics-history-v1` is a node error naming `tako upgrade servers`.

**`tako metrics` (point-in-time)**
- An `OS:` line is added when `status` is present: `OS: 12 updates (3 security), reboot required since 2026-10-09`.
- `MetricsNodeSample` gains an additive `status` field holding takod's node status document.
- Network and disk I/O lines now appear, because the data exists.

**`tako doctor`**
- New checks per node, read from `/v1/node/status` over the existing takod client:

  | Check | Result |
  |---|---|
  | `OS security updates` | `warn` when pending for more than 72h, else `pass` with counts |
  | `Reboot required` | `warn`, with since and packages |
  | `Clock synchronized` | `fail` when the condition fails |
  | `Metrics collector` | `fail` or `warn` from the condition |
  | `Legacy monitoring agent` | `pass`, with the detail "tako-monitor.service still installed; `tako setup` removes it" |

- The legacy-agent probe is an SSH `systemctl is-enabled` like the existing probes.
- On a node without `node.status-v1`, the checks are `skip` with remediation `tako upgrade servers`.
- `--skip-remote` skips them all.
- The existing disk-pressure check stays CLI-side and unchanged; the threshold alerts are the sibling's.

**`tako prometheus`**
- New series:
  - `tako_node_condition{server,type,status}` (1 for the current status)
  - `tako_node_pending_updates{server,kind="all|security"}`
  - `tako_node_reboot_required{server}`
  - `tako_node_clock_synchronized{server}`
  - `tako_network_receive_bytes_total`, `tako_network_transmit_bytes_total`, `tako_disk_read_bytes_total`, `tako_disk_written_bytes_total` (counters accumulated by takod since start, exposed in the sample as `*_total` additive fields)
  - `tako_docker_filesystem_used_percent`
- Existing series are unchanged.

### D8. No automatic reboots, and no reboot command in this change

- **Why not reboot automatically:**
  - On the typical single-node VPS a reboot takes down every service, including databases, with no drain.
  - On multi-node setups a safe reboot means cordon → drain → reboot → wait → verify → uncordon. Tako deliberately keeps drain and rebalance as reviewed, operator-applied plans (`docs/ORCHESTRATION-MODEL.md:618-628`).
  - `tako setup` intentionally leaves unattended-upgrades' `Automatic-Reboot` unset.
  - Reporting with a `since` timestamp and an alert after 24h gives the operator the decision.
- **A `tako server reboot` command is out of scope.** It needs the drain orchestration above and its own change.

### D9. Setup migration and dead-code removal

- **Setup:** `tako setup` replaces `InstallMonitoringAgent` with `RemoveLegacyMonitoringAgent`. That step is idempotent:
  1. `systemctl disable --now tako-monitor`
  2. remove `/etc/systemd/system/tako-monitor.service` and `/usr/local/bin/tako-monitor.sh`
  3. `systemctl daemon-reload`
  4. leave `current.json` and the `bc` package in place
- **Ordering guard:** the removal runs only after takod has been installed and is advertising `node.metrics-history-v1`, so a node is never left without metrics.
- **`bc`** is no longer installed on new setups.
- **Dead files:** `pkg/monitoring/agent.sh`, `agent_v2.sh` and `tako-monitor.service` are deleted after `git grep` and a `go:embed` search show no references. `pkg/monitoring/monitor.go` stays; it belongs to `tako monitor` and the sibling change.
- **Existing servers:**
  - After `tako upgrade servers`, takod collects natively and history starts.
  - The leftover bash agent keeps writing a file nobody reads until `tako setup` is re-run, and doctor says so.

### D10. Prometheus endpoint: no network listener; an opt-in textfile export

- **Rejected: an HTTP `/metrics` listener on takod.** takod is socket-only by design. Today its trust model is SSH plus a unix socket with a group. A TCP listener would need:
  - an authentication design;
  - firewall changes (`tako setup` denies inbound by default);
  - mesh exposure decisions;
  - its own attack-surface review.
- **Chosen:** an opt-in `--metrics-textfile-dir <dir>` takod flag. On each collection takod atomically writes `<dir>/tako_node.prom` (temp file plus rename) with the node-local series from D7. That is everything except the externally observed alert-owner reachability series.
  - node_exporter's textfile collector scrapes it with node_exporter's own auth and network config.
  - The flag is off by default and documented as a systemd drop-in, and it survives upgrades.
- **`tako prometheus`** (CLI over SSH) stays for setups without node_exporter.

### D11. Capability gating

- **`node.metrics-history-v1`** is advertised when native collection is enabled. It covers `/v1/metrics/history` and the native `metrics` fields. With `--metrics-interval 0` it is not advertised.
- **`node.status-v1`** covers `/v1/node/status` and the `status` object in `/v1/metrics`.
- **CLI behavior:** the CLI checks capabilities per node with `takodclient.RequireCapability`. A missing capability is that node's error in `--since` reads, and a `skip` in doctor. It never fails the whole command when other nodes succeed (exit 6).
- **Old CLI against new takod:** old CLIs ignore the new fields and keep working.

## Risks / Trade-offs

- **The apt simulation costs CPU and I/O every 6h.** It is mitigated by `nice`/`ionice`, the 120s timeout and the 6h interval. On tiny VPSes `apt-get -s` takes about 1–3s.
- **`Debug::NoLocking` can read half-written lists.** A parse failure keeps the previous result as stale, and that state is reported, never fatal.
- **The native and bash CPU numbers may differ slightly on the first sample.** Same formula; the first sample after start reports CPU from a 1s window.
- **`/proc` in containers or LXC can reflect the host or be restricted.** Unreadable sources yield absent fields and a `warn` on `metrics-collector` with a reason. Tako targets VMs and bare metal.
- **Disk use of history.** It is bounded by both the retention and the byte cap. The disk-full path degrades to memory-only (D3).
- **Many servers mean many SSH reads.** History queries reuse the existing parallel fan-out. Payloads are at most 1440 buckets, roughly 300 KB worst case per node.
- **Node-scoped alerts on a node shared by several projects.** Which project's targets receive `node/*` alerts is the sibling's routing decision; see the open questions.

## Migration / Compatibility

- **New takod, old CLI:** works. `/v1/metrics` is a superset.
- **New CLI, old takod:** `tako metrics` behaves as today. `--since` and the doctor node-status checks report per-node upgrade hints.
- **No config changes.** All new knobs are takod flags with defaults. No `tako.yaml` field is added.
- **Docs:** `docs/RESOURCE-MONITORING.md` is rewritten. Network and disk I/O become true, history and OS status are added, the "Planned" list item for retention is removed, and the comparison table is corrected. `docs/MACHINE-INTERFACE.md` documents `MetricsHistoryResult`, `MetricsNodeSample.status`, `/v1/metrics/history`, `/v1/node/status` and both capabilities.

## Open Questions

1. **Default retention: 7 days, or 24h to minimize disk use?** *Recommendation: 7 days.* It is about 2.2 MiB, and "since last week's deploy" is a common question. The byte cap guards the edge cases.
2. **Should `node/*` alerts go to every alert scope configured on the node, or need an explicit opt-in?** *Recommendation: deliver to every configured scope on that node, deduplicated by target.* This needs confirmation by `add-takod-health-monitoring`, which owns routing. Node problems affect every project on the node, and opt-in would leave them silent by default.
3. **Should takod disable the legacy `tako-monitor.service` itself on startup?** *Recommendation: no.* takod should not gain authority over systemd units. The agent is harmless, doctor reports it, and setup removes it.
4. **Expose `--metrics-textfile-dir` through `tako setup` or `tako.yaml`?** *Recommendation: takod flag plus a documented drop-in only, for now.* It is host-level, not project-level, and few users run node_exporter. Revisit if asked.
5. **Should `os-updates` warn on any pending updates, or only security ones?** *Recommendation: security only.* Non-security updates are routine and would make the condition permanently `warn`. Counts are still shown.
6. **Should the reboot-required alert window be 24h or configurable?** *Recommendation: fixed at 24h in v1.* It gives nightly unattended-upgrades time without paging immediately. It can become a sibling policy knob later.
7. **Should history also store per-container stats?** *Recommendation: no, out of scope.* The cardinality grows with services, and `tako stats` covers the point-in-time view.
