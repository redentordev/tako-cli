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
| plan deploy logs exec state |
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
replication and storage behavior.

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
first and can fall back to remote healthy containers over the mesh.

## Switching Computers

```bash
git clone <repo>
tako state pull -e production
tako state status -e production
tako deploy -e production
```

By default, state commands read from the active environment's primary node. Use
`--server <name>` to read from another environment node when recovering from a
machine change or a primary-node outage.

## CI/CD

CI uses the same path as a laptop:

```text
CI runner
  checkout
  tako state status
  tako deploy --yes
       |
       v
  connect to the environment state node
       |
       v
  acquire remote lease + reconcile selected nodes
```

Deploy, rollback, and destroy acquire a remote lease under
`/var/lib/tako-cli/<project>/lease`. CI and local machines compete for the same
lease, so concurrent operations fail fast instead of racing. The local `.tako`
lock remains as a same-machine guard.

## Implementation Order

```text
1. Keep the CLI surface takod-only.
2. Add CI-friendly remote deploy leases and state pull/status workflows.
3. Persist desired revisions and events on every node.
4. Reconcile WireGuard peer material and node configs.
5. Move ingress to per-node proxies with mesh upstream fallback.
6. Promote reconcile/state operations to the takod socket.
```
