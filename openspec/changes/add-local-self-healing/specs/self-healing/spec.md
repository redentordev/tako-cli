## ADDED Requirements

### Requirement: Self-healing config keys
Tako SHALL accept a `selfHealing` block on an environment and on a service with the keys `enabled`, `restartUnhealthy` (default true), `startExited` (default true), `unhealthyThreshold` (1-20, default 3), `maxRestarts` (1-20, default 3) and `window` (5m-24h, default 30m). Unknown keys and out-of-range values SHALL be validation errors. `selfHealing` on a `kind: job` or `kind: run` service SHALL be a validation error.

#### Scenario: Job service rejected
- **WHEN** a `kind: job` service declares `selfHealing`
- **THEN** `tako validate` fails with exit code 2 naming the service and key

#### Scenario: Out-of-range threshold rejected
- **WHEN** a service sets `selfHealing.unhealthyThreshold: 0`
- **THEN** `tako validate` fails naming the key and the allowed range

### Requirement: Effective self-healing policy
A service's effective policy SHALL be its own keys, else its environment's keys, else the defaults, with `enabled` defaulting to false. For a `persistent: true` service the environment-level `enabled` SHALL be ignored, so only the service's own `enabled: true` turns healing on.

#### Scenario: Disabled by default
- **WHEN** a config declares no `selfHealing` block
- **THEN** no service has an effective self-healing policy and takod takes no heal action

#### Scenario: Environment default applies to stateless services only
- **WHEN** environment `production` sets `selfHealing: {enabled: true}` and contains stateless `web` and `persistent: true` service `db` without their own block
- **THEN** `web` has healing enabled and `db` does not

#### Scenario: Persistent service opts in explicitly
- **WHEN** persistent service `db` sets `selfHealing: {enabled: true, restartUnhealthy: false}`
- **THEN** `db` has healing enabled with `restartUnhealthy` false and `startExited` true

### Requirement: Effective policy travels in desired state
The effective policy of each service SHALL be written into its desired-state document on every desired-state write (deploy, rollback, promote, scale, start, stop) and omitted when disabled. takod SHALL read self-healing policy only from desired state.

#### Scenario: Policy change takes effect on next deploy
- **WHEN** an operator adds `selfHealing: {enabled: true}` to `web` without deploying
- **THEN** takod does not heal `web`
- **AND** after the next `tako deploy`, takod uses the new policy

### Requirement: Capability gate
takod SHALL advertise `heal.local-v1`. A command that writes desired state SHALL fail with exit code 2 before any mutation when a service's effective policy is enabled and a target node does not advertise `heal.local-v1`, naming the capability and asking to upgrade the node agent. `tako heal` commands SHALL require the capability on every node they contact.

#### Scenario: Old agent refused
- **WHEN** `web` enables self-healing and target node `app` runs a takod without `heal.local-v1`
- **THEN** `tako deploy` exits 2 before any container change, naming `heal.local-v1`

### Requirement: Converged records
takod SHALL keep a node-local converged record per service listing the containers it started for the service's current revision. It SHALL write the record only after a reconcile, restore-on-failure finalize or restore succeeds on that node, mark it converging while a reconcile is in progress, and clear it when the service is removed, scaled to zero or cleaned up.

#### Scenario: Interrupted rollout
- **WHEN** a deploy is interrupted after starting some new containers
- **THEN** the service's record stays converging and takod takes no action on the service until a later reconcile succeeds

### Requirement: Heal acts only on recorded containers
takod SHALL take heal actions only on containers listed in a non-converging converged record whose container ID still matches, and only on active containers (not labeled `tako.active=false`).

#### Scenario: Retained and failed containers untouched
- **WHEN** a recreate rollout with restore-on-failure has retained and stopped the previous containers, or retain-failed has stopped failed new containers
- **THEN** none of those containers is started or restarted by takod

#### Scenario: Hand-started container untouched
- **WHEN** an operator runs a container with Tako labels by hand on the node
- **THEN** takod never restarts or starts it

#### Scenario: Warming blue-green containers ignored
- **WHEN** `web` has inactive warming containers awaiting promotion that are unhealthy
- **THEN** takod does not restart them

### Requirement: Restart unhealthy containers
For a service with `restartUnhealthy` enabled, takod SHALL run `docker restart` on a recorded running container after `unhealthyThreshold` consecutive unhealthy observations spanning at least 60 seconds, and only after its grace period: the later of container start plus `max(healthCheck.startPeriod, 2m)` and the record's write time plus 5 minutes.

#### Scenario: Hung web container restarted
- **WHEN** `web` has `healthCheck.path: /health`, healing enabled, and one replica fails 3 consecutive checks over 90 seconds, 10 minutes after it started
- **THEN** takod restarts that one container
- **AND** records a `heal.restarted` event naming the service, container and reason

#### Scenario: Grace period respected
- **WHEN** a container fails its checks 1 minute after it started with `startPeriod: 30s`
- **THEN** takod does not restart it until 2 minutes after start

### Requirement: Health observation sources
Unhealthy observations SHALL come from Docker's health status and from per-container check results provided by health monitoring. An observation older than three check intervals SHALL count as unknown and SHALL NOT trigger an action. A service without any health source SHALL NOT receive unhealthy restarts.

#### Scenario: Stale observations do nothing
- **WHEN** health monitoring stops producing observations for a container
- **THEN** takod treats its health as unknown and takes no action

### Requirement: Start exited containers Docker will not restart
For a service with `startExited` enabled, takod SHALL run `docker start` on a recorded container that is exited or created, not being restarted by Docker, and whose restart policy is `no` or `on-failure[:N]`. takod SHALL NOT start an exited container whose restart policy is `always` or `unless-stopped`, and SHALL report it as `stopped`.

#### Scenario: on-failure exhausted
- **WHEN** worker `queue` uses `restart: on-failure:3` and its recorded container exited after Docker gave up
- **THEN** takod starts the container and records `heal.started`

#### Scenario: Manual stop with default policy respected
- **WHEN** an operator runs `docker stop` on a `web` container that uses the default `unless-stopped` policy
- **THEN** takod does not start it
- **AND** `tako heal status` shows the container as `stopped`

### Requirement: Missing containers are reported, not recreated
takod SHALL NOT create containers. A recorded container that no longer exists SHALL be reported as `missing` together with the command that repairs it.

#### Scenario: Missing container reported
- **WHEN** a recorded `web` container was removed with `docker rm -f`
- **THEN** takod takes no action
- **AND** `tako heal status` shows it as `missing` with `tako deploy -e production --service web`

### Requirement: Environment eligibility
takod SHALL skip a whole environment while an unexpired operation lease for it is held by anyone else, a controller worker operation is active, a node upgrade holds the lifecycle exclusion, a volume restore transaction is pending, the node is not schedulable or has a deployment-deny latch, a pause applies, or no desired-state document exists.

#### Scenario: Deploy in progress
- **WHEN** a deploy holds the environment lease while a `web` container is unhealthy
- **THEN** takod takes no action until the lease is released and the grace period after the new record has passed

### Requirement: Service eligibility
takod SHALL skip a service that has no enabled policy; is `kind: job`, `kind: run` or removal-pending; has zero desired replicas or none assigned to the node; has a restore-on-failure retention record; has a backup or restore of its volumes running; has a `<service>-maintenance` container; has a missing or converging record or a record whose size differs from its desired replicas on the node; or is given up or paused.

#### Scenario: Maintenance mode
- **WHEN** `tako maintenance --service web` is active
- **THEN** takod takes no heal action on `web`

#### Scenario: Stopped service
- **WHEN** `tako stop --service web` scaled `web` to zero
- **THEN** takod takes no action on `web`

### Requirement: Heal actions hold the environment lease
Before each action takod SHALL acquire the environment's operation lease on its own node with operation `self-heal` and a TTL of at most 5 minutes, re-evaluate eligibility under it, perform exactly one `docker restart` or `docker start`, and release the lease without waiting for the container to become healthy.

#### Scenario: Heal never blocks a deploy for long
- **WHEN** takod crashes while holding a `self-heal` lease
- **THEN** the lease expires within 5 minutes

#### Scenario: Eligibility changes before the action
- **WHEN** a maintenance companion appears between the evaluation and the lease acquisition
- **THEN** takod releases the lease without acting

### Requirement: CLI waits briefly for self-heal leases
A CLI operation that finds the environment lease held with operation `self-heal` SHALL wait and retry for up to 60 seconds before failing with the existing lock error. Leases held by any other operation SHALL keep their current behavior.

#### Scenario: Deploy meets a heal action
- **WHEN** `tako deploy` starts while takod holds the lease for a 3-second restart
- **THEN** the deploy waits, acquires the lease after it is released, and proceeds normally

### Requirement: Per-container backoff
takod SHALL wait at least 2 minutes after an action before acting on the same container again, doubling the wait per action up to 30 minutes, and SHALL reset a container's action count and wait after it stays healthy for a full `window`. takod SHALL run at most one action at a time per node.

#### Scenario: Cooldown doubles
- **WHEN** a container was restarted twice and is unhealthy again
- **THEN** takod waits at least 4 minutes after the second restart before acting again

### Requirement: Give up after repeated actions
When a service's containers on a node reach `maxRestarts` actions within `window`, takod SHALL mark the service `gave_up` on that node, stop acting on it, record `heal.gave_up` and raise one alert through the health-monitoring alert API. `gave_up` SHALL clear when a converged record with a different revision is written or on `tako heal reset`.

#### Scenario: Crash-looping app gives up
- **WHEN** `web` is restarted 3 times within 30 minutes and is still unhealthy
- **THEN** `web` becomes `gave_up` on that node and no further restarts happen
- **AND** one alert reports the give-up with the service, node and last reason

#### Scenario: Deploy clears give-up
- **WHEN** a new revision of `web` is deployed successfully
- **THEN** `web` leaves `gave_up` and is watched again after the grace period

### Requirement: Node circuit breaker
When more than half of a node's eligible services (at least 2) need an action in the same evaluation, or more than 6 actions ran on the node within 10 minutes, takod SHALL pause healing on the node for 30 minutes and record and alert `heal.circuit_open`.

#### Scenario: Node-wide failure trips the breaker
- **WHEN** 4 of 5 healed services on a node become unhealthy at the same time
- **THEN** takod restarts nothing, pauses healing on the node for 30 minutes, and raises a `heal.circuit_open` alert

### Requirement: Heal never moves or creates workloads
Self-healing SHALL only run `docker restart` and `docker start` on existing containers of its own node. It SHALL NOT create, remove or rename containers, change restart policies, desired state, deployment history, placement or proxy configuration, or act on another node. `state.onUnreachableNode` SHALL keep accepting only `block`.

#### Scenario: Node down
- **WHEN** node `app2` is unreachable
- **THEN** no node starts or moves `app2`'s replicas

### Requirement: Heal journal
takod SHALL append every heal action and state change (restarted, started, failed, gave up, reset, paused, resumed, circuit opened or closed, newly stopped or missing containers) to a node-local journal capped at the last 1000 entries per environment, with redacted reasons. Heal actions SHALL NOT create deployment history entries.

#### Scenario: Restart is auditable
- **WHEN** takod restarts a `web` container
- **THEN** `tako heal status -e production --service web` lists the restart with time, node, container, reason and result
- **AND** `tako history` is unchanged

### Requirement: Heal state events
takod SHALL append heal actions and state changes to the environment's state event log with the types `heal.restarted`, `heal.started`, `heal.failed`, `heal.gave_up`, `heal.reset`, `heal.paused`, `heal.resumed` and `heal.circuit_open`.

#### Scenario: Give-up event recorded
- **WHEN** `web` gives up on node `app`
- **THEN** the `production` event log on `app` contains one `heal.gave_up` event for `web`

### Requirement: Heal status command
`tako heal status` SHALL show, per node and service, the effective policy, heal state, per-container state and the last 10 journal entries, and with `--output json` SHALL return a `HealStatus` document.

#### Scenario: Status as JSON
- **WHEN** an operator runs `tako heal status -e production --output json`
- **THEN** the output is one `HealStatus` document with an entry per target node and healed service

### Requirement: Heal pause, resume and reset commands
`tako heal pause` SHALL accept `--service` and `--for <duration>` (default 1h, maximum 7d) or `--until-resume`. `pause`, `resume` and `reset` SHALL apply to every target node or to `--server`, return a `HealChangeResult` with per-node outcomes, work on cordoned nodes, and not take the environment lease.

#### Scenario: Pause during an incident
- **WHEN** an operator runs `tako heal pause -e production --for 2h`
- **THEN** no heal action runs in `production` on any target node for 2 hours
- **AND** `tako heal status` shows the pause and its expiry

#### Scenario: Reset after fixing the cause
- **WHEN** `web` is `gave_up` and the operator runs `tako heal reset -e production --service web`
- **THEN** `web` is watched again with fresh counters

### Requirement: Heal state in tako ps
When a node advertises `heal.local-v1` and a service has an effective policy, `tako ps` rows SHALL include an additive `heal` object with `state`, `lastAction` and `lastActionAt`, and the text status SHALL note `heal:gave_up` or `heal:paused` for those states only.

#### Scenario: Given-up service visible in ps
- **WHEN** `web` is `gave_up` on node `app`
- **THEN** `tako ps` shows `heal:gave_up` for `web` on `app`
- **AND** `tako ps --output json` includes `heal.state: "gave_up"`
