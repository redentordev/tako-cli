# Tako Orchestration Model

Tako has one job: run an app on owned machines with the same workflow from one
node to many nodes.

There is one runtime path:

```text
tako CLI -> takod -> local Docker
               |
               +-> mesh
               +-> local proxy
               +-> replicated state
               +-> health + reconciliation
```

Tako does not expose multiple orchestration modes. Single-node deployments are
one-node meshes, and multi-node deployments use the same takod reconciliation
path.

## Mental Model

```text
Git repo
  tako.yaml
  app source
     |
     v
+-----------------------------+
| tako CLI                    |
| deploy logs state scale     |
+--------------+--------------+
               |
               v
       connect to any node
               |
               v
+-----------------------------+
| takod on each server        |
| desired revision            |
| local Docker reconciliation |
| event log + state snapshot  |
| proxy upstream table        |
+--------------+--------------+
               |
               v
        private mesh peers
```

One node is a mesh with one member. Adding a second node should not change the
shape of config or day-to-day commands.

## App And Stage Identity

Tako treats `project.name` as the app name and the selected environment as the
stage. That pair is the isolation boundary for deployment history, desired
state, actual snapshots, leases, env bundles, Docker labels, proxy files,
networks, containers, and generated volume names.

Multiple unrelated projects can share the same server when they use distinct
app/stage pairs. The node-local proxy is intentionally shared because only one
process can own ports 80 and 443, but each app/stage writes its own dynamic
proxy file and service routes. Runtime Docker artifacts include a deterministic
short identity hash so similar names such as `prod_api/web` and
`prod/api_web` cannot collapse into the same container, network, proxy, or
volume name.

The operational rule is the same as SST's app/stage model: keep app names
stable and unique per product, and use environments as stages such as
`production`, `staging`, or `preview`.

## Config Contract

```yaml
runtime:
  mode: takod
  agent:
    enabled: true
    socket: /run/tako/takod.sock
    dataDir: /var/lib/tako

state:
  backend: replicated
  deployConsistency: lease
  onUnreachableNode: block
  remoteCacheEnabled: true

mesh:
  enabled: true
  networkCIDR: 10.210.0.0/16
  interface: tako
  listenPort: 51820
  subnetBits: 24
  natTraversal: true
```

During runtime preparation, Tako installs WireGuard tools when missing, creates a
stable node key under `/etc/tako/wireguard`, writes `/etc/wireguard/tako.conf`,
and brings up `wg-quick@tako`. One-node deployments still get the same
interface, just without peer blocks.

The node-local `takod` process listens on `/run/tako/takod.sock`. On released
CLI builds, `tako setup`, `tako deploy`, `tako scale`, and `tako rollback`
download the matching Linux release binary for the server architecture when the
node agent is missing or running a different version, install it at
`/usr/local/bin/tako`, and restart the systemd service. Development builds reuse
an existing server binary when one is already installed. For local agent smoke
tests, set `TAKO_TAKOD_BINARY` to a Linux tako binary to upload that binary
before runtime preparation.

`takod` exposes health, status, actual container discovery, service container
reconcile, proxy file updates, proxy container reconcile, logs, stats, metrics,
access logs, volume backups, acme-dns state, mesh metadata, and project cleanup
from node-local state. Runtime workflows ask the local agent to remove, pull,
run, verify containers, publish proxy config, persist credentials, stream logs,
read metrics, and clean project resources through typed socket requests. The
CLI may still use SSH as a transport to reach the Unix socket, but runtime Docker
inspection and mutation belong to `takod`.

Installed `takod` services also refresh node-local actual container state in the
background. The loop is lease-aware: if a mutating operation holds the
environment lease, the background refresh skips that project/environment until
the lease is clear.

Local `.tako` files are cache and UX acceleration. The durable truth lives in
Git plus the last accepted desired revision and event log replicated by takod.

## Runtime Flow

```text
+-------------------+
| Desired State     |
| tako.yaml + git   |
+---------+---------+
          |
          v
+-------------------+
| Plan              |
| desired vs remote |
| vs local actual   |
+---------+---------+
          |
          v
+-------------------+
| Lease             |
| one deploy writer |
+---------+---------+
          |
          v
+-------------------+
| Distribute        |
| image + revision  |
+---------+---------+
          |
          v
+-------------------+
| Reconcile         |
| each takod node   |
+---------+---------+
          |
          v
+-------------------+
| Route             |
| healthy upstreams |
+-------------------+
```

## Node State

Each node stores enough state to stand alone:

```text
/var/lib/tako/
  node.json
  desired/
    <project>/<env>/revision.json
  actual/
    <project>/<env>/containers.json
    <project>/<env>/nodes/<node>.json
  events/
    <project>/<env>.jsonl
  mesh/
    peers.json
```

If a node loses contact with the CLI or peers, it keeps serving its last accepted
revision. New deployments obey `state.onUnreachableNode`.

## Placement

```text
global:
  one instance on every selected node

replicated:
  N instances spread across selected nodes

pinned:
  instances stay on named nodes

spread:
  spread replicas across selected nodes, optionally filtered by node label constraints
```

Named Docker volumes are node-local by default. If a volume already exists on a
node, Tako schedules the service there unless placement explicitly says
otherwise. Services sharing the same local volume are co-located, and
non-global replicas that would write the same local volume from multiple nodes
are rejected. Mark a top-level volume `external: true` or `replicated: true`
only when an external storage layer or the application handles safe multi-node
writes.

Volume backup and restore follow the same app/stage boundary. The CLI resolves
`SERVICE VOLUME` through the service's configured mounts, skips bind mounts, and
sends `takod` both a safe backup key and the real Docker volume name. `takod`
backs up or restores only generated app/stage volumes or custom named volumes
with matching Tako ownership labels; missing custom volumes are not created
during restore.

## Deploy Strategy

`recreate` remains the default. Services can opt into rolling replacement:

```yaml
deploy:
  strategy: rolling
  order: start-first
  maxUnavailable: 0
  monitor: 30s
```

For `start-first`, `takod` starts a temporary replacement container, waits for
it to become healthy, then removes the old slot and renames the replacement into
the stable slot name. If Docker rejects the temporary container because a host
port is already bound, `takod` falls back to `stop-first`: it stops the old slot
to free the port, starts and health-checks the replacement, and restarts the old
slot if the replacement fails.

When a replacement fails its health check, `takod` removes only the failed
temporary replacement and leaves the existing slot in place. Health-check
failures include recent container logs in the error returned to the CLI.
`tako ps` preserves total running replica counts but also shows a health column
and marks services with unhealthy replicas as `unhealthy`; `tako inspect` shows
the per-container health state for slot-level debugging.

## Mesh + Ingress

```text
             DNS
              |
       +------+------+
       |             |
       v             v
  proxy@node-a  proxy@node-b
       |             |
       | local first |
       v             v
   web@node-a    web@node-b
       \           /
        \  mesh   /
         v       v
          web@node-c
```

Every ingress node runs a local proxy. It routes to local healthy containers
and remote healthy containers through node-local mesh-only upstream ports.
One-node deployments use the same proxy path with only local upstreams and do
not publish mesh host ports. Multi-node upstream ports are allocated and
recorded by the target node's `takod` agent. The CLI sends a deterministic
app/stage/service/slot preferred port, but `takod` checks existing Docker port
bindings and its allocation registry before accepting it, then returns the
actual assigned port for container publish and proxy rendering. This lets
unrelated apps with common service names such as `web` share the same server
without taking each other's mesh upstream port.

For services without `proxy`, `port` is still the container port used by health
checks and service-to-service networking, but it is not published on the host.
This keeps databases, queues, and internal APIs private by default.

`ports[]` is the explicit form for services that have more than one listener or
need a host bind. Each entry has a stable name and a container `target` port.
`mode: proxy` routes HTTP/HTTPS through Tako's shared ingress, `mode: internal`
keeps the port private to the app/stage network, and `mode: host` publishes the
port on the selected node after reserving it through `takod`.

```yaml
ports:
  - name: http
    target: 3000
    protocol: http
    proxy:
      domain: app.example.com
  - name: metrics
    target: 9090
    protocol: tcp
    internal: true
  - name: dns
    target: 5353
    published: 53
    protocol: udp
    mode: host
    hostIP: 10.210.0.0/16
```

Host binds may use a concrete IP or a CIDR. For a CIDR, the deployer resolves
the node's public IP first and then its mesh IP. `takod` rejects allocations that
collide with existing Docker bindings or Tako's per-node allocation registry, so
unrelated apps can share the same server without silently stealing ports from
each other.

## Internal Discovery

Every service container receives private Docker network aliases scoped to its
app/stage network:

```text
SERVICE
SERVICE.tako.internal
SERVICE.ENV.PROJECT.tako.internal
```

The short `.tako.internal` name is intentionally scoped by the Docker network,
so unrelated projects on the same node cannot resolve or reach it. The full name
is stable across app/stage clones and works from any service attached to that
same app/stage network. When multiple healthy replicas are connected to the
same node-local network, Docker DNS returns multiple A records and clients can
round-robin across them.

For operator inspection, `tako discovery [SERVICE]` asks each configured
environment node's `takod` for its local healthy endpoints and merges the
responses. `takod` only returns containers that are running, healthy, and
attached to the app/stage Docker network; unreachable nodes do not contribute
stale endpoints. Use `--port` to inspect a specific target port or
`--round-robin` to rotate endpoint order.

Global or otherwise co-located services get local-first behavior naturally
because each node resolves the replicas attached to its own app/stage network.
Cross-project discovery stays private by default. Services must declare
explicit exported named ports, and consumer or edge projects declare top-level
import aliases that point at project, environment, service, and exported port.
`tako discovery --import ALIAS` reads the exporting project's desired state,
validates the named export, then queries live `takod` discovery for healthy
endpoints. Exported internal ports are published on mesh-local host ports so a
dedicated edge node can use mesh-reachable upstreams instead of Docker bridge
addresses. `--format upstreams` renders those rows as HTTP upstream URLs for
edge config workflows such as Caddy environment placeholders.

## Metrics

`tako metrics` keeps the human-readable node view. `tako metrics --prometheus`
prints Prometheus exposition text collected through each node's takod Unix
socket:

```bash
tako metrics --prometheus
tako metrics --prometheus --once
tako metrics --prometheus --server node-a
```

The underlying endpoint is `GET /v1/metrics?format=prometheus`. It is not bound
to a public TCP listener; the CLI reaches it over SSH and the node-local takod
socket. Scrapes include node CPU, memory, disk, load, network, disk IO, takod
uptime, service running/desired replica gauges for the selected project/stage,
latest deployment status, and lease state.

## Node Inspection

Node-level operator commands stay on the takod socket path:

```bash
tako inspect [SERVICE]
tako inspect [SERVICE] --server node-a
tako node logs [NODE]
tako node logs [NODE] --unit tako-monitor --tail 200
tako mesh rtt
tako mesh rtt --server node-a --count 5
```

`inspect` asks each selected node's takod for scoped container details and
prints node, service, replica slot, container ID, state, health, image, ports,
and mounts. It intentionally does not return container environment variables.

`node logs` streams an allowlisted systemd unit from the target node. It
defaults to `takod` and also supports `tako-monitor`; it does not accept
arbitrary unit names. `mesh rtt` asks each selected node's takod to ping peer
WireGuard mesh IPs and reports average RTT plus packet loss. Both commands work
for one node or many nodes without exposing a public debug listener.

## Switching Computers

```bash
git clone <repo>
tako state pull -e production
tako state status -e production
tako deploy -e production
```

By default, state commands read all configured environment nodes and select the
freshest deployment history, desired revision, aggregate actual snapshot, and
node-local actual snapshots they can reach. Use `--server <name>` only for
focused one-node debugging. When nodes disagree or a state copy was lost, run
`tako state repair -e production`; it writes the freshest deployment history,
desired revision, aggregate actual snapshot, and node-local actual snapshots
back to the reachable mesh, and refreshes local `.tako` state when deployment
history is available.

## Build Source

The default build source is the local worktree:

```yaml
deployment:
  source: local
```

This is best for fast iteration. If the worktree has uncommitted changes, deploy
continues but prints a warning because deployment history still points at the
current `HEAD` commit while the built image includes local edits.

Use committed Git source for CI and production workflows that must match the
repository exactly:

```yaml
deployment:
  source: git
```

With `source: git`, Tako refuses dirty worktrees, creates build contexts from
committed `HEAD` content, then applies the committed `.dockerignore` before
streaming the archive to `takod`. Untracked files, ignored local artifacts,
`node_modules`, stale `.next` output, and local `.env` files are not included
unless they are committed and not excluded by `.dockerignore`.

## Build Platform

Services can set `platform` when a build must target a specific Linux
architecture:

```yaml
services:
  web:
    build: .
    platform: linux/amd64
```

The CLI forwards the platform to `takod`, and `takod` runs Docker with
`--platform`. The field is persisted in desired state and included in safe
config hashes, so changing it reconciles like any other build-affecting config
change.

Before building or reconciling a platform-scoped service, the CLI asks each
assigned `takod` node for its Docker platform through `/v1/node/info`. If
`platform` is set, every assigned node must report the same platform. If a
`build:` service does not set `platform`, Tako verifies that the source build
node and assigned runtime nodes report one matching platform. This avoids
silently building one architecture and distributing it to incompatible nodes.
Per-platform multi-arch manifest publishing is still future work.

## Build Cache

Build cache is configured under `deployment.cache`:

```yaml
deployment:
  cache:
    enabled: true
    type: local
```

`type: local` is the default. The CLI passes the service image as
`cacheFrom`, and `takod` only adds `docker build --cache-from` when that image
already exists on the build node. First deploys do not fail just because there
is no previous image; later deploys can reuse node-local layers.

Registry cache uses Docker buildx:

```yaml
deployment:
  cache:
    enabled: true
    type: registry
    ref: ghcr.io/acme/my-app/buildcache
    builder: mesh-builder
```

For `type: registry`, `takod` runs `docker buildx build --load` with
`--cache-from type=registry,ref=...` and
`--cache-to type=registry,ref=...,mode=max`. If buildx is missing on the build
node, setup tries to install it from OS packages. Hosts without a buildx package
still fail before service reconciliation with a clear buildx requirement error.
Registry cache references are not stored in desired runtime state as secrets,
and they should not contain credentials; use normal registry auth paths for
private registries.

## Private Registry Pulls

For prebuilt private images, configure one registry credential set:

```yaml
registry:
  url: ghcr.io
  username: ${REGISTRY_USER}
  password: ${REGISTRY_TOKEN}

services:
  api:
    image: ghcr.io/acme/api:2026-06-14
```

The CLI expands environment placeholders locally and sends matching credentials
only in the takod request body over SSH stdin. Takod writes a temporary
`DOCKER_CONFIG`, runs `docker pull`, and removes the config immediately after
the pull. Credentials are not written to desired state, labels, command
arguments, or deployment history. This first pass covers prebuilt service images
used by deploys, hooks, and `tako run --one-off`; private base images for
`build:` services still need build-time auth support.

## CI/CD

CI uses the same path as a laptop:

```text
CI runner
  checkout
  tako state status
  tako deploy --yes
       |
       v
  connect to every target environment node
       |
       v
  acquire remote leases + reconcile selected nodes
```

Deploy, rollback, scale, env push, remove, destroy, and state repair acquire
remote leases through `takod` on the target nodes before mutating runtime or
state. CI and local machines compete for the same per-node leases, so
concurrent operations fail fast instead of racing. The local `.tako` lock
remains as a same-machine guard.

## Implementation Status

```text
Done:
1. CLI runtime operations go through takod.
2. Mutating runtime and state operations share remote leases.
3. State pull/status and env push/pull support clone and CI workflows.
4. Desired revisions, actual snapshots, and events persist on nodes.
5. WireGuard peer material and node configs reconcile through takod.
6. Per-node proxies render mesh upstreams from desired and actual state.
7. State repair can rebuild deployment history and runtime state across reachable mesh nodes.
8. Mutating operations acquire leases across their target nodes.
9. `tako image prune --force` removes only app-owned images that are not used
   by app containers on the target node; Docker still protects images used by
   unrelated containers.

Next:
1. Evaluate background peer anti-entropy after the explicit repair workflow is proven.
2. Expand e2e validation across one-node and multi-node meshes.
```
