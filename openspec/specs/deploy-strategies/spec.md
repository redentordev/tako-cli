# deploy-strategies Specification

## Purpose
Defines how Tako chooses and executes a long-running service's rollout strategy, how it guards traffic cutover with readiness checks, how it recovers the previous revision when a rollout fails, and how strategy and recovery are reported to humans and machines.

## Requirements

### Requirement: Explicit strategy is preserved
When a service sets `deploy.strategy`, Tako SHALL use exactly that strategy with the same validation rules and runtime behavior it had before this change. Default selection SHALL NOT override an explicit value.

#### Scenario: Explicit recreate stays recreate
- **WHEN** a stateless proxied service sets `deploy.strategy: recreate`
- **THEN** the effective strategy is `recreate`
- **AND** the strategy source is reported as `explicit`

#### Scenario: Explicit rolling keeps its validation errors
- **WHEN** a persistent service sets `deploy.strategy: rolling`
- **THEN** validation fails with the same error message as before this change

### Requirement: Rolling is the default for eligible services
When `deploy.strategy` is omitted on a long-running service (`kind` unset or `service`), Tako SHALL select `rolling` if and only if every one of these holds. The service is not `persistent`. It declares `port > 0`. It declares no raw `ports` and no `volumes`. `deploy.maxUnavailable` is 0. `deploy.maxSurge` is 0 or at least `replicas`. `deploy.gracePeriod`, `deploy.promotion` and `deploy.smokeTest` are unset. The config would pass `rolling` validation unchanged.

#### Scenario: Stateless proxied web service defaults to rolling
- **WHEN** a service with `port: 3000`, a `proxy` route, `replicas: 2`, and no `deploy.strategy` is validated
- **THEN** the effective strategy is `rolling`
- **AND** the strategy source is reported as `default`

#### Scenario: Internal-only service with a port defaults to rolling
- **WHEN** a non-persistent service with `port: 8080`, no `proxy`, no `ports`, no `volumes`, and no `deploy.strategy` is validated
- **THEN** the effective strategy is `rolling`

#### Scenario: maxSurge equal to replicas still defaults to rolling
- **WHEN** an otherwise eligible service sets `replicas: 3` and `deploy.maxSurge: 3` without `deploy.strategy`
- **THEN** the effective strategy is `rolling`

### Requirement: Recreate remains the default for ineligible services
When `deploy.strategy` is omitted and any rolling-eligibility condition fails, Tako SHALL select `recreate`. A tako.yaml that validated before this change SHALL still validate, with no new errors or warnings caused by default selection.

#### Scenario: Persistent service defaults to recreate
- **WHEN** a service with `persistent: true` omits `deploy.strategy`
- **THEN** the effective strategy is `recreate`

#### Scenario: Raw host ports default to recreate
- **WHEN** a service declares `ports: ["5432:5432"]` and omits `deploy.strategy`
- **THEN** the effective strategy is `recreate`
- **AND** validation succeeds as it did before this change

#### Scenario: Volume-backed service defaults to recreate
- **WHEN** a non-persistent service declares `volumes: ["uploads:/app/uploads"]` and omits `deploy.strategy`
- **THEN** the effective strategy is `recreate`

#### Scenario: Portless worker defaults to recreate
- **WHEN** a service with no `port` (for example a queue worker) omits `deploy.strategy`
- **THEN** the effective strategy is `recreate`

#### Scenario: Partial maxSurge defaults to recreate instead of failing
- **WHEN** a service sets `replicas: 3` and `deploy.maxSurge: 1` without `deploy.strategy`
- **THEN** the effective strategy is `recreate`
- **AND** validation succeeds

#### Scenario: maxUnavailable keeps recreate
- **WHEN** a service sets `deploy.maxUnavailable: 1` without `deploy.strategy`
- **THEN** the effective strategy is `recreate`

### Requirement: Jobs and runs have no rollout strategy
Services with `kind: job` or `kind: run` SHALL NOT receive a default deploy strategy. Setting `deploy.strategy` or `deploy.rollbackOnFailure` on `kind: run` SHALL remain a validation error.

#### Scenario: Run kind rejects rollbackOnFailure
- **WHEN** a `kind: run` service sets `deploy.rollbackOnFailure: false`
- **THEN** validation fails with "kind: run cannot set deploy rollout controls"

#### Scenario: Job kind is unaffected
- **WHEN** a `kind: job` service is deployed
- **THEN** its schedule is applied exactly as before this change and no rollout strategy is reported for it

### Requirement: Defaulted rolling gates traffic on readiness
Before shifting proxy traffic to a revision deployed through defaulted `rolling`, Tako SHALL wait for each new container to pass `deploy.readiness` if set, else `healthCheck` if set, else a TCP connect to the service `port`. Explicit `rolling` without either check SHALL keep the running-only gate it had before this change.

#### Scenario: No healthCheck uses implicit TCP readiness
- **WHEN** a defaulted-rolling service with `port: 3000` and no `healthCheck` or `deploy.readiness` is deployed
- **THEN** Tako waits until a TCP connection to port 3000 in each new container succeeds before switching traffic
- **AND** it makes at most 60 connection attempts per container before failing readiness

#### Scenario: Implicit readiness failure keeps the old revision serving
- **WHEN** a defaulted-rolling service's new container never accepts TCP connections on its port within the readiness budget
- **THEN** the deploy fails
- **AND** proxy routes still point at the previous revision
- **AND** the previous revision's containers are still running

#### Scenario: Configured healthCheck takes precedence
- **WHEN** a defaulted-rolling service declares `healthCheck.path: /up`
- **THEN** readiness is the HTTP check on `/up`, not the implicit TCP probe

### Requirement: Rolling cutover order
A `rolling` deploy SHALL start the full replacement revision alongside the old one, pass readiness on every new container, switch proxy routes to the new revision, and only then stop and remove the old revision's containers. The effective surge for `deploy.maxSurge: 0` SHALL equal `replicas`.

#### Scenario: Old containers outlive the cutover
- **WHEN** a rolling deploy of a 2-replica service succeeds
- **THEN** 2 new-revision containers pass readiness before proxy routes change
- **AND** the old-revision containers are stopped only after the proxy acknowledges the new routes

### Requirement: rollbackOnFailure defaults to true
`deploy.rollbackOnFailure` SHALL be tri-state: unset means `true`. With `true`, a failed rollout SHALL leave the previous revision serving. With `false`, Tako SHALL NOT restore the previous revision and SHALL keep failed new containers stopped but not removed until the next deploy of that service.

#### Scenario: Unset behaves as true
- **WHEN** a service omits `deploy.rollbackOnFailure` and its rollout fails
- **THEN** Tako applies restore-on-failure behavior

#### Scenario: Rolling failure with rollbackOnFailure false keeps failed containers for debugging
- **WHEN** a rolling service sets `deploy.rollbackOnFailure: false` and a new container fails readiness
- **THEN** the old revision keeps serving traffic
- **AND** the failed new-revision containers remain on the node in a stopped state so their logs can be read

#### Scenario: Recreate failure with rollbackOnFailure false does not restore
- **WHEN** a recreate service sets `deploy.rollbackOnFailure: false` and its new container fails its health check
- **THEN** the previous revision is not restarted
- **AND** the failed container remains stopped on the node
- **AND** the deploy result reports recovery status `disabled`

### Requirement: Recreate restores the previous revision on failure
For a `recreate` service with `rollbackOnFailure` true, Tako SHALL stop the previous containers and keep them instead of removing them. If any new container on any assigned node fails to start, join networks, or pass its health check, Tako SHALL remove all new containers and restart the kept containers on every node for that service.

#### Scenario: Health check failure restores old containers
- **WHEN** a single-node recreate service's new container fails its health check
- **THEN** the new container is removed
- **AND** the previous container is started again under its original name and network aliases
- **AND** the service serves the previous revision

#### Scenario: Partial multi-node failure restores every node
- **WHEN** a recreate service is assigned to nodes A and B, node A's reconcile succeeds and node B's fails
- **THEN** Tako restores the previous containers on both A and B
- **AND** no node is left running the new revision for that service

#### Scenario: Persistent service restore reuses its volumes
- **WHEN** a persistent recreate service's new container fails its health check
- **THEN** the previous container is restarted with the same volume mounts and host ports it had before
- **AND** no volume is created, removed, or rewritten by the restore

### Requirement: Recreate finalizes kept containers only after success
Tako SHALL remove the kept previous containers of a `recreate` service only after every assigned node has passed health checks for the new revision. The next reconcile of that service SHALL remove kept containers left behind by an interrupted deploy.

#### Scenario: Successful recreate removes kept containers
- **WHEN** a recreate deploy succeeds on all assigned nodes
- **THEN** the kept previous containers are removed before the deploy reports success

#### Scenario: Interrupted deploy leftovers are cleaned on next deploy
- **WHEN** a kept previous container remains after the CLI was interrupted mid-deploy
- **THEN** the next deploy of that service removes it before starting new containers

#### Scenario: Leftovers are identified by recomputed name, not suffix
- **WHEN** a stopped container's name equals the retained name recomputed from its own project, environment, service and slot labels
- **THEN** the next reconcile of that service treats it as a retained leftover and removes it
- **AND** a container whose name merely ends in `--prev` but does not match the recomputed retained name is not removed as a leftover

#### Scenario: Retained names are always valid container names
- **WHEN** a recreate service with very long project, environment and service names retains its previous containers
- **THEN** every retained name is at most 128 characters and passes takod's container-name validation

### Requirement: Restore is scoped to the failing service
Restore SHALL apply only to the service whose rollout failed. Services that completed earlier in the same deploy SHALL NOT be reverted. Services not yet started SHALL NOT be deployed.

#### Scenario: Earlier service is not reverted
- **WHEN** a deploy updates services `api` then `web`, `api` succeeds and `web` fails and is restored
- **THEN** `api` remains on its new revision
- **AND** the result reports `api` as `deployed` and `web` as `restored`

### Requirement: Restore failure is reported, not hidden
If restoring the previous revision fails on any node, Tako SHALL report recovery status `failed` with the per-node error. It SHALL leave remaining containers in place for inspection. The deploy SHALL exit with code 1.

#### Scenario: Restore cannot restart old container
- **WHEN** a recreate rollout fails and the kept previous container cannot be restarted
- **THEN** the deploy result for that service has `recovery.status: failed` and the node error
- **AND** a `deploy.service.restore_failed` event is emitted
- **AND** the process exits with code 1

### Requirement: Restore requires a capable takod
Failure handling that differs from legacy behavior (`recreate` with restore, or any strategy with `rollbackOnFailure: false`) SHALL require every assigned node to advertise `deploy.restore-on-failure-v1`. If `rollbackOnFailure` is set explicitly, a missing capability SHALL fail the deploy before any container changes with a `tako upgrade servers` message.

#### Scenario: Explicit rollbackOnFailure on old takod fails closed
- **WHEN** a recreate service sets `deploy.rollbackOnFailure: true` and a target node's takod lacks `deploy.restore-on-failure-v1`
- **THEN** the deploy fails before stopping any container
- **AND** the error names the node, the capability, and `tako upgrade servers`

#### Scenario: Explicit rollbackOnFailure false on old takod fails closed
- **WHEN** a rolling service sets `deploy.rollbackOnFailure: false` and a target node's takod lacks `deploy.restore-on-failure-v1`
- **THEN** the deploy fails before starting any container, with the same upgrade message

#### Scenario: Rolling default needs no new capability
- **WHEN** a defaulted-rolling service deploys to a node running an older takod that supports `rolling`
- **THEN** the deploy proceeds without a capability error

### Requirement: Defaulted restore falls back on older takod
If `rollbackOnFailure` is not set and any assigned node lacks `deploy.restore-on-failure-v1`, Tako SHALL use legacy replacement for that service on all of its nodes, SHALL NOT send a failure policy to any of them, and SHALL emit a warning recommending `tako upgrade servers`.

#### Scenario: Defaulted rollbackOnFailure on old takod falls back with warning
- **WHEN** a recreate service omits `deploy.rollbackOnFailure` and a target node's takod lacks the capability
- **THEN** the service deploys with the previous remove-then-start behavior
- **AND** a warning event names the node and recommends `tako upgrade servers`

#### Scenario: One old node downgrades the service on every node
- **WHEN** a recreate service that omits `deploy.rollbackOnFailure` is assigned to nodes A and B, and only node B lacks `deploy.restore-on-failure-v1`
- **THEN** the service deploys with legacy remove-then-start on both A and B
- **AND** neither node receives a failure policy in its reconcile request
- **AND** a single warning event names node B

### Requirement: Plan output shows effective strategy and its source
`tako deploy --plan-only` SHALL report, for each changed long-running service, the effective strategy and whether it is `explicit` or `default`. When a deployed service's recorded strategy is `recreate` and the defaulted effective strategy is `rolling`, the change reasons SHALL include "Deploy strategy default changed: recreate → rolling".

#### Scenario: JSON plan includes strategy fields
- **WHEN** `tako deploy --plan-only --output json` runs for a config with a defaulted-rolling service `web`
- **THEN** the `DeployPlan` change for `web` has `strategy: "rolling"` and `strategySource: "default"`

#### Scenario: First deploy after upgrade explains the redeploy
- **WHEN** service `web` was last deployed with implicit `recreate` and is unchanged except for the new default
- **THEN** the plan lists `web` as an update with reason "Deploy strategy default changed: recreate → rolling"

### Requirement: Results, events, notifications and history record recovery
The `DeployResult` SHALL include each long-running service's effective strategy. On rollout failure it SHALL include a `recovery` object (`status`: `restored`, `failed`, `disabled`, or `unsupported`) and the action `restored` if recovery succeeded. Events, failure notifications, and the history record SHALL carry the same status.

#### Scenario: Restored deploy result document
- **WHEN** a recreate service's rollout fails and is restored
- **THEN** the `DeployResult` has `status: "failed"`
- **AND** the service outcome has `action: "restored"`, `strategy: "recreate"`, and `recovery.status: "restored"` with the restored revision
- **AND** events `deploy.service.failed`, `deploy.service.restore.started`, and `deploy.service.restored` are emitted in that order
- **AND** the failure notification states that the previous revision was restored
- **AND** the failed history entry records the service's strategy and recovery status

#### Scenario: Rolling failure reports old revision still serving
- **WHEN** a rolling service's new revision fails readiness with `rollbackOnFailure` defaulted
- **THEN** the service outcome has `action: "restored"` and `recovery.status: "restored"`
- **AND** no restart of old containers is performed, because they never stopped

#### Scenario: Recovery data contains no secrets
- **WHEN** a recovery outcome, event, or notification is emitted
- **THEN** it contains revision IDs, node names, container names, and error text only, and no environment values or secret material
