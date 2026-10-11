# Design

## Context

- **takod already has a durable alert engine** (`pkg/takod/alerts.go`). It is scoped per `(project, environment)` and has:
  - subjects (`kind/service`) with episodes: `opening`, then `recovery`;
  - a bounded delivery queue (1024 entries; 3 attempts; exhausted deliveries pruned first);
  - credential generations, so changed targets close episodes on the old targets;
  - an SSRF-safe HTTP client (`alerttarget.IsPublicIP`);
  - persistence under `<dataDir>/scheduler-alerts/{config,credentials,state}`.
- **The engine's loops:** `Run` evaluates every 30s and dispatches every 1s (`alerts.go:200-217`).
- **Hardcoded vocabulary:** subject kinds are fixed to `job` and `backup` (`validAlertSubject`, `alerts.go:1164`), and reasons to five codes (`validAlertReason`, `:1176`).
- **The loader is strict.** Any unknown kind, reason or schema version fails `LoadContext` (`alerts.go:1588-1634`).
- **How config arrives:** the CLI pushes it on every deploy (`pkg/deployer/alerts.go`, called from `pkg/engine/deploy.go:705,1054`) through `PUT /v1/alerts`. Schedulable environment servers get the policy and the other servers get a zero (disable) policy.
- **Gating:** enabling requires `scheduler.alerts-v1` (preflight at `deployer/alerts.go:15-31`). On enrolled nodes the PUT is fenced by `lifecycle_gate.go:149-153,229-234`.
- **takod accepts unknown JSON fields.** `decodeJSONRequestWithLimit` uses no `DisallowUnknownFields`. An older takod therefore silently ignores new policy fields, so new features must be capability-gated.
- **Desired state on each node** (`<dataDir>/desired/<p>/<e>/revision.json`, `takodstate.DesiredRevision`) carries per-service `Replicas`, `Assignments` (`{slot, nodeId, node}`), `HealthCheck`, `Domains`, `Proxy` and `Restart`. It does not carry `monitoring`.
- **Actual state:** `GatherActualState` (`pkg/takod/actual.go`) reads containers with `docker ps` and reports running replicas, active and warming containers, and worst-wins `Health`. `runActualRefreshLoop` (30s) skips environments whose operation lease is held (`actual_refresh.go:73-77`).
- **Host metrics:** the bash agent rewrites `/var/lib/tako/metrics/current.json` every 60s (`provisioner.go:1758-1775`). It contains `timestamp`, `cpu_percent` (string), `memory{total_mb,used_mb,available_mb,percent(string)}`, `disk{...percent(string)}` for `/`, `uptime_seconds`, and `load_average{1min,5min,15min}` (strings). `takod.ReadNodeMetrics` reads it (`metrics.go:23-46`).
- **Certificates:**
  - takod's store at `/var/lib/tako/certs` keeps `NotAfter` in its metadata (`proxy_certs.go:51,658`), for both pushed and DNS-01 certificates.
  - Caddy's HTTP-01 and on-demand certificates live under `proxyCaddyDataDir` = `/etc/tako/proxy/caddy-data` (`proxy_routes.go:37`), mounted at `/data`. The layout is `caddy/certificates/<issuer>/<domain>/<domain>.crt`.
  - Route manifests (`readProxyRouteManifests`) map domains to a project and environment.
- **Maintenance mode** writes a proxy config named by `runtimeid.MaintenanceProxyConfigFileName(project, env, service)`.

## Goals / Non-Goals

**Goals**
- Service, host, container and certificate alerts that run on the node with no terminal open, with deduplicated `opening` and `recovery` deliveries.
- One alert source API the sibling changes reuse.
- A stable, versioned, credential-free payload.
- `tako monitor` becomes a correct viewer, with a machine contract.
- Fail closed on old nodes, and stay safe if takod is downgraded.

**Non-Goals**
- Remediation (restarts, recreation, cleanup): siblings.
- Metrics history, collector replacement, OS updates and scheduled doctor runs: `add-server-status-history`.
- Detecting that a whole host is down, peer heartbeats, external probes, paging integrations beyond Slack, Discord and webhooks, and arbitrary user-defined alert rules.

## Decisions

### D1. Alert source API (shared with siblings)

New file `pkg/takod/alert_sources.go`. Sources are in-process takod components. They never talk to HTTP targets themselves; they report observations to the `SourceAlertManager`.

```go
// AlertSourceSpec is registered once at takod startup.
type AlertSourceSpec struct {
    Kind        string            // subject kind, e.g. "service", "host", "container", "certificate"
    Reasons     map[string]Severity // closed reason vocabulary -> default severity
    DetailKeys  []string          // allowlisted detail keys (values: enum tokens or numbers)
    Events      map[string]string // reason -> notification event name for opening, e.g. "unreachable" -> "service_down"
    Recoveries  map[string]string // reason -> event name for recovery, e.g. "unreachable" -> "service_up"
    NodeScoped  bool              // true: subject is node-global (host); fanned out to subscribing scopes (D6)
}

type Severity string // "warning" | "critical"

// LevelObservation is a level-triggered sample. A subject is failing when
// len(Failing) > 0. Sources report every evaluation; omission for longer
// than 3x the subject's interval marks the subject "stale" (no transition).
type LevelObservation struct {
    Project, Environment string // empty for NodeScoped kinds
    Kind, Subject        string // subject: service name, domain, "root" for host
    Failing              []string          // reason codes from the spec
    Details              map[string]string // allowlisted keys only
    Damping              Damping           // per subject; zero = defaults
    ObservedAt           time.Time
}

// EventObservation is a one-shot occurrence (e.g. one OOM kill). It is
// delivered once per (subject, reason) per Cooldown (default 15m); further
// occurrences inside the cooldown increment a counter carried in the next delivery.
type EventObservation struct {
    Project, Environment, Kind, Subject, Reason string
    Details    map[string]string
    Cooldown   time.Duration
    ObservedAt time.Time
}

type Damping struct {
    OpenAfter    int           // consecutive failing observations before opening (default 1)
    OpenFor      time.Duration // OR failing continuously this long (default 0)
    RecoverAfter int           // consecutive passing observations before recovery (default 1)
}

func (m *SourceAlertManager) RegisterSource(spec AlertSourceSpec) error
func (m *SourceAlertManager) ReportLevel(LevelObservation) error
func (m *SourceAlertManager) ReportEvent(EventObservation) error
func (m *SourceAlertManager) Subscribed(project, environment, kind string) bool // policy says this kind is enabled for the scope
func (m *SourceAlertManager) Status(project, environment string) (SourceAlertStatus, error)
```

- **Validation:** an unknown kind, an unregistered reason or a non-allowlisted detail key is rejected. A detail value must match `^[A-Za-z0-9._:@/-]{1,128}$`, or be a number. A report for an unsubscribed scope or kind is dropped silently.
- **Reserved kinds:**
  - here: `service`, `host`, `container`, `certificate`;
  - for siblings: `selfheal` (`add-local-self-healing`), `disk` (`add-disk-hygiene-defaults`) and `node` (`add-server-status-history`). Siblings define their own reasons and detail keys in their own specs and register them through `RegisterSource`.
- **Shared engine:** the episode and delivery engine of `AlertManager` is extracted into an internal `alertCore` used by both managers. That covers queueing, dispatch, credential generations, retries, the SSRF client and payload building. `AlertManager`'s job and backup behavior and its persisted format stay byte-compatible.

### D2. Episodes, damping and flap control

- **Level subjects:** a subject opens when `OpenAfter` consecutive failing observations are seen, or it has failed continuously for `OpenFor`. It recovers after `RecoverAfter` consecutive passing observations.
  - Reasons are merged while the subject is open; the opening delivery lists the reasons at open time.
  - A reason added while open is not a new episode. It appears in `details.addedReasons` of the eventual recovery and in status.
- **Severity:** the highest severity among the subject's active reasons. An escalation from `warning` to `critical` while open (for example a certificate going from 14 to 3 days) queues one `escalation` transition. Escalation is the only non-opening/recovery level transition.
- **Flap control:** at most 6 transitions per subject per rolling hour. When the limit is hit, the manager queues one `suppressed` delivery and marks the subject `flapping`. It delivers nothing more until the subject has been stable (passing or failing) for 30 minutes, then sends the current state (opening or recovery) once.
- **Events:** a one-shot `event` transition, cooldown-deduplicated per `(subject, reason)`. The counter of suppressed repeats is included as `details.repeats`.
- **Transitions:** the full set is `opening | recovery | escalation | event | suppressed`. Job and backup subjects keep `opening | recovery`.

### D3. Payload schema v1 (all deliveries, including jobs and backups)

The webhook body (Slack and Discord still receive only `message`, as today):

```json
{
  "schemaVersion": 1,
  "eventId": "…", "episodeId": "…", "transition": "opening",
  "event": "service_down",
  "project": "shop", "environment": "production", "node": "app-1",
  "kind": "service", "subject": "web", "service": "web",
  "severity": "critical",
  "reasons": ["unreachable"],
  "details": {"checkType": "http", "errorClass": "connect", "consecutiveFailures": "3", "domain": "shop.example.com"},
  "openedAt": "2026-10-11T03:00:00Z",
  "message": "Tako alert opening for shop/production service web on app-1: unreachable",
  "timestamp": "2026-10-11T03:00:30Z"
}
```

- Existing keys keep their meaning: `eventId`, `episodeId`, `transition`, `project`, `environment`, `kind`, `service`, `message`, `reasons` and `timestamp`.
- `service` is kept as an alias of `subject` for compatibility.
- `event` uses the `pkg/notification` EventType vocabulary. Job and backup events: `job_alert` and `backup_stale`, which are new constants.
- `message` is built only from public names, reason codes and numbers. It is never built from error strings, response bodies, headers or environment values, so no redaction pass is needed. Closed vocabularies replace redaction.

### D4. Config and delivery to takod

```yaml
notifications:
  slack: ${SLACK_WEBHOOK}
  alerts:
    jobs: {...}            # unchanged
    backups: {...}         # unchanged
    services:              # defaults for services with monitoring.enabled
      alertAfter: 3        # consecutive failed checks before alerting (default 3)
      recoverAfter: 2      # consecutive passed checks before recovery (default 2)
    host:
      disk: 90             # percent of / used; 0 disables (default 90 when alerts block exists)
      memory: 0            # percent; default off
      cpu: 0               # percent, sustained for cpuFor; default off
      cpuFor: 10m
      load: 0              # 5-min load per core; default off
    containers:
      oom: true            # default true
      crashLoop: 3         # restarts within crashLoopWindow; 0 disables (default 3)
      crashLoopWindow: 10m
    certificates:
      warnBefore: 336h     # 14 days (Go duration; default 336h)
      criticalBefore: 72h  # default 72h
services:
  web:
    monitoring:
      enabled: true
      interval: 60s        # 10s..1h, default 60s (now honored)
      timeout: 10s         # 1s..60s, default 10s, must be < interval
      alertAfter: 3        # overrides notifications.alerts.services
      recoverAfter: 2
      checkType: http      # http | container (auto-detected as today)
      webhook: ${WEB_ALERT_HOOK}   # optional extra target for this service only
```

- **Defaults:** they apply only when a `notifications.alerts` block exists, as job and backup alerts do today. A config without `alerts:` sends nothing new. The exception is `monitoring.enabled` services, which are monitored whenever they have any target: a notifications target or `monitoring.webhook`.
- **Validation** (`validator.go`):
  - `monitoring.webhook` is required only when `notifications` has no slack, discord or webhook target. This relaxes `validator.go:1126-1128`.
  - Every target, including per-service ones, must pass `alerttarget.NormalizeAndValidate` at config load. This catches an `http://10.0.0.5` target before deploy.
  - Durations must be within the bounds above, and percents within 1..99.
  - Monitoring on `kind: run`/`job` services stays rejected.
- **Request:** `takod.AlertConfigRequest` gains additive fields:
  - `Monitors []MonitorSpec` with `{Service, CheckType, Interval, Timeout, AlertAfter, RecoverAfter, Domain, HealthPath, CheckerNode, HasWebhook}`;
  - `Host *HostAlertPolicy`, `Containers *ContainerAlertPolicy`, `Certificates *CertificateAlertPolicy`;
  - `Targets.Services map[string]string`, write-only like the other targets.
- **Persistence:** these fields are persisted by the new `SourceAlertManager` under a separate tree, `<dataDir>/alerts/{config,credentials,state}`. The legacy `scheduler-alerts` tree keeps `schemaVersion: 1` and only `job`/`backup` subjects.
- **Why a separate tree:** a downgraded takod never sees the new kinds and cannot fail its strict loader (`alerts.go:1588-1634`). `alertPolicyEnabled` is extended so a policy with only new sections is "enabled".
- **Disabling:** a zero policy still disables and removes both trees for the scope. `cleanup` (`server.go:1359,1388`) removes both trees.
- **Per-service targets** are deliveries with provider `service-webhook`. They go only to subjects of that service: kind `service`, and `container`/`selfheal` subjects whose subject is that service.
- **Checker node** (http checks only):
  - The CLI assigns `CheckerNode` deterministically: the first, by sort order, of the schedulable environment servers that host a replica of the service. If none hosts one, the first schedulable server.
  - Each node runs only the http monitors whose `CheckerNode` is itself, so one service gets one http alert stream.
  - Container checks run on every node, for that node's own assignments.

### D5. Service checks (`pkg/takod/monitor_checks.go`)

- **Loop:** one goroutine per scope. It schedules each monitor at its `Interval`, with up to 10% jitter, and caps concurrency at 4 checks per node.
- **`http` check:**
  - It dials the node's local proxy at `127.0.0.1:443` and uses `Domain` for both SNI and Host. It verifies TLS against system roots for `Domain`, issues `GET HealthPath`, does not follow redirects, and reads at most 64 KiB of the body before discarding it.
  - **Success** is a 2xx status within `Timeout`.
  - **Failure reasons:**
    - `unreachable`, with `errorClass` = `connect|tls|timeout`;
    - `unhealthy_status`, with details `statusCode` and `errorClass=status`;
    - `tls_invalid`, for a certificate error.
  - This approach avoids hairpin NAT and needs no DNS. It does not prove public DNS, which `tako domains status` covers.
  - A service with `proxy.basicAuth` is checked without credentials, so a 401 counts as success when basic auth is configured.
- **`container` check:**
  - Expected count: the number of `Assignments` for this node in the local desired revision, or 1 for a `global` placement.
  - Actual count: running active containers from `GatherActualState`.
  - **Reasons:**
    - `replicas_missing`, with details `expected` and `running`;
    - `unhealthy`, when Docker health is `unhealthy` for any active container.
  - A node with no assignment for the service reports nothing.
- **Pauses:** a subject is `paused`, with no observations and no transitions, while:
  - the scope's operation lease is held (`stateOperationLeaseHeld`, as in actual refresh);
  - the post-lease grace period is running: `max(healthCheck.startPeriod, 30s)` after the lease is released;
  - for http checks, the service's maintenance proxy config exists;
  - the service's desired entry has `RemovalPending`.
- **Damping:** `OpenAfter = alertAfter`, `RecoverAfter = recoverAfter`.

### D6. Host thresholds (`pkg/takod/host_alerts.go`)

- **Source:** every 60s, parse `current.json`. String percents are parsed as floats, and a parse error counts as stale.
- **Reasons and severities:**
  - `disk_high`: critical at 95% or more, otherwise warning.
  - `memory_high`: warning.
  - `cpu_high`: uses `OpenFor = cpuFor`; warning.
  - `load_high`: `load_average.5min / nproc`; warning.
  - `metrics_stale`: the file is missing or its timestamp is more than 5 minutes old; warning.
  - Details carry `percent`, `threshold`, `usedMb` and `totalMb`.
- **Fan-out:** host subjects are `NodeScoped`. They are evaluated once per node per subscribing policy, and each scope with a `host` section gets its own episode against its own thresholds.
- **Deduplication across scopes:** a node-global ledger keyed by `(node, reason, transition, targetFingerprint)` suppresses an identical delivery to the same endpoint from another scope within the same open window. `targetFingerprint` is the SHA-256 of provider plus normalized URL. The ledger is persisted in the new tree. Two environments of one project that share a server and a Slack webhook therefore produce one message, not two.

### D7. Container OOM and crash-loop detection (`pkg/takod/container_alerts.go`)

- **Sampling:** every 30s, for each running or restarting container labeled with a subscribed scope, read `docker inspect` for `RestartCount`, `State.OOMKilled`, `State.StartedAt`, `State.Status` and `State.ExitCode`. Docker is called once per batch, at most 64 containers per call.
- **`container_oom`:** an event observation when `OOMKilled` is true and the `StartedAt` (or restart count) differs from the last sample. Details: `container`, `exitCode`, `memoryLimitMb` (when set).
- **`crash_loop`:** a level observation when restarts inside `crashLoopWindow` reach `crashLoop`, or the status is `restarting` for 3 consecutive samples. It recovers after the container has been running 2 windows without a restart. Details: `restarts`, `window`, `exitCode`.
- **Exported for siblings:** the samples are published in memory through `ContainerObservations() map[containerID]ContainerSample`, read-only. `add-local-self-healing` consumes them. This change performs no remediation.

### D8. Certificate expiry (`pkg/takod/cert_expiry.go`)

- **Frequency:** every 6h, and once 2 minutes after start.
- **Sources:**
  - takod store entries (`loadProxyCertificateEntries(false)`), skipping orphaned entries;
  - Caddy's store: walk `proxyCaddyDataDir/caddy/certificates/*/*/*.crt`, without following symlinks and with a 1 MiB file cap, and parse the leaf `NotAfter`.
- **Duplicates:** when both stores hold a domain, the later `NotAfter` wins, since that is the certificate actually served after renewal.
- **Ownership:** a domain maps to a project and environment through the route manifests. Unowned certificates are ignored.
- **Reasons:**
  - `cert_expiring`: within `warnBefore`; warning. Escalates to critical within `criticalBefore`.
  - `cert_expired`: critical.
  - Details: `domain`, `expiresAt` (RFC 3339), `daysLeft`, `source` (`caddy|acme-dns|pushed`).
  - Event names: `ssl_expiring_soon` and `ssl_expired`; on recovery, `ssl_renewed`.
- **Rationale:** Let's Encrypt certificates are normally renewed with about 30 days left. Reaching 14 days means renewal has been failing for about two weeks, which makes this a high-signal alert.

### D9. `tako monitor` rework

- **`tako monitor`:** reads `GET /v1/alerts?project&environment` on every environment server in parallel, with a 10s timeout per node. It prints one table:

  | Column | Values |
  |---|---|
  | `KIND` | |
  | `SUBJECT` | |
  | `NODE` | |
  | `STATE` | `ok`, `alerting`, `paused`, `maintenance`, `flapping`, `stale`, `unknown` |
  | `SEVERITY` | |
  | `SINCE` | |
  | `LAST CHECK` | |
  | `REASONS` | |

  It also prints a footer with pending and exhausted deliveries. `--service` filters to one service.
- **Exit codes:** 0 when everything is ok; 6 when any subject is alerting or flapping, or any node read failed (the document is still emitted); 4 when every node is unreachable; 2 when no target node has `monitoring.health-v1`. In that last case it prints a hint to run `tako upgrade servers`, or to use `--local`.
- **`--watch [duration]`:** default 5s. Human mode only; rejected in machine modes, as `drift --watch` is.
- **`--once`:** accepted, hidden and deprecated. It prints a stderr notice that it is now the default.
- **`--local`:** the legacy foreground checker, fixed:
  - per-service intervals;
  - the real node name (or `external`);
  - `opening` and `recovery` with the same damping defaults;
  - the payload schema v1;
  - targets validated by `alerttarget`, using the SSRF-safe transport for delivery.

  It checks the public URL (`https://domain/path`) from the CLI host, so it is a genuine outside vantage point. Human only.
- **Machine output:** `--output json` returns a `MonitorResult`:

  ```json
  {"apiVersion":"tako.redentor.dev/v1alpha1","kind":"MonitorResult","project":"shop","environment":"production",
   "status":"attention","counts":{"ok":4,"alerting":1,"paused":0,"unknown":0},
   "nodes":[{"node":"app-1","reachable":true,"subjects":[{"kind":"service","subject":"web","state":"alerting","severity":"critical",
     "reasons":["unreachable"],"since":"…","lastCheckedAt":"…","details":{"errorClass":"connect"}}],"pendingDeliveries":0,"exhaustedDeliveries":0}]}
  ```

- **Machine events:** `--events ndjson` emits a single `result`. `monitor` moves to the "Full contract" row of the command coverage table.

### D10. Status endpoint and Prometheus

- **`GET /v1/alerts`:** the response gains an additive `sources` object: `{evaluatedAt, subjects:[{kind, subject, node, state, severity, reasons, since, lastObservedAt, details, paused, flapping}], deliveries:[...]}`. `GET /v1/metrics` includes it inside each `alerts[]` entry, also additively. Targets are never returned.
- **`tako prometheus` adds:**
  - `tako_monitor_subject_active{project,environment,node,kind,subject,severity}`
  - `tako_monitor_subject_last_observed_timestamp_seconds{...}`
  - `tako_monitor_pending_deliveries{project,environment,node}`
  - `tako_monitor_exhausted_deliveries{...}`
  - `tako_host_metrics_stale{node}`

### D11. Capability, rollout and compatibility

- **New capability:** `monitoring.health-v1`.
- **Deploy preflight** (`PreflightAlertPolicy`): when the request carries any of `Monitors`, `Host`, `Containers`, `Certificates` or `Targets.Services`, every schedulable target server must advertise the capability. Otherwise deploy fails with exit 2 before mutating anything, with a message naming the servers and pointing to `tako upgrade servers`.
- **Disable path:** stale servers that lack the capability are skipped, as today (`deployer/alerts.go:75-82`).
- **Lifecycle gate:** `lifecycle_gate.go` validates the extended request with the same normalizer.
- **Older CLI against a newer takod:** the request has no new fields, so the source tree for that scope is cleared. This matches the existing semantics: the last deploy wins.

### D12. Safety: SSRF, secrets and resource limits

- All deliveries go through the existing SSRF-safe client, with public IPs only and no redirects. Per-service targets now get the same treatment, which fixes `pkg/monitoring/monitor.go:309-313`.
- http checks dial only `127.0.0.1:443`. The configured domain sets SNI and Host, and is never used to dial elsewhere.
- Payload, status and `MonitorResult` fields come only from closed vocabularies, public names and numbers (D3). Raw error text is mapped to `errorClass` before it leaves the check function. Unit tests assert that no field contains an error string.
- **Resource limits:** at most 4 concurrent checks per node, at most 64 KiB read per response, at most 200 monitored services per scope (validation error above that), and delivery queue caps unchanged. When `requireFreeDisk` fails, state is kept in memory and persisted later, and observations continue.

## Risks / Trade-offs

- **Local-proxy http checks miss DNS and upstream network faults.** This is accepted and documented. `--local` and external probes cover outside-in checks.
- **Host alerts on by default (disk 90%) for users who already have an `alerts:` block** may surprise some. This is mitigated by release notes and `host.disk: 0`. Disk-full is the most damaging silent failure, so on by default is the safer default.
- **Per-scope host episodes multiply internal state** (subjects x scopes). This is bounded by the number of scopes on a node. The ledger deduplicates outward noise.
- **Reading Caddy's store relies on its on-disk layout.** That layout is stable across Caddy 2.x. If the walk fails, the source logs an error and keeps the takod-store results.
- **Refactoring `AlertManager` into `alertCore` risks job and backup alert regressions.** The existing `alerts_test.go` suite must pass unchanged, and a golden test pins the persisted format.

## Migration Plan

1. Ship takod with `monitoring.health-v1`, the source manager and the extended `/v1/alerts`. With old CLIs, nothing changes.
2. Operators run `tako upgrade servers`. The next `tako deploy` with a new CLI pushes the policies.
3. `tako monitor` users:
   - The default changes from an endless loop to a status read.
   - Scripts that relied on the loop should deploy (takod now does the checking) or switch to `tako monitor --local`.
   - The release notes call this out.
4. Downgrading takod is safe: the `alerts/` tree is ignored by older binaries, and job and backup alerts keep working.

## Assumptions

- Each server runs a `tako-proxy` that serves the domains of services hosted on it (true for the current proxy model, `proxy_routes.go`).
- `nproc` is available to takod through `runtime.NumCPU()` on the host. takod runs on the host, not in a container.

## Open questions

1. **Should host and container alerts be on by default when an `alerts:` block exists?** *Recommendation: yes for `disk` (90%), `oom`, `crashLoop` and certificates; no for cpu, memory and load.* The defaults catch silent failures; the noisy ones stay opt-in.
2. **Should the http check go through the local proxy or the public URL from the node?** *Recommendation: the local proxy.* Hairpin NAT breaks public-URL checks on many VPS providers. Public reachability is covered by `--local` and by external monitors.
3. **Which node runs a service's http check: one checker node or every proxy node?** *Recommendation: one deterministic checker node.* One alert stream per service. Multi-node proxy coverage can be added later as an opt-in.
4. **Should `tako monitor` with no flags keep the endless loop for compatibility?** *Recommendation: no, it becomes a status read; `--local` keeps the loop.* The old loop was buggy and needs a terminal. The new default is safe to script.
5. **Should host alerts deduplicate across scopes through a target-fingerprint ledger, or use one node-global policy?** *Recommendation: the ledger.* Each project keeps its own thresholds, and identical endpoints still get one message.
6. **Should new alert state live in a new storage tree or bump `scheduler-alerts` to schema 2?** *Recommendation: a new tree.* Downgrade safety outweighs the duplicated credential documents.
7. **Should job and backup payloads gain the v1 keys?** *Recommendation: yes, additively.* One schema for webhook consumers; existing keys are untouched.
8. **Should certificate alerts escalate to a separate `critical` delivery?** *Recommendation: yes, one `escalation` transition.* "3 days left" deserves a second notice.
