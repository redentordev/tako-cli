# Proposal

## Why

Tako has no unattended monitoring of the apps and hosts it deploys. What exists today:

- **`tako monitor` only runs while a terminal is open** (`cmd/monitor.go`, `pkg/monitoring/monitor.go`). It also has bugs:
  - It ignores `monitoring.interval` and always checks every 10s (`monitor.go:92`).
  - It hardcodes the server name `"prod"` in every alert (`monitor.go:147`).
  - It never sends a recovery alert.
  - It re-sends `service_down` on every failing check, with no deduplication.
  - It POSTs with a plain `http.Client`, bypassing the SSRF-safe alert target validation.
- **takod alerts cover only cron jobs and backup age.** `pkg/takod/alerts.go` has durable episodes (opening and recovery), retried deliveries, credential generations and an SSRF-safe client, but the subject kinds are hardcoded to `job` and `backup` (`alerts.go:1164-1166`). The reason codes are fixed to five values (`alerts.go:1176-1183`).
- **Nobody sends the host and container alert types.** `pkg/notification` declares `high_cpu`, `high_memory`, `high_disk`, `container_oom`, `container_crash_loop`, `ssl_expiring_soon`, `ssl_expired`, `health_check_failed` and `health_check_recovered` (`notification.go:28-43`), but nothing emits them.
- **Disk-full is only detected when someone runs `tako doctor`.** It warns at 85% and fails at 95% (`cmd/doctor.go:394-398`).
- **Certificate expiry is invisible.** Only DNS-01 renewal failures are recorded, as state events. Caddy-managed (HTTP-01) certificates are inspected only on demand, by `tako domains status` through v0.14.2's `/v1/certs/status`; nothing watches them.
- **`docs/RESOURCE-MONITORING.md` lists "Alerting rules and notifications" as planned** and suggests a hand-written alert script. (Its false network and disk I/O claims are fixed by `add-server-status-history`.)

Operators on their own VPS need to find out that the site is down, the disk is filling, a container is crash-looping or a certificate is about to expire, without keeping a terminal open.

## What Changes

- **Service checks run inside takod.**
  - A service with `monitoring.enabled: true` is checked on its own node at the configured `interval`.
  - **`http`** checks request the service's health path through the node's local proxy, using the real domain for SNI and Host.
  - **`container`** checks compare running and healthy local replicas against the desired assignments on that node.
  - Checks pause while an operation lease is held, during a post-deploy grace period, and while the service is in maintenance mode.
- **Generic alert sources.** The takod alert engine gets a pluggable source API. It covers:
  - level observations, which open an episode and later recover;
  - one-shot event observations, with a cooldown;
  - closed reason and detail vocabularies, flap damping, and a transition rate limit.

  Jobs and backups keep their current storage, code path and behavior; the new manager is separate and shares only stateless helpers. New kinds live in a separate storage tree, so a takod downgrade cannot be broken by them. The sibling changes `add-local-self-healing` (`selfheal`), `add-disk-hygiene-defaults` (`hygiene`) and `add-server-status-history` (`node`) register their own kinds through this API; design D13 is the shared contract (kinds, subjects, events, capabilities, config, order).
- **New alert kinds from takod.**
  - **Host thresholds** for disk and inode usage (`/` and a separate Docker data root), memory, CPU and load. They read samples through a host-sample interface: the native collector from `add-server-status-history` when present, otherwise the legacy `current.json`. Defaults: disk and inodes on at 90%; memory, CPU and load off until set. A stale sample raises a warning.
  - **Container OOM kills and crash loops.** These are detection only; acting on them belongs to `add-local-self-healing`.
  - **Certificate expiry** of the certificate the proxy actually serves for each routed domain: the takod store entry (pushed and DNS-01) when present, otherwise Caddy's HTTP-01 certificate, using the same lookup as v0.14.2's `/v1/certs/status`. Warning at 14 days, critical at 3 days, and expired.
- **Stable alert payload, `schemaVersion: 1`.** Every delivery carries:
  - the existing keys;
  - additively, `schemaVersion`, `event` (reusing the `pkg/notification` event names, e.g. `service_down`, `high_disk`), `node`, `severity` and `subject`;
  - a `details` map with an allowlisted set of keys.

  Free-text error strings are never sent; failure details are enum codes and numbers.
- **Config.**
  - New optional blocks: `notifications.alerts.services`, `.host`, `.containers` and `.certificates`.
  - `monitoring` gains `alertAfter`, `recoverAfter` and `timeout`.
  - `monitoring.webhook` becomes optional when `notifications` has any target. When set, it is an extra webhook target for that service only, validated with the same SSRF rules. An existing value that fails them is a deprecation warning, not an error, and is never sent to takod.
  - The config is delivered on `deploy` through the existing `PUT /v1/alerts`, behind the new takod capability `monitoring.health-v1`. Deploy fails before mutating anything when the config *explicitly* sets a new key and a target node lacks the capability. Sections that exist only through defaults (an existing `alerts:` block or `monitoring.enabled: true`) are sent only to capable nodes, with a warning for the others, so upgrading the CLI never breaks a working deploy.
- **`tako monitor` becomes a viewer.**
  - **Default:** a single read of takod check and alert status from every server in the environment.
  - **`--watch`:** refreshes the view; human mode only.
  - **`--local [--once]`:** keeps a fixed version of the foreground checker, which honors intervals, reports the real node, sends recoveries and uses the validated targets. It is useful as an outside vantage point that can detect a whole-host outage.
  - **Machine output:** `tako monitor --output json` returns a `MonitorResult`, and `monitor` moves from "human-only" to "full contract". Exit code 6 when any subject is alerting or a node could not be read.
- **Observability.** `GET /v1/alerts` status and `tako prometheus` gain additive per-subject fields and metrics for the new kinds.
- **Docs.**
  - `docs/RESOURCE-MONITORING.md` gets a new alerting section. Which metrics it may claim (network and disk I/O) is owned by `add-server-status-history`; this change does not edit those claims.
  - New monitoring sections in `docs/CONFIGURATION.md`.
  - `docs/MACHINE-INTERFACE.md` documents the `MonitorResult`, the payload schema and the status fields.
- **Behavior change, not breaking:**
  - Running `tako monitor` with no flags no longer loops forever sending webhooks; it prints status once. The old loop is `tako monitor --local`. The deprecated `--once` keeps its old meaning (one local check pass that alerts) as an alias of `--local --once`.
  - Existing job and backup alert payloads gain the additive keys above. No existing key changes.

## Capabilities

### New Capabilities
- `health-monitoring`: takod-run service checks, host, container and certificate alerts, the alert source API and payload schema, and the `tako monitor` viewer.

### Modified Capabilities
- `documentation-accuracy`: the resource monitoring guide documents takod alerting as implemented. Which metrics it may claim stays owned by `add-server-status-history`.

## Impact

- **takod:**
  - `pkg/takod/alerts.go`: the v1 payload builder and the stateless delivery helpers are shared; the job and backup episode logic and format are unchanged.
  - New files: `alert_sources.go`, `monitor_checks.go`, `host_alerts.go` (with the `HostSampleSource` interface and legacy adapter), `container_alerts.go` (shared container sampler) and `cert_expiry.go` (reusing v0.14.2's `proxy_cert_status.go` helpers).
  - `server.go`: start the loops, add the capability.
  - `lifecycle_gate.go`: validate the extended PUT.
  - `cleanup.go`: remove source alert state with the environment.
- **Config:** `pkg/config/types.go` and `validator.go` (monitoring and alerts blocks), plus `schema/tako.schema.json`.
- **Deployer:** `pkg/deployer/alerts.go` builds the extended request and runs the capability preflight. The checker node is assigned there.
- **CLI:** `cmd/monitor.go` is rewritten, `pkg/monitoring/monitor.go` is fixed for `--local`, `cmd/prometheus.go` gains metrics, and `cmd/machine_coverage_test.go` is updated.
- **Shared vocabulary:** the `pkg/notification` event constants are reused by takod.
- **Docs:** `docs/RESOURCE-MONITORING.md`, `docs/CONFIGURATION.md`, `docs/MACHINE-INTERFACE.md`, and the generated man pages and completions.
- **Siblings** (contract in design D13; this change lands first):
  - `add-local-self-healing` consumes the shared container sampler defined here (D7) and reports through the source API (D1). It lands last.
  - `add-disk-hygiene-defaults` and `add-server-status-history` report through the source API.
  - `add-server-status-history` owns the metrics collector, the metric claims in the docs and the removal of the unused `pkg/monitoring/agent*.sh`. It plugs its native collector into this change's `HostSampleSource`; until then this change reads `current.json` through the legacy adapter.
- **Out of scope:**
  - Detecting that a whole host is down (it cannot report on itself). Use `tako monitor --local` from another machine, or an external uptime check.
  - Peer heartbeats between nodes.
  - Automatic remediation.
