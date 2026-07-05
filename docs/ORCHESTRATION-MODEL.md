# Tako Orchestration Model

Tako has one job: run an app on owned machines with the same workflow from one
node to many nodes.

There is one runtime path:

```text
tako CLI -> takod -> local Docker
               |
               +-> mesh
               +-> local proxy (Caddy-backed tako-proxy)
               +-> replicated state
               +-> health + reconciliation
```

Tako does not expose multiple orchestration modes. Single-node deployments are
one-node meshes, and multi-node deployments use the same takod reconciliation
path. For planned foundation work around raw inputs, canonical state APIs, and
SDK extraction, see [Foundation Roadmap](FOUNDATION-ROADMAP.md).

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
app/stage pairs. The node-local Caddy-backed `tako-proxy` is intentionally
shared because only one process can own ports 80, TCP 443, and UDP 443 for
HTTP/3, but each app/stage writes its own route manifest and service routes.
Runtime Docker artifacts include a deterministic short identity hash so similar
names such as `prod_api/web` and
`prod/api_web` cannot collapse into the same container, network, proxy, or
volume name.

Proxy routes also target deterministic project/stage-scoped container aliases,
not generic service DNS names. This matters because the shared proxy can be
attached to several app networks at once; `web` may exist in many projects, but
`tako-myapp-production-web-1-...` resolves to one intended upstream.
Each route manifest records its owning app/stage network, so a recreated
`tako-proxy` reconnects to every live network represented by current manifests
before serving the regenerated Caddy config.

Cross-project imports use a separate service-scoped export network. A service
with `export: true` is attached to an export network with a readable alias such
as `backend-api-production-api`; consumers declare `imports: [backend-api.api]`
and are attached only to that exported service network. This keeps non-exported
services in the provider project network private. Export networks are labeled
with `tako.discovery=export`, app, environment, service, and alias metadata so
operators can run `tako discovery exports` without guessing from network names.
This is an inspection path, not mesh DNS/SRV; consumers still opt in with
explicit `imports: [project.service]`.

The operational rule is the same as SST's app/stage model: keep app names
stable and unique per product, and use environments as stages such as
`production`, `staging`, or `preview`.

`project.version` is project metadata, not the redeploy trigger. Deployment
history, desired runtime state, and build-backed image tags use the Git commit
from the clean checkout. Commit source/config changes, then run `tako deploy`;
use `tako deploy --force` only when you need to recreate unchanged app
containers.

## Config Contract

App repos should describe app intent first. A normal one-node or multi-node
project does not need to declare `runtime`, `state`, or `mesh`:

```yaml
project:
  name: web-app
  version: 1.0.0

servers:
  production:
    host: ${TAKO_PRODUCTION_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}

environments:
  production:
    servers: [production]
    services:
      web:
        build: .
        port: 3000
        proxy:
          domain: web.${TAKO_PRODUCTION_HOST}.sslip.io
          email: ${LETSENCRYPT_EMAIL}
        healthCheck:
          path: /
```

Tako infers the same runtime contract for every app:

- `runtime.mode: takod`
- `runtime.proxy: tako-proxy`
- `runtime.agent.enabled: true`
- `runtime.agent.socket: /run/tako/takod.sock`
- `runtime.agent.dataDir: /var/lib/tako`
- `state.backend: replicated`
- `state.deployConsistency: lease`
- `state.onUnreachableNode: block`
- `state.remoteCacheEnabled: true`
- `mesh.enabled: true`
- `mesh.networkCIDR: 10.210.0.0/16`
- `mesh.interface: tako`
- `mesh.listenPort: 51820`
- `mesh.subnetBits: 24`
- `mesh.natTraversal: true`

Use `tako config explain -e <environment>` to see the effective values and
whether each value came from config, environment expansion, or a Tako default.
Add explicit `runtime`, `state`, or `mesh` blocks only for intentional advanced
overrides.

`runtime.proxy` must remain `tako-proxy` in the current model. It is the
Caddy-backed built-in ingress proxy that publishes HTTP on TCP 80, HTTPS on
TCP 443, and HTTP/3 on UDP 443. `remoteCacheEnabled` must remain `true` in the
current model. Local `.tako` files are a cache; deployment history and runtime
revisions have to be replicated to `takod` so another laptop or CI runner can
reconcile from the same state.

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

The default build strategy is `deployment.build.strategy: remote`. For
build-backed services, the CLI validates and archives the local build context,
then streams it to each assigned remote `takod` node where Docker performs the
image build. The local machine still needs the source files, an accessible build
directory, and either a Dockerfile or Nixpacks when no Dockerfile exists.
`tako doctor --skip-remote` reports those local build inputs without contacting
servers.

`deployment.build.strategy: local` uses the developer or CI machine as the
builder. Tako detects each assigned node's architecture, groups targets by
`linux/amd64` or `linux/arm64`, runs `docker buildx build --platform ... --load`
once per architecture, and transfers the loaded image to every assigned target
with psviderski/unregistry's `docker pussh` plugin. `auto` tries this local path
first and falls back to the remote takod builder if local Docker, buildx,
docker-pussh, SSH key/agent auth, or remote Docker prerequisites are not ready.

Local build mode requires Docker CLI plugin support and `docker pussh` on the
client machine. The remote SSH user must be able to run Docker directly or with
passwordless `sudo docker`, matching upstream docker-pussh requirements. Password
only SSH auth from `tako.yaml` is not usable by docker-pussh because it uses the
system OpenSSH client.

Docker Desktop, Colima, and rootless Docker can be used as the local builder
when buildx can produce the target platform and load it into local Docker. In
remote build mode, the active local Docker context is not used by deploy.

Remote `takod` hosts have stricter requirements than a laptop build context.
Setup and runtime reconciliation currently assume a Linux server with Docker
available to `takod`, systemd for the agent service, and enough privileges to
configure WireGuard, firewall rules, published ports, and the shared proxy. A
fully rootless remote server mode is not implemented yet. `tako setup` verifies
that rootful system Docker is reachable through `sudo docker info`, and
`tako doctor` reports the same Docker runtime mode and compares reachable
server-side takod agent versions with the running CLI. For environments with
public routes, `tako doctor` also inspects the live shared proxy container and
verifies the required Caddy config watcher, TCP 80/443 publishes, UDP 443
publish for HTTP/3, and certificate, runtime-config, route-manifest, and
access-log mounts.
It also reads replicated deployment/runtime state through takod and reports
whether deployment history, desired state, aggregate actual state, node-local
actual snapshots, and the remote operation lease are healthy.
Rootless Docker on remote deployment nodes is blocked until the live mesh
checklist proves setup, proxy ports, WireGuard routing, volumes, and service
reconciliation on that host.

Dockerfile syntax is evaluated by the remote Docker builder, not by the local
laptop. If a Dockerfile uses BuildKit-only features such as `COPY --chmod`, the
target node must have working BuildKit/buildx support. When a remote builder is
legacy-only or has a broken buildx install, takod surfaces the Docker output and
adds a hint to either install/repair buildx on the node or replace the
BuildKit-only syntax with portable steps such as `RUN chmod`.

For build-based services in a multi-node environment, remote build mode builds
the image on each assigned node, skipping nodes where the exact commit image
already exists. Local build mode builds once per target architecture on the
client and pushes directly to every assigned node through docker-pussh. The
docker-pussh path starts a temporary unregistry container on the target over SSH
and transfers only missing layers; when the target Docker daemon does not use
the containerd image store, upstream docker-pussh pulls the temporary registry
image back into Docker's classic image store so takod can run it.

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

## Deployment Strategies

`recreate` is the conservative default. Tako replaces the service containers
that match the app, stage, and service identity, then reconciles proxy routes.

`rolling` is supported for stateless services as a full-revision rollout. Tako
starts the replacement revision side-by-side, waits for `deploy.readiness` when
configured, points proxy routes at the new revision, then prunes stale revision
containers and mesh port leases. Persistent services cannot use `rolling`.
`maxUnavailable` is not accepted yet, and a configured `maxSurge` must be large
enough to warm the full replacement revision.

`blue_green` is supported for stateless public services. With the default
automatic promotion, Tako starts the green revision side-by-side, waits for
`deploy.readiness` when configured, runs `deploy.smokeTest` when configured,
points proxy routes at the green revision only after those checks pass, then
prunes stale revision containers and mesh port leases. Set
`deploy.gracePeriod` to a duration such as `30s` or `2m` to keep the previous
revision running after the proxy switch before stale revision cleanup. With
`promotion: manual`, deploy warms green without moving public traffic, and
`tako promote <service>` switches the route to the warmed revision. Persistent
services cannot use `blue_green`.

## Placement

```text
spread:
  distribute the configured replica count across selected nodes
pinned:
  instances stay on named nodes
global:
  one instance on every selected node
any or omitted:
  no placement preference; replicas are assigned across selected nodes
```

Placement can also be filtered by supported node-label constraints such as
`node.labels.role==web`. Stateful services should use `pinned` placement unless
they are designed for multi-writer operation and external persistence.

For persistent services, placement is part of the lifecycle contract. In a
multi-node environment, `persistent: true` requires `placement.strategy:
pinned` or `global`. `pinned` is the singleton accessory/database shape; `global`
means one independent stateful instance per selected node. Persistent services
do not support `replicas > 1`; scale stateless clients, or use external/clustered
storage when the stateful system itself needs high availability.

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

Every selected environment node with proxy routes runs the shared node-local
proxy by default. `environment.proxy.placement` can narrow ingress to dedicated
edge nodes with pinned servers or node-label constraints while service
containers keep their own placement. Built-in ACME TLS currently requires public
proxy placement to resolve to one node; multi-edge certificate issuance and
storage is blocked at config validation until distributed certificate handling
is implemented. Public proxy domains must be explicit hostnames; wildcard
hostnames such as `*.example.com` are blocked until DNS-01 certificate handling
is implemented in the generated Caddy proxy config. Internal proxy hosts use
`proxy.visibility: internal`, render as HTTP-only Caddy routes such as
`http://admin.production.demo.tako.internal`, and are intended for private
network, VPN, or `/etc/hosts` resolution rather than public DNS/ACME. The proxy
routes to local containers through Docker DNS and remote containers through
node-local mesh-only upstream ports. Health is enforced by the generated Caddy
reverse-proxy health checks when configured.

Deploy success is intentionally separate from public DNS readiness. A deploy
can reconcile containers, routes, state, and proxy config before a domain points
at the edge. After successful reconciliation, the CLI checks public domains for
DNS and TLS readiness and reports `pending_dns`, `wrong_dns`, `pending_tls`, or
`active`; those states are warnings by default and become deploy failures only
with `tako deploy --strict-domains`. The check infers expected DNS targets from
`environment.proxy.placement` and accepts working HTTPS through an external
CDN/proxy as active, so Cloudflare-style routing does not require direct A/AAAA
records to the VPS. Use `--domain-target` for custom CNAME or edge targets and
`tako domains status` to re-check domains without redeploying.

Internal routes are intentionally excluded from public DNS/TLS readiness checks.
Run `tako domains hosts` to print host-file entries for internal proxy hosts.
The command maps each internal host to `servers.<name>.privateHost` when present
and otherwise to the deterministic mesh IP for that proxy node; `--address
private`, `--address mesh`, and `--address ssh` make the target selection
explicit.

Dynamic customer domains use Caddy on-demand TLS with a same-project
`dynamicDomains.ask` endpoint. That endpoint is the domain authority for the
edge node: it must approve only exact domains owned by the current app/stage and
should use an indexed domain lookup so first requests for new domains do not
block on slow scans. Phase 1 allows one dynamic-domain authority per edge node;
explicit-domain projects can still share that node through their own route
manifests.

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
`round_robin` uses Caddy's default load balancing, and `sticky` enables
HTTP-only cookie stickiness for session-affine or WebSocket-heavy workloads.
Other algorithms are rejected at config validation until they are implemented in
the generated proxy config.

For services without `proxy`, `port` is still the container port used by health
checks and service-to-service networking, but it is not published on the host.
This keeps databases, queues, and internal APIs private by default. Internal
proxy routes are a deliberate exception for HTTP services that should be
reachable through the shared proxy on a private address without public DNS.

Stateful image services should be declared with `persistent: true` and at least
one named or external Docker volume. The validator rejects persistent services
without volumes because container filesystems are replaced during reconcile. In
multi-node environments it also rejects persistent services without explicit
`pinned` or `global` placement so node-local data has a known home. Normal
commit deploys leave unchanged image-only services alone, and broad `--force`
skips persistent services; targeted `--service <name> --force` is the explicit
opt-in for recreating one persistent container while retaining its volume.
Persistent services with `replicas > 1` are rejected because two containers
sharing or independently writing node-local data is not a safe generic
deployment model.

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
fresh target-node state is written. That state pruning does not stop containers
on a still-running host after its SSH config is removed. If a node still exists,
clean its services while the node remains in `tako.yaml` with
`tako remove --server <node>`, or temporarily re-add it and run that cleanup
before deleting it from the environment. Operators can run
`tako state forget-node <node> --yes` after removing a destroyed node from
`tako.yaml` to explicitly delete its standalone node snapshot and prune it from
aggregate actual state on reachable nodes before the next deploy.

When a destroyed node is rebuilt with the same logical name, keep it in
`tako.yaml` and run:

```bash
tako setup --server <node> -e production
tako upgrade servers --server <node> -e production
tako state repair -e production
tako deploy -e production --yes
```

That path recreates server runtime, verifies the matching `takod` agent,
rewrites replicated state from the surviving mesh, and reconciles proxy,
WireGuard, lease, and live container state through the normal deploy flow.

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

On shared nodes, destructive commands are app/stage scoped. `remove`,
`destroy`, and default `cleanup` target resources identified by the current
`project.name` and environment, plus explicit service image repositories from
the active config. They preserve unrelated containers, volumes, proxy routes,
export networks, and the shared `takod`/`tako-proxy` runtime. Node-wide Docker
builder cache and dangling image cleanup are intentionally excluded from default
cleanup because those caches may be useful to unrelated projects on the same
host. Successful deploys and explicit `tako cleanup --docker-cache` requests run
Docker builder cache pruning with a default `20GB` keep-storage budget instead
of clearing the entire cache. The installed takod service also runs a scheduled
builder-cache prune loop every 24 hours with the same keep-storage budget;
operators can tune explicit cleanup with `--docker-cache-keep-storage <size>`
or disable the agent loop with `takod run --build-cache-prune-interval 0` in a
custom service unit.

## Implementation Status

```text
Done:
1. CLI runtime operations go through takod.
2. Mutating runtime and state operations share remote leases.
3. State pull/status and env push/pull support clone and CI workflows.
4. Desired revisions, actual snapshots, and events persist on nodes.
5. WireGuard peer material and node configs reconcile through takod.
6. Per-node proxies render mesh upstreams from desired placement and Caddy health checks.
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
17. `tako discovery exports` lists exported service records from reachable
    nodes.
18. `tako doctor` verifies takod agent versions, rootful remote Docker, and live
    proxy runtime shape.

Next:
1. Add distributed certificate handling for multi-edge deployments.
2. Add DNS-01 certificate handling for wildcard public hostnames.
3. Evaluate background peer anti-entropy after the explicit repair workflow is proven.
4. Add layer-delta peer image distribution.
5. Expand e2e validation across one-node and multi-node meshes.
```
