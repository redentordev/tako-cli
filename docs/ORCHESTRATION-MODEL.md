# Tako Orchestration Model

Tako has one job: run an app on owned machines with the same workflow from one
node to many nodes.

There is one runtime path:

```text
tako CLI -> takod -> local Docker
               |
               +-> mesh
               +-> local proxy (Traefik-backed tako-proxy)
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
app/stage pairs. The node-local Traefik-backed `tako-proxy` is intentionally
shared because only one process can own ports 80, TCP 443, and UDP 443 for
HTTP/3, but each app/stage writes its own dynamic proxy file and service routes.
Runtime Docker artifacts include a deterministic short identity hash so similar
names such as `prod_api/web` and
`prod/api_web` cannot collapse into the same container, network, proxy, or
volume name.

Proxy routes also target deterministic project/stage-scoped container aliases,
not generic service DNS names. This matters because the shared proxy can be
attached to several app networks at once; `web` may exist in many projects, but
`tako-myapp-production-web-1-...` resolves to one intended upstream.

Cross-project imports use a separate service-scoped export network. A service
with `export: true` is attached to an export network with a readable alias such
as `backend-api-production-api`; consumers declare `imports: [backend-api.api]`
and are attached only to that exported service network. This keeps non-exported
services in the provider project network private. Export networks are labeled
with `tako.discovery=export`, app, environment, service, and alias metadata so
future discovery tooling can inspect Docker state without guessing from network
names.

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

`remoteCacheEnabled` must remain `true` in the current model. Local `.tako`
files are a cache; deployment history and runtime revisions have to be
replicated to `takod` so another laptop or CI runner can reconcile from the same
state.

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
before runtime preparation. Operators can also run `tako upgrade servers` to
explicitly patch stale server-side agents, refresh the setup manifest, and
verify `/v1/status` before reconciling application services.

`takod` exposes health, status, actual container discovery, service container
reconcile, proxy file updates, proxy container reconcile, logs, stats, metrics,
access logs, volume backups, mesh metadata, and project cleanup from node-local
state. Runtime workflows ask the local agent to remove, pull, run, verify
containers, publish proxy config, persist credentials, stream logs, read
metrics, and clean project resources through typed socket requests. The CLI may
still use SSH as a transport to reach the Unix socket, but runtime Docker
inspection and mutation belong to `takod`.

Installed `takod` services also refresh node-local actual container state in the
background. The loop is lease-aware: if a mutating operation holds the
environment lease, the background refresh skips that project/environment until
the lease is clear.

Local `.tako` files are cache and UX acceleration. The durable truth lives in
Git plus the last accepted desired revision and event log replicated by takod.

## Local Docker Compatibility

Local development only needs a Docker-compatible daemon when Tako has to build
or inspect images from the machine running the CLI. Docker Desktop and Colima
work for that path as long as the active Docker context answers `docker info`.
Rootless Docker can also be used locally for builds when the current user can
run normal `docker build`, `docker pull`, and `docker save` commands.

Remote `takod` hosts have stricter requirements than a laptop build context.
Setup and runtime reconciliation currently assume a Linux server with Docker
available to `takod`, systemd for the agent service, and enough privileges to
configure WireGuard, firewall rules, published ports, and the shared proxy. A
fully rootless remote server mode is not implemented yet; treat rootless Docker
on remote deployment nodes as experimental until the live mesh checklist proves
setup, proxy ports, WireGuard routing, volumes, and service reconciliation on
that host.

For build-based services in a multi-node environment, Tako builds the image on
the selected source node through that node's `takod` socket, then brokers a
stream from the source node's `takod` image export endpoint into each peer
node's `takod` image import endpoint over the CLI's existing SSH sessions. Peer
nodes do not need the operator's private key on disk, and runtime Docker
save/load still runs only inside node-local agents. This is full-image transfer;
layer-delta peer distribution is still future work.

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

Stateful services default to pinned unless the config explicitly defines
replication, placement, and external persistence behavior.

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

Every selected environment node with public routes runs the shared node-local
proxy by default. `environment.proxy.placement` can narrow ingress to dedicated
edge nodes with pinned servers or node-label constraints while service
containers keep their own placement. Built-in ACME TLS currently requires the
proxy placement to resolve to one node; multi-edge certificate issuance and
storage is blocked at config validation until distributed certificate handling
is implemented. Public proxy domains must be explicit hostnames; wildcard
hostnames such as `*.example.com` are blocked until DNS-01 certificate handling
is implemented in the generated Traefik proxy config. The proxy routes to local
containers through Docker DNS and remote containers through node-local mesh-only
upstream ports. Health is enforced by the generated Traefik service health
checks when configured.
One-node deployments use the same proxy path with only local upstreams and do
not publish mesh host ports. Multi-node upstream ports are allocated and
recorded by the target node's `takod` agent. The CLI sends a
deterministic
app/stage/service/slot preferred port, but `takod` checks existing Docker port
bindings and its allocation registry before accepting it, then returns the
actual assigned port for container publish and proxy rendering. This lets
unrelated apps with common service names such as `web` share the same server
without taking each other's mesh upstream port.

The built-in load balancer strategies are intentionally narrow:
`round_robin` uses Traefik's default load balancing, and `sticky` enables secure
HTTP-only cookie stickiness for session-affine or WebSocket-heavy workloads.
Other algorithms are rejected at config validation until they are implemented in
the generated proxy config.

For services without `proxy`, `port` is still the container port used by health
checks and service-to-service networking, but it is not published on the host.
This keeps databases, queues, and internal APIs private by default.

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
history is available. When a node has been removed from the environment, deploy
and state repair prune stale per-node actual snapshots for that node after the
fresh target-node state is written. Operators can also run
`tako state forget-node <node> --yes` after removing a destroyed node from
`tako.yaml` to explicitly delete its standalone node snapshot and prune it from
aggregate actual state on reachable nodes before the next deploy.

## CI/CD

CI uses the same path as a laptop:

```text
CI runner
  checkout
  tako upgrade servers --dry-run
  tako upgrade servers
  tako state status
  tako deploy --yes
       |
       v
  connect to every target environment node
       |
       v
  acquire remote leases + reconcile selected nodes
```

Deploy, rollback, scale, maintenance, live, remove, cleanup, destroy, and state
repair acquire remote leases through `takod` on the target nodes before
mutating runtime or state. CI and local machines compete for the same per-node
leases, so concurrent operations fail fast instead of racing. The local `.tako`
lock remains as a same-machine guard.

## Implementation Status

```text
Done:
1. CLI runtime operations go through takod.
2. Mutating runtime and state operations share remote leases.
3. State pull/status and env push/pull support clone and CI workflows.
4. Desired revisions, actual snapshots, and events persist on nodes.
5. WireGuard peer material and node configs reconcile through takod.
6. Per-node proxies render mesh upstreams from desired placement and Traefik health checks.
7. State repair can rebuild deployment history and runtime state across reachable mesh nodes.
8. Mutating operations acquire leases across their target nodes.
9. Proxy setup supports HTTP/1.1, HTTP/2, HTTP/3, WebSocket traffic, and sticky sessions.
10. `tako upgrade servers` explicitly patches stale server-side takod agents.
11. Tag releases publish verified multi-arch Linux CLI images to GHCR.
12. Environment proxy placement can limit public ingress to dedicated edge nodes.
13. Config validation blocks unsafe multi-edge automatic ACME TLS.
14. Build-image distribution is brokered by the CLI between node-local takod agents.
15. Config validation blocks wildcard public hostnames until DNS-01 certificate
    handling is implemented.
16. `tako state forget-node` explicitly prunes retired nodes from replicated
    runtime state.

Next:
1. Add distributed certificate handling for multi-edge deployments.
2. Add DNS-01 certificate handling for wildcard public hostnames.
3. Evaluate background peer anti-entropy after the explicit repair workflow is proven.
4. Add layer-delta peer image distribution.
5. Expand e2e validation across one-node and multi-node meshes.
```
