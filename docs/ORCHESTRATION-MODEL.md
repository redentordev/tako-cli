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
CLI builds, `tako setup` downloads the matching Linux release binary for the
server architecture, installs it at `/usr/local/bin/tako`, and restarts the
systemd service. Development builds reuse an existing server binary when one is
already installed, which keeps local iteration from depending on a published
release.

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
  choose nodes by labels and capacity
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

Every ingress node runs a local proxy. It routes to local healthy containers
and remote healthy containers through deterministic mesh-only upstream ports.
One-node deployments use the same proxy path with only local upstreams.

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
6. Per-node proxies render mesh upstreams from desired and actual state.
7. State repair can rebuild deployment history and runtime state across reachable mesh nodes.
8. Mutating operations acquire leases across their target nodes.

Next:
1. Evaluate background peer anti-entropy after the explicit repair workflow is proven.
2. Expand e2e validation across one-node and multi-node meshes.
```
