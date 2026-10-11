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
- **Setup steps.** `cmd/setup.go:331-343` runs `monitor-agent` (`engine.SetupStepMonitorAgent`, "Installing monitoring agent") *before* `takod-install` and `takod-service`, and skips it in converge mode (a server already at `setup.CurrentVersion`). Step keys are part of the `SetupResult` machine contract; titles are human text.
- **Siblings** (shared contract: `add-takod-health-monitoring` design D13).
  - `add-takod-health-monitoring` lands first. It owns the alert-source API, routing and delivery, every host threshold alert (CPU, memory, load, disk and inode percentages, and `metrics_stale` for a stalled sample source), and certificate expiry. It reads host samples through its `HostSampleSource` interface, with a legacy `current.json` adapter until this change provides the native one.
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
- **Native host-sample source for monitoring.** This change implements monitoring's `HostSampleSource` (`Latest() (HostSample, time.Time, error)`) over the collector's in-memory latest sample, with bytes and inodes for `/` and a separate Docker data root, memory, CPU and load. Monitoring uses it whenever native collection is enabled and falls back to its legacy adapter only with `--metrics-interval 0`. The sample time it returns drives monitoring's `metrics_stale`.
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
| `os-updates` | **apt:** `apt-get -s -o Debug::NoLocking=1 dist-upgrade` (simulated; unlike `upgrade` it also counts updates that need new or removed packages, such as kernel updates) with `LANG=C`, `nice -n 10` and `ionice -c3`. Count `Inst` lines; a line is security when its origin contains `-security`. List age is the mtime of `/var/lib/apt/periodic/update-success-stamp`, else of `/var/lib/apt/lists`. **dnf/yum:** `--cacheonly check-update` (exit 100 means updates) and `--cacheonly updateinfo list --security` for the security count. 120s timeout. | First run 2–5 min after start, then every 6h | `warn` when security updates are pending; `ok` otherwise. `unknown` when the package manager is unsupported or the probe fails; the previous result is kept with `stale: true`. |
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

- This change does not add alert policy, targets or delivery. It registers alert kind `node` with monitoring's source API (`RegisterSource`, node-scoped) and reports condition levels with `ReportLevel`.
- **Subjects and reasons** (monitoring D13): the subject is the condition type.

  | Condition | Reason | Severity | Opens when | Events (opening / recovery) |
  |---|---|---|---|---|
  | `reboot-required` | `reboot_required` | warning | `warn` for 24h | `node_reboot_required` / `node_condition_cleared` |
  | `os-updates` | `security_updates_pending` | warning | `warn` for 72h (unattended-upgrades is not working) | `node_security_updates` / `node_condition_cleared` |
  | `clock-sync` | `clock_unsynchronized` | critical | `fail` (already two consecutive failed checks) | `node_clock_unsynchronized` / `node_condition_cleared` |
  | `docker-daemon` | `docker_unreachable` | critical | `fail` for 3m (rides out a daemon restart during package upgrades) | `node_docker_unreachable` / `node_condition_cleared` |

- **Windows survive restarts.** This change applies the windows itself from the persisted `since` and reports a level as active only once the window has elapsed; monitoring's damping for kind `node` opens on the first active observation. A takod restart therefore neither restarts a 72h window nor re-opens an episode (monitoring persists episodes).
- **Recovery** is reported when the condition returns to `ok`. `unknown` and stale results report nothing, so a failing probe neither opens nor recovers an episode.
- **`metrics-collector` is not an alert.** A stalled collector is monitoring's `metrics_stale` (kind `host`), driven by the native source's sample time; a history write failure is a disk problem already covered by `high_disk`. The condition stays visible in `/v1/node/status`, doctor and Prometheus.
- **Routing** is monitoring's: kind `node` goes to every scope on the node whose `notifications.alerts.host` is effective with `conditions` true (the default), deduplicated per webhook target by monitoring's ledger. Per-service webhooks never receive it.
- Kind `node` is delivered only on nodes that also advertise `monitoring.health-v1`, which every node with this change does.
- **Landing order:** after `add-takod-health-monitoring`, which provides the source API and the `HostSampleSource` interface; independent of `add-disk-hygiene-defaults`; before `add-local-self-healing`. The kind is registered in the same change that adds the conditions, so there is no deferred wiring.

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
- **Step identity and order:** the step keeps its stable `SetupResult` key `monitor-agent`; only its title changes, to "Removing legacy monitoring agent". It moves from before `takod-install` to after `takod-service`, and it runs in every mode, including converge, because it is idempotent and a previous run may have deferred the removal. `setup.CurrentVersion` is bumped so servers set up by an older release reapply.
- **Ordering guard:** the removal runs only after takod has been installed and is advertising `node.metrics-history-v1`, so a node is never left without metrics. When the guard is not met (for example takod runs with `--metrics-interval 0`), the step completes without removing anything and prints why; doctor keeps reporting the agent.
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
- **Node-scoped alerts on a node shared by several projects.** Every project with host alerts receives `node` alerts, once per webhook target thanks to monitoring's ledger; a project opts out with `notifications.alerts.host.conditions: false`.

## Migration / Compatibility

- **New takod, old CLI:** works. `/v1/metrics` is a superset.
- **New CLI, old takod:** `tako metrics` behaves as today. `--since` and the doctor node-status checks report per-node upgrade hints.
- **No config changes.** All new knobs are takod flags with defaults. No `tako.yaml` field is added.
- **Docs:** `docs/RESOURCE-MONITORING.md` is rewritten. Network and disk I/O become true, history and OS status are added, the "Planned" list item for retention is removed, and the comparison table is corrected. `docs/MACHINE-INTERFACE.md` documents `MetricsHistoryResult`, `MetricsNodeSample.status`, `/v1/metrics/history`, `/v1/node/status` and both capabilities.

## Resolved questions

1. **Default retention: 7 days or 24h?** **7 days**, with the 64 MiB cap. *Rationale: about 2.2 MiB answers "since last week's deploy"; the cap guards the edge cases.*
2. **Node alerts to every configured scope, or explicit opt-in?** **Every scope with effective host alerts, deduplicated per target by monitoring's ledger; opt out with `host.conditions: false`.** *Rationale: node problems affect every project on the node, and opt-in would leave them silent; monitoring owns the routing (its D13).*
3. **Should takod disable the legacy `tako-monitor.service` on startup?** **No.** *Rationale: takod must not gain authority over systemd units; the agent is harmless, doctor reports it and setup removes it.*
4. **Expose `--metrics-textfile-dir` through `tako setup` or `tako.yaml`?** **No; takod flag and a documented drop-in only.** *Rationale: it is host-level, and few users run node_exporter.*
5. **`os-updates` warn on any pending update, or security only?** **Security only**; counts are still shown. *Rationale: routine updates would keep the condition permanently `warn`.*
6. **Reboot-required alert window fixed or configurable?** **Fixed at 24h, measured from the persisted `since`.** *Rationale: gives nightly unattended-upgrades time without paging, and survives takod restarts.*
7. **Per-container stats in history?** **No.** *Rationale: cardinality grows with services; `tako stats` covers the point-in-time view.*
8. **Publish `metrics-collector` as an alert?** **No; monitoring's `metrics_stale` owns a stalled collector.** *Rationale: one alert per fault, and the native source's sample time already drives it.*
9. **How does the setup step change without breaking `SetupResult`?** **Keep the key `monitor-agent`, change only the title, move it after `takod-service`, and run it in every mode.** *Rationale: step keys are machine contract, and the capability guard needs takod installed first.*

## Implementation notes

Differences from the plan above, chosen as the safest equivalent while implementing:

- **`os.listsAgeSeconds`** (integer seconds) is used instead of a `listsAge` duration string, so machine clients do not parse durations.
- **History records add `mp`, `dp`, `ddp` and `w`** (memory, root and Docker disk percent, plus the sample's I/O window in seconds) alongside the planned keys. This keeps bucket math exact without recomputing percentages. Unknown keys are ignored on read.
- **Additive `/v1/metrics` fields beyond D2:** `disk.path`, `cpu_count`, `warnings`, and `*_total` counters in `network` and `disk_io`. These counters feed the Prometheus `_total` series. All legacy fields keep their names and types.
- **`GET /v1/metrics/history` returns 404** when native collection is disabled (`--metrics-interval 0`), matching the missing `node.metrics-history-v1` capability.
- **`reboot-required` `since`** is the earliest observation: the `/var/run/reboot-required` mtime when present, otherwise the first check that saw it. A later probe never moves it forward.
- **Inside an alert window**, a `warn`/`fail` condition is reported to the alert-source API as passing, so an open episode can still recover. `unknown` and stale results report nothing.
- **`tako_node_condition`** emits all four statuses per condition (1 for the current one, 0 for the others), so absent-versus-zero is never ambiguous.
- **No TCP listener** is verified by a source-scan test of `pkg/takod` (no `net.Listen("tcp"…)`/`http.ListenAndServe`), in addition to the textfile writer tests.
- **`ServerOptions.Metrics` with a zero interval keeps legacy behavior** for embedders and existing tests (`NewServer`). The `takod run` flag default (`60s`) is what enables native collection on nodes.
- **Node alert event names** (`node_reboot_required`, …, `node_condition_cleared`) are local takod constants in the `node` alert-source spec, rather than additions to `pkg/notification`. This avoids churn in a package shared with a parallel change.
- **Doctor node-status checks** run inside the existing "Node Agent Health" section, not a new section. The `--skip-remote` section list is unchanged and already skips them. The legacy-agent check is a `pass` with the `tako setup` remediation, so it never fails doctor.
- **`tako metrics --since` accepts a day suffix** (`7d`), which the CLI normalizes to hours (`168h`) before querying takod, because Go durations have no day unit. Gaps in the sparkline render as blanks.
- **Setup:** `RemoveLegacyMonitoringAgent` retries the capability guard 5 times, 2s apart, because takod has just restarted in the preceding step. If the guard is not met, the step completes and emits a setup warning. Converge runs it too.
- **7.3 (manual live-server check)** was not performed in this environment. It is listed in the PR for a reviewer with server access.
- **7.2 verification** ran on Windows: `go build ./...`, `GOOS=linux go vet ./...`, and `go test` for the touched packages that compile on Windows. The Linux-only `pkg/takod` tests run in CI.
