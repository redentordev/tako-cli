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
- **Config layout:** services live under `environments.<env>.services.<name>`; `notifications` (targets and `alerts`) is top-level and project-wide. `services.<name>.monitoring.webhook` is not validated today (`validator.go:1121-1142`): existing configs may hold `http://` or private URLs.
- **Certificate status (v0.14.2):** `pkg/takod/proxy_cert_status.go` adds `ProxyCertificateStatus`, `latestCaddyCertificate` and `readCaddyLeafCertificate` (Lstat, regular files only, 1 MiB cap). It consults the takod store first, because pushed and DNS-01 certificates take routing precedence, and then Caddy's storage, including covering wildcards.
- **Proxy placement:** `environments.<env>.proxy.placement` can put a service's routes on servers that host none of its replicas.
- **Locks:** the snapshot mutation lock and the maintenance barrier are *shared* flocks (`pkg/recovery/lock_unix.go`); they do not exclude other mutators. Per-environment exclusion is the operation lease, and on enrolled workers the worker fence admitted under `workerFenceAdmissionMu`.

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
- **Subjects:** a service-bound kind uses the service name as subject. A subject that is not a service (node-wide or scope-wide) starts with `@` (for example `@node`). Service names only contain `[A-Za-z0-9_-]`, so the two can never collide. Host subjects are filesystem names (`root`, `docker`), certificate subjects are domains, node-condition subjects are condition types.
- **Severity** is only `warning` or `critical`. There is no `info` alert: purely informational facts (a successful self-heal restart, a reclaim that freed space) go to journals and state events, never to alert targets.
- **Kinds** (the full cross-plan table is D13): `service`, `host`, `container` and `certificate` here; `selfheal` (`add-local-self-healing`), `hygiene` (`add-disk-hygiene-defaults`) and `node` (`add-server-status-history`) are reserved for the siblings, which define their own reasons and detail keys and register them through `RegisterSource`. Each kind is registered by exactly one source.
- **No shared-engine refactor.** `SourceAlertManager` is a new, separate implementation. It reuses only the stateless pieces of `alerts.go`: the SSRF-safe client (`newAlertHTTPClient`), `deliverHTTP`, target normalization and validation, and the v1 payload builder (D3). `AlertManager`'s episode logic, loader and persisted `scheduler-alerts` format are not touched, so job and backup alerts cannot regress through this change; only their payload gains the additive v1 keys.

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
- `event` uses the `pkg/notification` EventType vocabulary. Job and backup events: `job_alert` and `backup_stale`, which are new constants. Each sibling adds its own constants (D13).
- Job and backup deliveries keep their current `message` text byte-for-byte; only the new kinds use the `… on <node>` form.
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
    host:                  # also subscribes the scope to node-wide kinds (D13)
      disk: 90             # percent of a filesystem used (/ and a separate Docker data root); 0 disables (default 90 when alerts block exists)
      inodes: 90           # percent of inodes used, same filesystems; 0 disables (default 90); evaluated only when the sample has inode data
      memory: 0            # percent; default off
      cpu: 0               # percent, sustained for cpuFor; default off
      cpuFor: 10m
      load: 0              # 5-min load per core; default off
      conditions: true     # node-condition alerts from add-server-status-history; default true
    containers:
      oom: true            # default true
      crashLoop: 3         # restarts within crashLoopWindow; 0 disables (default 3)
      crashLoopWindow: 10m
    certificates:
      warnBefore: 336h     # 14 days (Go duration; default 336h)
      criticalBefore: 72h  # default 72h
environments:
  production:
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
- **Explicit versus default sections.** The request records which sections came from explicit keys. A section is explicit when the config sets `notifications.alerts.services|host|containers|certificates` or any of `monitoring.timeout|alertAfter|recoverAfter`. Sections that exist only because of defaults (an `alerts:` block for jobs, or a pre-existing `monitoring.enabled: true`) are *default-derived*. D11 treats the two differently so that upgrading the CLI never breaks an existing config.
- **Validation** (`validator.go`):
  - `monitoring.webhook` is required only when `notifications` has no slack, discord or webhook target. This relaxes `validator.go:1126-1128`.
  - New targets are validated with `alerttarget.NormalizeAndValidate` at config load and are errors when invalid. `notifications` targets already are.
  - An existing `monitoring.webhook` that fails `alerttarget` (for example `http://10.0.0.5`) is a validation **warning** for one deprecation cycle (`ValidationWarnings`, until `DeprecatedKeysRemovalRelease`), not an error, because it was accepted before. Such a target is never sent to takod and never used by `--local`; the warning says so. It becomes an error at the removal release.
  - Durations must be within the bounds above, and percents within 1..99.
  - Monitoring on `kind: run`/`job` services stays rejected.
- **Request:** `takod.AlertConfigRequest` gains additive fields:
  - `Monitors []MonitorSpec` with `{Service, CheckType, Interval, Timeout, AlertAfter, RecoverAfter, Domain, HealthPath, CheckerNode, HasWebhook}`;
  - `Host *HostAlertPolicy`, `Containers *ContainerAlertPolicy`, `Certificates *CertificateAlertPolicy`;
  - `Targets.Services map[string]string`, write-only like the other targets.

  The explicit/default distinction is CLI-side only (it decides preflight and which nodes get default-derived sections); takod treats every received section the same.
- **Persistence:** these fields are persisted by the new `SourceAlertManager` under a separate tree, `<dataDir>/alerts/{config,credentials,state}`. The legacy `scheduler-alerts` tree keeps `schemaVersion: 1` and only `job`/`backup` subjects.
- **Why a separate tree:** a downgraded takod never sees the new kinds and cannot fail its strict loader (`alerts.go:1588-1634`). `alertPolicyEnabled` is extended so a policy with only new sections is "enabled".
- **Disabling:** a zero policy still disables and removes both trees for the scope. `cleanup` (`server.go:1359,1388`) removes both trees.
- **Per-service targets** are deliveries with provider `service-webhook`. They go only to subjects of that service: kind `service`, and `container`/`selfheal` subjects whose subject is that service.
- **Checker node** (http checks only):
  - The CLI assigns `CheckerNode` deterministically: the first, by sort order, of the schedulable servers the deploy writes the service's proxy route to. That is the environment `proxy.placement` targets when set, otherwise the servers hosting a replica. If that set is empty, the first schedulable server.
  - Each node runs only the http monitors whose `CheckerNode` is itself, so one service gets one http alert stream.
  - A checker node that has no proxy route manifest for `Domain` reports the subject `unknown` (reason `no_local_route` in status) and never a failing observation, so a placement mistake cannot page anyone.
  - When the checker moves (servers added or removed), the old checker retires its http subject the way removed job subjects are retired today: one `recovery` to the providers that received the opening, with `details.retired=true`.
  - Container checks run on every node, for that node's own assignments.

### D5. Service checks (`pkg/takod/monitor_checks.go`)

- **Loop:** one goroutine per scope. It schedules each monitor at its `Interval`, with up to 10% jitter, and caps concurrency at 4 checks per node.
- **`http` check:**
  - It dials the node's local proxy at `127.0.0.1:443` and uses `Domain` for both SNI and Host. It issues `GET HealthPath`, does not follow redirects, and reads at most 64 KiB of the body before discarding it.
  - **TLS:** it verifies that the served leaf covers `Domain` and is inside its validity window. It does **not** verify the chain against system roots: pushed origin certificates (for example a CDN origin CA behind a fronting proxy) are legitimately not publicly trusted and would otherwise alert forever. Expiry is the `certificate` kind's job (D8).
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
  - the post-lease grace period is running: `max(healthCheck.startPeriod, 30s)` after the later of the last observed lease release and the last write of the scope's desired `revision.json` (so an operation that started and finished between two checks still gets its grace). This includes `self-heal` leases from `add-local-self-healing`;
  - for http checks, the service's maintenance proxy config exists;
  - the service's desired entry has `RemovalPending`.
- **Damping:** `OpenAfter = alertAfter`, `RecoverAfter = recoverAfter`.

### D6. Host thresholds (`pkg/takod/host_alerts.go`)

- **Source:** every 60s the evaluator reads one sample through a `HostSampleSource` interface (`Latest() (HostSample, time.Time, error)`); it never opens `current.json` itself.
  - This change ships the *legacy adapter*, which parses `current.json` (string percents parsed as floats; a parse error counts as stale; no inode or Docker-root data).
  - `add-server-status-history` provides the *native* source (`hostmetrics.Latest()`, typed, with inodes and a separate Docker data-root filesystem). takod uses the native source whenever native collection is enabled and falls back to the legacy adapter only when it is disabled (`--metrics-interval 0`). Monitoring therefore always consumes the native collector when present, and the two never run side by side for the same node.
- **Subjects:** `root` for `/` plus the node-wide reasons (memory, cpu, load, staleness); `docker` for the Docker data root when it is a separate filesystem (native source only).
- **Reasons and severities:**
  - `disk_high`: critical at 95% or more, otherwise warning. Evaluated per filesystem subject.
  - `inodes_high`: same rule against `host.inodes`; native source only.
  - `memory_high`: warning.
  - `cpu_high`: uses `OpenFor = cpuFor`; warning.
  - `load_high`: `load_average.5min / nproc`; warning.
  - `metrics_stale`: no sample, or the newest sample is more than 5 minutes old; warning. This is the only alert for a stalled collector: `add-server-status-history` keeps its `metrics-collector` condition for status and doctor but does not publish it as an alert.
  - Details carry `percent`, `threshold`, `usedMb` and `totalMb`.
- **Ownership:** every host *alert* threshold (disk, inodes, memory, cpu, load) lives here. `add-disk-hygiene-defaults` owns reclaim *action* thresholds, which it measures itself with `statfs`; it never raises `high_disk`.
- **Fan-out:** host subjects are `NodeScoped`. They are evaluated once per node per subscribing policy, and each scope with a `host` section gets its own episode against its own thresholds.
- **Deduplication across scopes:** a node-global ledger keyed by `(kind, subject, reason, transition, targetFingerprint)` suppresses an identical delivery to the same endpoint from another scope while the first scope's episode is open. `targetFingerprint` is the SHA-256 of provider plus normalized URL. The ledger is persisted in the new tree. Two environments of one project that share a server and a Slack webhook therefore produce one message, not two.
  - A target whose opening was suppressed by the ledger counts as "did not receive the opening", so that scope's later escalation and recovery are also not sent to it. The receiver sees one coherent episode, from the scope that opened first.
  - The ledger applies to every `NodeScoped` kind (`host`, `node`, `hygiene`) and to `@`-subjects (for example the self-healing circuit breaker).

### D7. Container OOM and crash-loop detection (`pkg/takod/container_alerts.go`)

- **Sampling:** every 30s, one shared sampler lists every container labelled `tako.project` (any state) and reads `docker inspect` for ID, name, labels, `RestartCount`, `State.OOMKilled`, `State.StartedAt`, `State.FinishedAt`, `State.Status`, `State.Restarting`, `State.ExitCode`, `State.Health.Status` and `HostConfig.RestartPolicy`. Docker is called once per batch, at most 64 containers per call. The sampler runs whenever any scope subscribes to container alerts or any desired service has a self-healing policy; detection below reports only for subscribed scopes.
- **`container_oom`:** an event observation when `OOMKilled` is true and the `StartedAt` (or restart count) differs from the last sample. Details: `container`, `exitCode`, `memoryLimitMb` (when set).
- **`crash_loop`:** a level observation when restarts inside `crashLoopWindow` reach `crashLoop`, or the status is `restarting` for 3 consecutive samples. It recovers after the container has been running 2 windows without a restart. Details: `restarts`, `window`, `exitCode`.
- **Exported for siblings:** the samples are published in memory through `ContainerObservations() (map[containerID]ContainerSample, time.Time)`, read-only, with the sample time. `add-local-self-healing` consumes them instead of running its own inspect loop, so the node runs one Docker inspect batch per 30s for both features. This change performs no remediation.

### D8. Certificate expiry (`pkg/takod/cert_expiry.go`)

- **Frequency:** every 6h, and once 2 minutes after start.
- **Which domains:** the domains of the route manifests (`readProxyRouteManifests`) owned by subscribed scopes. The Caddy store is not walked; certificates for unrouted or unowned domains are ignored.
- **Which certificate:** the one the proxy actually serves, chosen exactly as v0.14.2's `ProxyCertificateStatus` does (`pkg/takod/proxy_cert_status.go`): the takod store entry when one covers the domain (pushed and DNS-01 certificates take routing precedence), otherwise Caddy's best certificate via `latestCaddyCertificate`/`readCaddyLeafCertificate` (exact name before covering wildcard, symlinks refused, 1 MiB cap). This change reuses those helpers instead of adding a second walker. "Later `NotAfter` wins" would be wrong: an expiring pushed certificate is served even when Caddy holds a fresh one, and must alert.
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
- **Configured but not deployed:** a service with `monitoring.enabled` in the local config for which no node reports a monitor is shown as `unknown` with reason `not_deployed` (deploy with this CLI to push the policy). It counts toward exit 6, so a cron job never reads "nothing configured" as "all ok".
- **Exit codes:** 0 when everything is ok; 6 when any subject is alerting, flapping, `stale` (takod stopped observing it, e.g. a wedged Docker daemon; a stale subject can never alert, so it must not read as ok), held with reason `docker_unavailable`, or `not_deployed`, or any node read failed (the document is still emitted); 4 when every node is unreachable; 2 when no target node has `monitoring.health-v1`. In that last case it prints a hint to run `tako upgrade servers`, or to use `--local`.
- **`--watch [duration]`:** default 5s. Human mode only; rejected in machine modes, as `drift --watch` is.
- **`--once`:** keeps its old meaning, one local check pass that delivers alerts, and is now an alias of `--local --once`. It is hidden and deprecated with a stderr notice. Cron jobs that run `tako monitor --once` as an outside checker therefore keep alerting instead of silently turning into a status read.
- **`--local [--once]`:** the legacy foreground checker (looping, or a single pass with `--once`), fixed:
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
- **Deploy preflight** (`PreflightAlertPolicy`), using the explicit/default split from D4:
  - **Explicit** new sections: every schedulable target server must advertise the capability. Otherwise deploy fails with exit 2 before mutating anything, naming the servers and pointing to `tako upgrade servers`. A user who wrote new keys asked for the feature and must not be silently unprotected.
  - **Default-derived** sections (an existing `alerts:` block, or a pre-existing `monitoring.enabled: true`): sent only to servers that advertise the capability. Other servers get today's request unchanged, and deploy prints one warning per server ("node-side monitoring inactive on X; run tako upgrade servers"). Upgrading the CLI therefore never turns a deploy that passed yesterday into exit 2.
- **Disable path:** stale servers that lack the capability are skipped, as today (`deployer/alerts.go:75-82`). `alertRequestEnabled` (`deployer/alerts.go:103`) is extended the same way as takod's `alertPolicyEnabled`, so a monitoring-only config is pushed as enabled.
- **Lifecycle gate:** `lifecycle_gate.go` validates the extended request with the same normalizer.
- **Older CLI against a newer takod:** the request has no new fields, so the source tree for that scope is cleared. This matches the existing semantics: the last deploy wins.

### D12. Safety: SSRF, secrets and resource limits

- All deliveries go through the existing SSRF-safe client, with public IPs only and no redirects. Per-service targets now get the same treatment, which fixes `pkg/monitoring/monitor.go:309-313`.
- http checks dial only `127.0.0.1:443`. The configured domain sets SNI and Host, and is never used to dial elsewhere.
- Payload, status and `MonitorResult` fields come only from closed vocabularies, public names and numbers (D3). Raw error text is mapped to `errorClass` before it leaves the check function. Unit tests assert that no field contains an error string.
- **Resource limits:** at most 4 concurrent checks per node, at most 64 KiB read per response, at most 200 monitored services per scope (validation error above that), and delivery queue caps unchanged. When `requireFreeDisk` fails, state is kept in memory and persisted later, and observations continue.

### D13. Sibling integration contract

This change owns the alert vocabulary and routing for all four ops-automation changes. Siblings register their kinds through D1 and add their `pkg/notification` constants; they never deliver anything themselves.

| Kind | Owner | Scope | Subjects | Reasons (severity; type) | Events (opening / recovery) | Subscribed when |
|---|---|---|---|---|---|---|
| `service` | this change | scope | service | `unreachable`, `unhealthy_status`, `tls_invalid`, `replicas_missing` (critical); `unhealthy` (warning); level | `service_down` / `service_up`; `unhealthy`: `health_check_failed` / `health_check_recovered` | `monitoring.enabled` and any target |
| `host` | this change | node | `root`, `docker` | `disk_high`, `inodes_high` (warning, critical ≥95%); `memory_high`, `cpu_high`, `load_high`, `metrics_stale` (warning); level | `high_disk`, `high_inodes`, `high_memory`, `high_cpu`, `high_load`, `metrics_stale` / `resource_normal` | `alerts.host` |
| `container` | this change | scope | service | `crash_loop` (critical; level); `oom` (warning; event) | `container_crash_loop` / `container_recovered`; `container_oom` | `alerts.containers` |
| `certificate` | this change | scope | domain | `cert_expiring` (warning, critical within `criticalBefore`); `cert_expired` (critical); level | `ssl_expiring_soon`, `ssl_expired` / `ssl_renewed` | `alerts.certificates` |
| `selfheal` | `add-local-self-healing` | scope | service; `@node` | `gave_up` (critical; level); `circuit_open` (critical; level; subject `@node`); `action_failed` (warning; event) | `heal_gave_up` / `heal_resumed`; `heal_circuit_open` / `heal_circuit_closed`; `heal_action_failed` | an effective self-healing policy and any target |
| `hygiene` | `add-disk-hygiene-defaults` | node | `root`, `docker`, `image-gc` | `reclaim_insufficient` (warning; level); `state_unreadable` (warning; level; subject `image-gc`) | `disk_reclaim_insufficient` / `resource_normal`; `image_gc_blocked` / `image_gc_resumed` | `alerts.host` |
| `node` | `add-server-status-history` | node | condition type | `reboot_required`, `security_updates_pending` (warning); `clock_unsynchronized`, `docker_unreachable` (critical); level | `node_reboot_required`, `node_security_updates`, `node_clock_unsynchronized`, `node_docker_unreachable` / `node_condition_cleared` | `alerts.host` with `conditions: true` |

- **Naming:** alert kinds and reason codes are `snake_case` tokens. Dotted names stay for state-event types (`heal.restarted`, `cert.renew.*`) and are not alert vocabulary.
- **Per-service webhooks** receive only service-bound subjects of that service (`service`, `container`, `selfheal` with that subject), never `@`-subjects or node-scoped kinds.
- **Capabilities** follow `<area>.<feature>-v1`: `monitoring.health-v1` (this change), `node.disk-hygiene-v1`, `node.metrics-history-v1`, `node.status-v1` and `node.self-healing-v1`. A sibling capability never implies alert delivery; sibling kinds are delivered only on nodes that also advertise `monitoring.health-v1`. Alert kinds need no capability of their own, because subscription travels inside this change's policy.
- **Config in one `tako.yaml`:**

  ```yaml
  notifications:                # project-wide targets and alert policy (this change)
    slack: ${SLACK_WEBHOOK}
    alerts: {jobs: {...}, backups: {...}, services: {...}, host: {...}, containers: {...}, certificates: {...}}
  environments:
    production:
      selfHealing: {enabled: true}   # add-local-self-healing, environment defaults
      services:
        web:
          logging: {...}             # existing; when absent takod applies its node default (add-disk-hygiene-defaults)
          monitoring: {...}          # this change
          selfHealing: {...}         # add-local-self-healing
  ```

  `add-disk-hygiene-defaults` and `add-server-status-history` add no `tako.yaml` keys: their knobs are takod flags whose defaults ship in the binary, overridden through a systemd drop-in.
- **takod loops once all four have landed:** this change's check scheduler, host evaluator (60s), shared container sampler (30s), certificate scan (6h) and source dispatch; the hygiene loop, which replaces the build-cache loop; the native collector and node-status loop; and the heal loop, which reads the shared container sampler and probes `healthCheck.path`/TCP per container itself (this change's http checks go through the load-balanced proxy and cannot single out a replica). Nothing reads `current.json` while native collection is on.
- **Implementation order:** this change first; then `add-disk-hygiene-defaults` (after v0.14.2's `image_cleanup.go` is on master) and `add-server-status-history` in either order; `add-local-self-healing` last. Each later change registers its kind in the same PR that adds its source, so no sibling needs a "monitoring not present" fallback.

## Risks / Trade-offs

- **Local-proxy http checks miss DNS and upstream network faults.** This is accepted and documented. `--local` and external probes cover outside-in checks.
- **Host alerts on by default (disk 90%) for users who already have an `alerts:` block** may surprise some. This is mitigated by release notes and `host.disk: 0`. Disk-full is the most damaging silent failure, so on by default is the safer default.
- **Per-scope host episodes multiply internal state** (subjects x scopes). This is bounded by the number of scopes on a node. The ledger deduplicates outward noise.
- **Reading Caddy's store relies on its on-disk layout.** That layout is stable across Caddy 2.x, and the lookup is v0.14.2's, already used by `tako domains status`. If it fails, the source keeps the takod-store results.
- **Two alert managers share targets but not code paths.** Episode logic exists twice (jobs/backups untouched, sources new). That duplication is the price of not touching the job and backup path; the shared stateless pieces (client, delivery, validation, payload) keep behavior aligned. The existing `alerts_test.go` must pass unchanged, and a golden test pins the `scheduler-alerts` format.

## Migration Plan

1. Ship takod with `monitoring.health-v1`, the source manager and the extended `/v1/alerts`. With old CLIs, nothing changes.
2. Operators run `tako upgrade servers`. The next `tako deploy` with a new CLI pushes the policies. Configs that only gain default-derived sections keep deploying to un-upgraded servers, with a warning (D11).
3. `tako monitor` users:
   - The default changes from an endless loop to a status read.
   - Scripts that relied on the loop should deploy (takod now does the checking) or switch to `tako monitor --local`. `tako monitor --once` keeps checking locally and alerting.
   - The release notes call this out.
4. Downgrading takod is safe: the `alerts/` tree is ignored by older binaries, and job and backup alerts keep working.

## Assumptions

- The checker node runs a `tako-proxy` holding the service's route (D4 picks it from the route placement; otherwise the check reports `unknown`).
- `nproc` is available to takod through `runtime.NumCPU()` on the host. takod runs on the host, not in a container.

## Resolved questions

1. **Host and container alerts on by default when an `alerts:` block exists?** **Yes** for `disk` (90%), `inodes` (90%, native source only), `oom`, `crashLoop` and certificates; **no** for cpu, memory and load. They reach un-upgraded servers only as default-derived sections, which never fail a deploy (D11). *Rationale: silent disk, inode and certificate failures are the costly ones, and defaults must not break existing configs.*
2. **Local proxy or public URL for the http check?** **Local proxy**, verifying hostname and validity but not the chain. *Rationale: hairpin NAT breaks public checks, and origin certificates are not publicly trusted; outside-in stays with `--local`.*
3. **One checker node or every proxy node?** **One deterministic checker, chosen from the route placement.** *Rationale: one alert stream per service, and a replica node may not hold the route.*
4. **Keep the endless loop as the `tako monitor` default?** **No**; it becomes a status read, `--local` keeps the loop, and `--once` keeps its old meaning as `--local --once`. *Rationale: a safe scriptable default without silently disabling existing cron-based checkers.*
5. **Target-fingerprint ledger or one node-global policy?** **The ledger**, applied to all node-scoped kinds and `@`-subjects, with suppressed openings also suppressing the matching recovery. *Rationale: projects keep their own thresholds, and each endpoint sees one coherent episode.*
6. **New storage tree or `scheduler-alerts` schema 2?** **New tree**, and no shared-engine refactor. *Rationale: downgrade safety, and the job and backup path stays untouched.*
7. **v1 keys on job and backup payloads?** **Yes, additively**, and the job and backup `message` text stays byte-identical. *Rationale: one schema for consumers with no change to existing keys or text.*
8. **Separate `critical` delivery for certificates?** **Yes, one `escalation`.** *Rationale: "3 days left" deserves a second notice.*
9. **Existing invalid `monitoring.webhook` values?** **Warning for one deprecation cycle; never sent to takod or used by `--local`.** *Rationale: SSRF safety without failing config loads that worked yesterday.*

## Implementation notes

Where the plan and the code disagreed, the implementation took the safest equivalent:

- **Per-service targets:** they travel as a request-level `serviceTargets` map, not as `Targets.Services`. `AlertTargets` must stay comparable, because the legacy manager compares target generations with `==`.
- **Source API extensions (additive):**
  - `LevelObservation` gains `Severities`, `Hold`/`HoldReason` (paused, maintenance, unknown) and `Interval` (staleness).
  - `EventObservation` gains `Severity` and `Cooldown`.
  - `SourceAlertManager` adds `Retire`, `Subjects`, `AnySubscribed`, `ScopePolicies` and `Configured` for source bookkeeping.
  - `Damping.OpenFor` is ANDed with `OpenAfter`.
- **Ledger key:** `(kind, subject, targetFingerprint)` per episode, rather than the 5-tuple. Escalation and recovery follow the episode that owns the key, so a suppressed opening suppresses both. Events dedupe on `subject#reason` with their cooldown.
- **Loading and persistence:**
  - A scope whose files fail to load is skipped with a warning; it never stops takod.
  - Subjects of kinds that are not registered are dropped on load.
  - A failed write keeps state in memory and retries on the next flush.
  - Credential file mode checks are skipped on Windows (development only).
- **Host evaluator:** `cpuFor` is tracked in the evaluator, which reports `cpu_high` only after the threshold has held for that long. The status `reasons` list shows the current reasons only.
- **Checker node:** the first schedulable server in sorted order among the environment's proxy servers (`GetEnvironmentProxyServers`), which is where routes are written. Otherwise the first schedulable server.
- **Http monitors without a routable domain:** they fall back to a container check, with a deploy warning.
- **Container check:**
  - "Running" uses the active containers and falls back to all containers.
  - When every service in the revision has no assignments, the expected count is the replica count.
- **Config:**
  - An out-of-range legacy `monitoring.interval` is a warning and is clamped.
  - `monitoring` on job and run services is skipped rather than rejected.
- **`tako monitor --local`:**
  - `--once` is stateless and sends an opening for every failure, matching its old behavior.
  - Delivery goes to the deliverable per-service webhook, otherwise to the validated notification targets. The node is reported as `external`.
- **`MonitorResult`:** adds `notDeployed`, plus per-node `supported` and `error` fields.
- **Verification gaps:** WSL was unavailable during implementation, so verification ran on Windows. Tests that are Linux-only, such as `syscall.Stat_t` in takod's `service_files_test.go`, were type-checked with `GOOS=linux go vet`. `openspec validate --strict` ran with the Windows CLI. The live-server check (task 6.3) was not run.
- **Review follow-ups (ops integration):**
  - *Global services.* The expected local replica count of a global service is 1 when it has no placement assignments, otherwise the number of this node's assignments.
  - *Docker timeouts.* Container checks and the container sampler bound every Docker call to 20s. A timed-out or failed container check holds the subject with reason `docker_unavailable` instead of reporting the service down.
  - *`tako monitor` exit 6* also covers subjects that are stale or held with `docker_unavailable`, because their health is unknown.
  - *Certificate expiry* is reported by one node per scope: the lowest-named http checker node. Other proxy nodes retire their certificate subjects, so a domain routed on every proxy node alerts once.
  - *Lock order.* The alert-source manager takes the shared mutation lock before its own mutex. Flushes try the lock with a 500ms bound outside the mutex, so a held snapshot lock never blocks reporters.
  - *Shared targets.* When a scope that owns the ledger entry for a node-scoped subject is removed or retires that subject while another scope still alerts on it, ownership moves to that scope. The other scope sends its suppressed opening to the endpoint, and the retiring scope sends no retired recovery (no false all-clear).
  - *Deploy and alert push.* After a successful rollout, a failed alert config push is a deploy summary warning, never a deploy failure. On an up-to-date deploy, a push failure for an explicit `alerts`/`monitoring` config still fails the command; one for the default-only policy is a warning.
