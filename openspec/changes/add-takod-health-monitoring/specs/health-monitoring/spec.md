# Spec Delta

## Purpose

Services, hosts, containers and certificates deployed by Tako are monitored by the node agent itself, without a terminal open. Operators get one deduplicated alert when something breaks and one when it recovers, through the notification targets they already configured.

## ADDED Requirements

### Requirement: takod runs configured service checks
For every service with `monitoring.enabled: true`, takod SHALL run the configured check at the service's `monitoring.interval`, defaulting to 60s, after a deploy has applied the monitoring policy. Running the checks SHALL NOT depend on any CLI process.

#### Scenario: Interval is honored
- **WHEN** a service sets `monitoring.interval: 30s` and the policy has been deployed
- **THEN** takod checks the service about every 30 seconds, with at most 10% jitter
- **AND** no CLI process is running

#### Scenario: HTTP check goes through the local proxy
- **WHEN** a public service with domain `shop.example.com` and health path `/health` uses `checkType: http`
- **THEN** the checker node connects to its local proxy on `127.0.0.1:443`, using SNI and Host `shop.example.com`, and requests `/health` without following redirects
- **AND** a 2xx response within the timeout counts as passing

#### Scenario: Container check compares local assignments
- **WHEN** a node holds 2 assignments for `worker` in the desired revision and only 1 active container is running
- **THEN** that node observes `worker` failing with reason `replicas_missing`, details `expected=2` and `running=1`

#### Scenario: One checker node per HTTP monitor
- **WHEN** an environment has servers `app-1` and `app-2`, and `web` has replicas on both
- **THEN** exactly one server, the first by sort order that hosts a replica, runs the http check for `web`
- **AND** both servers run container checks for their own `web` replicas

### Requirement: Checks pause during operations and maintenance
takod SHALL NOT record failing observations for a service subject while the environment's operation lease is held, during the post-operation grace period, while an http-checked service is in maintenance mode, or while the service is pending removal.

#### Scenario: Deploy does not trigger an outage alert
- **WHEN** a deploy holds the lease and recreates `web`, so it is unreachable for 40 seconds
- **THEN** no `opening` alert is sent for `web`
- **AND** checks resume after the lease is released plus `max(healthCheck.startPeriod, 30s)`

#### Scenario: Maintenance mode suppresses HTTP alerts
- **WHEN** `tako maintenance web` is active
- **THEN** `tako monitor` shows `web` in state `maintenance`
- **AND** no http failure alert is sent for `web`

### Requirement: Alerts open and recover once per episode
An alert subject SHALL send one `opening` delivery per target when its damping threshold is met, and one `recovery` delivery per target that received the opening once its recovery threshold is met. Repeated failing observations within an open episode SHALL NOT send further openings.

#### Scenario: Three failures then recovery
- **WHEN** `alertAfter` is 3 and `recoverAfter` is 2, and `web` fails 5 checks in a row and then passes 2
- **THEN** exactly one `opening` is delivered after the third failure
- **AND** exactly one `recovery` is delivered after the second pass

#### Scenario: Recovery reaches every target that got the opening
- **WHEN** an opening was delivered to Slack and to the service webhook
- **THEN** the recovery is delivered to both

### Requirement: Flapping subjects are rate limited
A subject SHALL NOT produce more than 6 transitions per rolling hour. When the limit is reached, takod SHALL send one `suppressed` delivery and mark the subject `flapping`. It SHALL send the then-current state once the subject has been stable for 30 minutes.

#### Scenario: Flapping service
- **WHEN** `web` alternates between failing and passing so it would open and recover 10 times in an hour
- **THEN** at most 6 transitions and one `suppressed` delivery are sent in that hour
- **AND** `tako monitor` shows `web` as `flapping`

### Requirement: Host threshold alerts
When the deployed policy has a `notifications.alerts.host` section, takod SHALL raise host alerts from the node metrics file:
- `disk_high`, on by default at 90% and critical at 95% or more;
- `memory_high`, `cpu_high` and `load_high`, off unless configured;
- `metrics_stale`, when the metrics file is missing or more than 5 minutes old.

#### Scenario: Disk filling up
- **WHEN** the metrics file reports 91% root disk usage and the policy has the default host thresholds
- **THEN** a `warning` alert with event `high_disk`, reason `disk_high` and details `percent=91` and `threshold=90` is delivered
- **AND** the alert escalates to `critical` once usage reaches 95%

#### Scenario: Sustained CPU only
- **WHEN** `host.cpu: 90` and `host.cpuFor: 10m` are set, and CPU is above 90% for 4 minutes and then drops
- **THEN** no alert is sent

#### Scenario: Stale metrics agent
- **WHEN** the metrics file timestamp is 7 minutes old
- **THEN** a `warning` alert with reason `metrics_stale` is delivered
- **AND** other host thresholds are not evaluated from the stale data

#### Scenario: Same endpoint, two environments
- **WHEN** environments `staging` and `production` of one project share a server and the same Slack webhook, and both enable host alerts
- **THEN** a disk alert for that server reaches the Slack webhook once, not twice

### Requirement: Container OOM and crash-loop detection
When container alerts are enabled, takod SHALL report an OOM kill of a monitored scope's container as a one-shot `container_oom` event, with at most one delivery per container per 15 minutes. It SHALL report a crash loop, meaning `crashLoop` restarts within `crashLoopWindow` or 3 consecutive samples in state `restarting`, as a `container_crash_loop` alert that recovers after two quiet windows. Detection SHALL NOT restart, stop or recreate any container.

#### Scenario: OOM kill
- **WHEN** a `web` container is restarted by Docker after being OOM-killed
- **THEN** one `event` delivery with event `container_oom` and the container name is sent
- **AND** a second OOM kill within 15 minutes is not delivered separately but counted in `details.repeats` of the next delivery

#### Scenario: Crash loop opens and recovers
- **WHEN** `worker` restarts 3 times within 10 minutes
- **THEN** a `critical` `container_crash_loop` alert opens
- **AND** it recovers after `worker` has run for 20 minutes without restarting

### Requirement: Certificate expiry alerts
When certificate alerts are enabled, takod SHALL check every certificate served for a subscribed scope at least every 6 hours: those in the takod certificate store and those in the proxy's ACME (HTTP-01) store. It SHALL raise a `warning` within `warnBefore` (default 336h), escalate to `critical` within `criticalBefore` (default 72h), raise `ssl_expired` after expiry, and recover after renewal.

#### Scenario: Renewal has been failing
- **WHEN** the certificate for `shop.example.com` expires in 10 days
- **THEN** a `warning` alert with event `ssl_expiring_soon`, details `domain`, `expiresAt` and `daysLeft=10` is delivered

#### Scenario: Renewal succeeds
- **WHEN** the certificate is then renewed and now expires in 89 days
- **THEN** a `recovery` with event `ssl_renewed` is delivered

#### Scenario: Caddy-managed certificate
- **WHEN** a domain's certificate was issued by the proxy through HTTP-01 and exists only in the proxy data store
- **THEN** its expiry is still checked and reported with `details.source=caddy`

### Requirement: Stable credential-free alert payload
Every alert delivery, including job and backup alerts, SHALL use payload `schemaVersion: 1`. It SHALL keep every existing key, and add `event`, `node`, `severity`, `subject`, `details` and `openedAt`. Payloads, status responses and monitor results SHALL contain only reason codes, public names, timestamps, numbers and allowlisted enum tokens. They SHALL NEVER contain targets, error strings, response bodies, headers or environment values.

#### Scenario: Existing job alert consumer
- **WHEN** a job alert fires for a consumer that reads `transition`, `kind`, `service` and `reasons`
- **THEN** those keys have the same values as before
- **AND** the payload additionally has `schemaVersion: 1` and `event: "job_alert"`

#### Scenario: Connection error text is not leaked
- **WHEN** an http check fails with a Go error string containing an internal address
- **THEN** the delivery carries `details.errorClass=connect`
- **AND** no field contains the error string

### Requirement: Alert targets are validated and SSRF-safe
Every alert target, including `services.<name>.monitoring.webhook`, SHALL be validated at config load with the same rules as `notifications` targets: public HTTPS endpoints only. Delivery SHALL use the SSRF-safe transport that refuses private or loopback addresses after DNS resolution and does not follow redirects.

#### Scenario: Private monitoring webhook
- **WHEN** `monitoring.webhook` is `http://10.0.0.5/hook`
- **THEN** config validation fails with exit 2, naming the service and the field

#### Scenario: Per-service webhook without global targets
- **WHEN** `notifications` has no target and `web.monitoring.webhook` is a valid public URL
- **THEN** the config is valid
- **AND** alerts for `web` go only to that webhook

### Requirement: Alert source API for node components
takod SHALL provide one alert source API through which node components report level and event observations for registered subject kinds, with closed reason and detail vocabularies. Episodes, damping, flap control, delivery, retries and targets SHALL be handled by the API, not by the source. Reports with unregistered kinds, reasons or detail keys SHALL be rejected.

#### Scenario: Sibling source registers a kind
- **WHEN** a node component registers kind `selfheal` with reasons `restart_budget_exhausted` and reports a level observation with that reason
- **THEN** the subject opens and is delivered with the standard payload

#### Scenario: Unknown detail key
- **WHEN** a source reports a detail key that it did not register
- **THEN** the report is rejected and nothing is delivered

### Requirement: Monitoring requires a capable node agent
`tako deploy` SHALL fail with exit code 2 before any mutation when the config enables service monitoring, host, container or certificate alerts, or per-service targets, and any schedulable target server lacks the `monitoring.health-v1` capability. The error SHALL name the servers and suggest `tako upgrade servers`.

#### Scenario: Old node agent
- **WHEN** `app-2` runs a takod without `monitoring.health-v1` and the config enables `web.monitoring`
- **THEN** `tako deploy` exits 2, naming `app-2`, and no container is changed

#### Scenario: Job alerts on an old node are unaffected
- **WHEN** the config only has `notifications.alerts.jobs` and the node has `scheduler.alerts-v1` but not `monitoring.health-v1`
- **THEN** deploy succeeds and job alerts are applied as before

### Requirement: Downgrade-safe alert storage
New alert kinds and their state SHALL be stored separately from the existing job and backup alert state, so a node agent without this feature can load its own alert state unchanged.

#### Scenario: Downgrade after enabling monitoring
- **WHEN** a node has active service and host alert state and takod is replaced by an older version
- **THEN** the older takod starts, and job and backup alerts keep working

### Requirement: tako monitor shows node-side status
`tako monitor` SHALL read alert and check status from every server of the environment and print it once. `--watch` SHALL refresh it in human mode. `--output json` SHALL return a `MonitorResult`. The command SHALL exit 6 when any subject is alerting or flapping or any server could not be read, and SHALL exit 4 when no server could be read.

#### Scenario: Status table
- **WHEN** `web` is alerting on `app-1` and everything else is ok
- **THEN** `tako monitor` prints `web` with state `alerting`, its severity, reasons and since-time
- **AND** exits 6

#### Scenario: Machine output
- **WHEN** `tako monitor --output json` runs
- **THEN** stdout is one `MonitorResult` document with per-node subjects and counts
- **AND** `--watch` combined with `--output json` exits 2

#### Scenario: Capability missing everywhere
- **WHEN** no server of the environment advertises `monitoring.health-v1`
- **THEN** `tako monitor` exits 2 with a hint to run `tako upgrade servers` or use `--local`

### Requirement: Local foreground monitoring remains available
`tako monitor --local` SHALL run checks from the CLI host against the public URLs at each service's own interval. It SHALL report the actual server name, or `external` for public URL checks, send one opening and one recovery per episode with the same damping defaults, use payload schema v1, and deliver only to validated targets through the SSRF-safe transport.

#### Scenario: Recovery from local mode
- **WHEN** `tako monitor --local` sees `web` fail 3 times and then pass 2 times
- **THEN** it sends one opening and one recovery
- **AND** no payload contains `"server": "prod"` unless a server is actually named `prod`
