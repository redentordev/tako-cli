# Self-Healing

Self-healing lets takod repair a container on its own node, without anyone
running a command. It restarts a container that stays unhealthy and starts a
container that crashed and that Docker will not restart. It is **off by
default**.

Self-healing is node-local. It never moves a workload to another node, never
creates a container, and never changes desired state, deployment history or
the proxy. See [Orchestration Model](ORCHESTRATION-MODEL.md) for what Tako
does when a whole node is lost.

## Enabling it

Add `selfHealing` to an environment (defaults for its services) or to a
service (per-key override), then deploy:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    selfHealing:
      enabled: true
    services:
      web:
        image: ghcr.io/example/web:1.4.2
        port: 3000
        healthCheck:
          path: /health
          interval: 15s
        selfHealing:
          unhealthyThreshold: 4
      db:
        image: postgres:16
        persistent: true
        volumes:
          - pgdata:/var/lib/postgresql/data
        selfHealing:
          enabled: true
          restartUnhealthy: false
          startExited: true
```

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Turn self-healing on. |
| `restartUnhealthy` | `true` | Restart a running container that stays unhealthy. |
| `startExited` | `true` | Start a crashed container that Docker will not restart. |
| `unhealthyThreshold` | `3` | Consecutive failed checks before a restart (1-20). The failures must also span at least 60s. |
| `maxRestarts` | `3` | Actions per service within `window` before self-healing gives up (1-20). |
| `window` | `30m` | Give-up window (5m-24h). A container healthy for a full window starts over. |

Each key falls back from the service to the environment, then to the default.
Previews inherit the block from their base environment like any other key.

**Persistent services** (`persistent: true`) ignore the environment's
`enabled`. Only the service's own `enabled: true` turns healing on, because
restarting a database during crash recovery can make things worse.

`selfHealing` is not part of a container's identity. Enabling, tuning or
disabling it recreates no container. The new policy reaches the node with the
next desired-state write (deploy, rollback, promote, scale, start or stop).
Jobs and `run` workloads are never healed; `selfHealing` on them is a
validation error.

**Upgrade first.** Every target node must advertise `node.self-healing-v1`.
A deploy with an enabled policy fails before any mutation (exit 2) when a
node lacks it. Run `tako upgrade servers`, then enable the policy.

## What takod does

takod records the containers of each service after every successful
reconcile (a *converged record*: image, revision and the container names and
IDs it started). It only ever touches containers listed in a valid record,
so retained, failed, warming and hand-started containers are never touched.
A record stops being valid when the desired image changes, while a rollout
is in progress, or when a container's ID, name or revision label no longer
matches.

| Container | Action |
|---|---|
| Running and unhealthy for `unhealthyThreshold` checks spanning 60s or more | `docker restart` |
| Exited with a non-zero code or OOM-killed, restart policy `no` or `on-failure` | `docker start` |
| Exited with code 0 under `no` or `on-failure` | none: reported as `completed` |
| Exited under `always` or `unless-stopped` | none: reported as `stopped` (someone stopped it) |
| Created, paused or dead | none: reported as `stopped` |
| Being restarted by Docker | none: reported as `docker-restarting` |
| Missing | none: reported as `missing`; run `tako deploy -e <env> --service <svc>` |

**Exit 0 versus a crash.** Under `restart: no` and `on-failure`, Docker
treats exit code 0 as a finished process. Tako does the same, so a one-shot
worker that completed stays down. A non-zero exit (including a manual
`docker kill`) is a crash and is started again. Use `tako heal pause` or
`tako stop` to keep such a container down.

**Restart policies.** With the default `unless-stopped`, Docker already
restarts crashes. A container that is exited under `always` or
`unless-stopped` was stopped on purpose, so self-healing reports it and
leaves it alone. The restart action is what self-healing adds for these
policies.

**Health sources.** Self-healing observes exactly the check the rollout
gates on. When `deploy.readiness` sets a `path` or `tcpPort`, it replaces
`healthCheck`'s path and port (its `interval` and `timeout` apply), just as
in the rollout. The order is the rollout's: an HTTP path first, then
`healthCheck.command`, then a TCP port.

- An HTTP path or a TCP check: takod probes each recorded replica directly,
  every check interval (default 30s, at least 10s), with the same checks the
  rollout uses (no smoke check), at most 4 at a time. One hung replica is
  restarted while its healthy siblings are left alone.
- `healthCheck.command` (without an HTTP path): Docker's health status.
- No explicit check: crashes only; running containers are never restarted.
  The implicit TCP readiness of a defaulted rolling service is a rollout
  gate only.

A probe that cannot judge the container (its address is unknown, or a result
is older than three intervals) counts as unknown. Unknown never triggers an
action.

**Grace.** No action until `max(healthCheck.startPeriod, 2m)` after the
container started and until 5m after the record was written.

## Eligibility

takod skips an environment while another operation holds its lease (deploy,
rollback, scale, preview teardown and so on), while a controller operation
fences the worker, during a node upgrade or a pending volume restore, while
the node is not schedulable or has a deployment-deny latch, and while a pause
is active.

It skips a service when it is a job or run, removal is pending, no replica
is assigned to the node, restore-on-failure retained its previous
containers, a backup runs in the environment, a `<service>-maintenance`
container runs, its record is missing, converging or stale, or it gave up.

Every check is repeated right before acting, under the environment lease
(operation `self-heal`, TTL 5m, held only for the one docker call), the
node's maintenance barrier and mutation lock, and, on enrolled nodes, the
worker fence admission lock. A fresh `docker inspect` must confirm the
container's ID, name, revision, restart policy and condition. A path or TCP
restart re-probes once more.

`docker restart` runs with `-t` set to the service's `stopGracePeriod`
(Docker's 10s when unset), capped at 30s, so the stop phase stays short.
The docker call itself is bounded by 3m; if it times out, takod keeps the
lease until a fresh inspect shows the container running again (up to 60s
more), and otherwise leaves the lease to expire at its TTL rather than
letting a deploy race a daemon that is still working.

A CLI operation that finds a `self-heal` lease waits for it until the lease
expires (at most 5m) before reporting the lock (exit 3).

**Upgrading.** CLIs older than this release do not wait on `self-heal`
leases: a deploy that meets one fails at once with the lock (exit 3) and can
simply be retried. Upgrade the CLI together with `tako upgrade servers`.

## Limits

- **Cooldown:** after an action, a container waits 2m, doubling per action
  (4m, 8m, ...) up to 30m.
- **Give-up:** after `maxRestarts` actions within `window`, the service is
  `gave_up` on that node. It clears when a deploy writes a new revision, or
  with `tako heal reset`.
- **Serial:** one action at a time per node; the next replica of a service
  waits until the first is healthy again or its cooldown ended.
- **Circuit breaker:** when more than half of the node's healed services
  (at least 2) need an action at once, or more than 6 actions ran within
  10m, healing on the node stops for 30m. Mass failure points to the host,
  the network or a shared dependency, where restarts make things worse.
  `tako heal reset` (without `--service`) closes the breaker.
- **All local replicas down:** when every local replica of a service with
  two or more replicas on the node needs an action at once, the cause is
  most likely shared (a database, the network, an upstream). Nothing is
  restarted: the service shows `blocked` with skip
  `all_replicas_unhealthy`, the decision is journaled once and a
  `replicas_unhealthy` alert opens until a replica is healthy again. A
  service with a single replica on the node has no sibling to compare with
  and is healed normally.
- **Jitter across nodes:** once a condition holds past grace, each node
  waits a fixed per-service delay derived from its node identity and the
  service (spread over `unhealthyThreshold` × the check interval, at most
  2m) before acting, so a shared dependency going down does not make every
  node restart its replica in the same tick.

## Pausing during incidents

```bash
tako heal pause                         # whole environment, 1h
tako heal pause --service web --for 4h  # one service, at most 168h
tako heal pause --until-resume          # until resumed
tako heal resume
tako heal reset --service web           # clear give-up, counters, cooldowns
```

Pause, resume and reset run on every server of the environment (or
`--server`) under the environment's operation lease. A pause waits for an
action already running; when the command returns, no action runs until the
pause ends. Pauses survive takod restarts.

## Reading `tako heal status`

```bash
tako heal status
tako heal status --service web --journal 50
tako heal status --output json          # HealStatus document
```

Each row is one service on one node:

| State | Meaning |
|---|---|
| `ok` | Containers are healthy or running. |
| `watching` | A container is unhealthy or crashed and is being counted, or an action is due. |
| `cooldown` | Waiting after an action. |
| `gave_up` | The action budget is used up; see the journal, fix, then deploy or reset. |
| `paused` | A pause covers the service. |
| `blocked` | Skipped; the skip column says why (for example `lease_held`, `retention`, `maintenance`). |
| `not-converged` | No valid record yet (deploy the service once with self-healing enabled). |

The journal lists decisions with reason codes such as `exited_nonzero`,
`oom_killed`, `docker_unhealthy`, `http_status`, `http_timeout` and
`connect_failed`, plus numbers such as the exit code or HTTP status. Error
text and container logs are never recorded or sent.

`tako ps` adds an additive `heal` object to each service with a policy and
appends `heal:gave_up` or `heal:paused` to the status cell.

Exit codes: 0 ok or disabled; 6 a service gave up, a breaker is open or a
node could not be read; 4 every node unreachable; 2 no node supports
`node.self-healing-v1`.

## Alerts

With any notification target configured, self-healing reports alert kind
`selfheal` through node-side monitoring (see
[Resource Monitoring](RESOURCE-MONITORING.md#4-alerting)):

| Reason | Subject | Severity | Events |
|---|---|---|---|
| `gave_up` | the service | critical | `heal_gave_up` / `heal_resumed` |
| `circuit_open` | `@node` | critical | `heal_circuit_open` / `heal_circuit_closed` |
| `action_failed` | the service | warning | `heal_action_failed` |
| `replicas_unhealthy` | the service | warning | `heal_replicas_unhealthy` / `heal_replicas_recovered` |

Successful restarts and starts are never alerted. They are journal entries
and `heal.*` state events (`heal.restarted`, `heal.started`, `heal.failed`,
`heal.gave_up`, `heal.reset`, `heal.paused`, `heal.resumed`,
`heal.circuit_open`, `heal.circuit_closed`). They do not appear in
`tako history`.

## Turning it off

Remove `selfHealing` (or set `enabled: false`) and deploy, or run
`tako heal pause --until-resume`. A downgraded takod ignores the policy and
never heals.
