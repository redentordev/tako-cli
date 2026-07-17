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
path. For current importable APIs and SDK-oriented building blocks, see
[API and SDK Foundation](API-SDK.md).

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

The node-local `takod` process listens on `/run/tako/takod.sock`. Node software
changes belong only to explicit `tako setup` and `tako upgrade servers`
lifecycle commands. Deploy, scale, rollback, and other application commands do
not install or restart agents. They capability-check the running agent and fail
before application mutation when a node upgrade is required. Development
upgrades pass a Linux binary with `--takod-binary`.

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

Private registry images declare credentials in a top-level `registries:`
block keyed by registry host:

```yaml
registries:
  ghcr.io:
    username: octocat
    password: ${GHCR_TOKEN}
```

Passwords must be `${ENV_VAR}` references — config load rejects literal
values before expansion. Credentials are request-scoped by design: the CLI
sends them inside typed request bodies to `takod` (never argv or query
strings, so they cannot leak through remote `ps` or shell history), `takod`
materializes an ephemeral `DOCKER_CONFIG` directory (0700/0600) for the one
pull or build and removes it before responding, and nothing credential-shaped
is ever written to replicated state, deployment records, or job specs.
`docker.io` and its aliases normalize to the canonical
`https://index.docker.io/v1/` auth key. Authentication failures are
classified distinctly from missing images and surface as an
`image.pull.auth_failed` event plus a typed error, so operators rotate
credentials instead of retrying. One-off runs pass credentials with
`tako run --registry-user <user> --registry-password-stdin`.

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
once per architecture, selects the loaded immutable image ID, and imports that
archive into every assigned target through the authenticated structured node
transport. `auto` tries this local path first and falls back to the remote takod
builder if local Docker or buildx is not ready. Local build mode requires no
Docker SSH plugin and does not grant the client a remote Docker shell.

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
once on one assigned node per target architecture, then streams that exact
Docker image to same-architecture peers, skipping nodes where the exact image
already exists. Local build mode builds once per target architecture on the
client and streams the exact image ID to every assigned node through takod's
image import API. Each target verifies digest, platform, and current Docker
daemon identity before the deployment can reuse the result.

Top-level `builds:` are scheduled before their selected consumers. Tako groups
all selected services by build key and computes the union of their placement
nodes. Remote mode builds once on one source node per target architecture and
streams that exact Docker image to same-architecture peers; local mode builds
once per target architecture and pushes to its peers. Each consumer then
reconciles through a prepared-image path with both
per-service build and pull disabled. The shared definition integrity digest is
part of each consumer's config hash and resolved artifact tag; desired state
records the context, argument names, target, Dockerfile, and resolved image, but
never argument values. Build args are explicitly non-secret configuration:
their digests are identity, not confidentiality, and secret values must use
runtime secrets or operator files instead.
Shared images live in a repository namespace separate from service builds, and
the definition fingerprint is also appended to the image tag. Scale and
rollback transfer a retained exact image to newly assigned same-architecture
nodes and fail before container mutation when no exact source remains; they
never rebuild a historical tag from today's source.

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

### Release commands

`deploy.release` runs a command from the **new** revision's image exactly once
per applied deploy — after the image exists on the assigned nodes and before
any rollout activation (before warm/activate for `blue_green`, before the
first replica replacement for `rolling`, before stop-old for `recreate`):

```yaml
services:
  web:
    build: .
    deploy:
      strategy: blue_green
      release:
        command: ["php", "artisan", "migrate", "--force"]
        timeout: 5m        # default 5m
        volumes: false     # opt-in to the service's volume mounts
```

The command runs in a one-off container with the service's env, secrets, and
network. A non-zero exit aborts the rollout before traffic cutover; the failed
deploy is recorded in history with the release step's exit code. Tako runs the
command exactly once per applied deploy — making the command itself
re-runnable (as `migrate` is) is the application's responsibility.

For ad-hoc commands against a deployed service (debugging, framework tasks),
use `tako exec SERVICE -- CMD` — attach to a running replica, or `--oneoff`
for a fresh container from the service's current image. `tako exec -it
SERVICE -- sh` opens an interactive shell: the CLI dials the takod socket
over an SSH direct-streamlocal channel and the remote command runs under a
pseudo-terminal that follows local window resizes (see the ptystream
terminal stream contract in MACHINE-INTERFACE.md). Sessions carry a 4h
absolute and 30m idle timeout, and disconnects kill the remote process and
remove one-off containers.

### Scheduled jobs

Service `files:` are request-scoped binary bundles. The CLI hashes the fully
resolved file tree during planning; takod validates every bundle and relative
entry name, stages it in a content-addressed version below
`/var/lib/tako/files`, then atomically publishes that immutable version before
container reconcile. Existing containers keep their prior version, and failed
rollouts leave it intact. Containers receive read-only bind
mounts. Desired state retains source/target metadata but never file bytes;
secret-marked trees receive private modes. Standard-service sets are retained
on every server that belonged to the environment at deploy time, allowing
rollback after placement changes within that fleet. Rollback fails before
container reconciliation when a newly added or replaced target node lacks the
historical set; Tako never silently substitutes current local content.
This immutable-version retention applies to standard services. Deploy-time run
sets are request-lifetime data, while scheduled jobs retain the current set and
defer pruning any prior set until its in-flight run releases it.

`kind: run` is a fingerprinted run-to-completion vertex in the deploy DAG.
Tako executes it through takod's one-off container path, records its machine
outcome in deployment history, skips completed unchanged fingerprints, reruns
under `--force`, and does not advance dependents until exit 0. `imageFrom`
reuses another service's resolved image and adds that service as an implicit
dependency. The fingerprint includes a non-reversible digest of fully resolved
environment and secret values without persisting those values in state.

`kind: job` declares a service that runs on a cron schedule instead of as
long-running replicas:

```yaml
services:
  report:
    kind: job
    schedule: "*/5 * * * *"   # robfig/cron, @every/@daily also accepted
    timezone: Europe/Berlin    # optional; default UTC
    timeout: 30m               # optional; kill + record failed (default 1h)
    build: ./report            # or image:
    command: generate-report
```

Jobs require `schedule` and `command`; they accept `image`/`build`/`imageFrom`, `env`,
`envFile`/`envFiles`, `secrets`, `volumes`, `placement`, `dependsOn`, and the
container runtime controls documented in `CONFIGURATION.md`, and reject
`proxy`, `replicas`, `healthCheck`, `loadBalancer`, and `persistent`. A
deploy builds the job's image exactly like a service's, distributes it to
the **owning node** — the job's first placement target, so multi-node
meshes never double-fire — and registers the schedule with that node's
agent. The agent fires each run as a one-off `--rm` container with the
job's env/secrets and the project network, records a bounded history (last
50 runs with an output tail), skips a firing whose previous run is still in
progress (recorded as `skipped`), and kills runs that exceed `timeout`
(recorded as `timeout`). Deploys reconcile schedules declaratively: a job
removed from the config (or a full `tako remove`) is unscheduled on every
node in the same pass.

`tako jobs` lists schedules with next/last runs, `tako jobs runs [JOB]`
shows history, `tako jobs trigger JOB` fires a run immediately and streams
its output, and `tako logs JOB` prints the latest run's recorded output.
In plans and `tako ps`, a job's actual state is its registered schedule —
not container presence — so an idle job is "up-to-date", never drift.

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

Replica placement is durable desired state, not a fresh calculation on every
command. Each one-based replica slot is bound to the logical node and its
immutable node ID. The first singleton prefers the local control-plane worker;
scaling out assigns only new slots, while joining nodes or reordering the
environment server list leaves healthy assignments untouched. A normal deploy
fails closed if an existing assignment is outside the current placement or is
on a cordoned/draining node, so configuration drift cannot become an implicit
rebalance.

The resolved assignment intent is replicated before the first image,
container, schedule, or route mutation. If a process crashes, a later service
fails, or final actual-state capture cannot complete, the chosen slots remain
durable and desired/actual drift exposes the incomplete operation. A retry
therefore reuses the same nodes instead of rescheduling from membership order.
When upgrading desired state that predates assignments, Tako adopts only slots
that can be matched to deterministic live container identities in the per-node
actual snapshot. It fails closed instead of inventing a location when that
evidence is missing or ambiguous.

Service deletion uses the same write-ahead rule. Before stateless or job
cleanup, desired state retains the prior sanitized configuration, image, and
slot bindings with `removalPending: true`. Cleanup success is followed by a
normal desired-state write that removes the entry. A crash or partial cleanup
therefore leaves enough authority for an exact retry; missing prior
configuration or an unreachable assigned node fails closed before the marker
is written.
Narrow workflows carry the complete prior desired record for services outside
their scope through both intent and completion writes. Consequently scale,
promotion, rollback, direct-image deployment, and targeted deploy cannot erase
an unrelated in-progress removal. Only a full deploy owns the transition from
`removalPending` to absent, and an actual-only workload without a proven slot
binding requires explicit adoption before removal.

Movement starts with a review artifact:

```bash
tako placement plan cordon --node node-2 --file cordon.json
tako placement plan drain --node node-2 --file drain.json
tako placement plan rebalance --file rebalance.json
tako placement verify drain.json --plan-id sha256:...
tako placement apply drain.json --plan-id sha256:...
```

Plans bind current/proposed slot assignments to the exact input desired
revision and produce a stable content digest for review. Drain excludes the
target from destinations; cordon reports replicas that remain in place while
the node becomes ineligible for new slots; rebalance makes only the minimum deterministic
stateless moves needed to reduce skew. Plan generation is read-only. Apply
rechecks the digest and exact desired revision before and after acquiring a
controller fence, copies the exact image, starts destinations before cleaning
sources, and persists a resumable placement intent. On cordoned or draining
sources, the signed placement fence authorizes only a reconcile with no desired
containers. Ordinary deploy/scale paths never consume a plan or bypass node
lifecycle latches.

For persistent services, placement is part of the lifecycle contract. In a
multi-node environment, `persistent: true` requires `placement.strategy:
pinned` or `global`. `pinned` is the singleton accessory/database shape; `global`
means one independent stateful instance per selected node. Persistent services
do not support `replicas > 1`; scale stateless clients, or use external/clustered
storage when the stateful system itself needs high availability.
Any proposed movement of a persistent service or a service with volume mounts
is left in place and emitted as a blocking `requiresVolumeMigration` step. Tako
does not automatically move node-local data; backup, restore, validation, and
cutover must be designed and reviewed separately.
Removing a still-running persistent service from `tako.yaml` is rejected: keep
the declaration so its node binding remains authoritative until an explicit
persistent-workload removal workflow has completed. Stateless and job removal
also fails before desired intent changes if any assigned node is cordoned or
otherwise unable to receive cleanup.

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
is implemented. Public proxy domains may be explicit hostnames or DNS-01-backed
wildcards. Wildcards require the environment's embedded ACME DNS provider and
are issued into the node-local certificate store before route publication.

A service can serve on several hostnames: `proxy.domain` is the primary (used
for URL display and as the target of `redirectFrom` redirects) and
`proxy.domains: []` adds co-equal serving hostnames. Every serving hostname
gets its own Caddy site block over the same upstream set and its own ACME
certificate, exactly like redirect hostnames do. A hostname may appear once
per environment across all services' `domain`/`domains`/`redirectFrom` —
duplicates fail validation (and therefore `deploy`/`--plan-only`) with exit
code 2. Static serving hostnames are exact-host Caddy sites, so they always
take precedence over a `dynamicDomains` on-demand authority's catch-all: keep
a hostname out of `domains` if the dynamic authority is supposed to serve it.
Re-deploying with an unchanged domain set is a proxy no-op — the route
manifest hash is unchanged, so no certificate churn and no Caddy reload. Internal proxy hosts use
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
`environment.proxy.placement`. Working HTTPS at another edge is reported as
`dns: proxied` only when the service explicitly declares
`proxy.cdn: cloudflare|generic`. Without that declaration, suspected-CDN detail
is human diagnostics while machine state remains wrong/unready. Thus a CDN
heuristic cannot silently change an exit code. Use
`--domain-target` for custom CNAME or edge targets and `tako domains status` to
re-check domains without redeploying.

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
manifests. These customer domains remain HTTP-01-managed. Validation warns
when a service combines `dynamicDomains` with `proxy.cdn`, because a CDN that
does not forward ACME challenge requests can break first-request issuance and
renewal.

Per-service proxy access controls guard every route of a service.
`proxy.basicAuth` takes a username and a pre-computed bcrypt hash
(`passwordBcrypt`, minted with `tako proxy hash-password`) — never a plaintext
password, because a fresh bcrypt salt per deploy would churn the route-manifest
hash and defeat idempotent redeploys. `proxy.allowIps` lists client IPs/CIDRs
allowed through the proxy; all other addresses receive 403 before basic auth is
evaluated. By default the allowlist matches the TCP peer address. Set
`proxy.trustedProxies` to the explicit CIDRs of a CDN or upstream proxy to make
Caddy trust forwarded client addresses and evaluate `allowIps` against the real
client IP. Forwarded chains are parsed right-to-left with Caddy's strict mode so
client-supplied leftmost entries cannot override the trusted proxy chain.
Prefixes broader than `/8` for IPv4 or `/24` for IPv6 are rejected;
`0.0.0.0/0` and `::/0` are never accepted. Because Caddy's trust set is
server-global, every route sharing a node that declares `trustedProxies` must
declare the same nonempty CIDR set; conflicting sets fail before publication
instead of letting one project weaken another project's trust boundary. Routes
without `trustedProxies` keep evaluating their allowlist with `remote_ip`.
Both controls are validated at config time and again by `takod` before route
manifests reach the generated Caddy config. Networked domain checks warn when
an access-controlled route appears to be behind a CDN without trusted proxies.
Caddy's JSON access entries retain both `request.client_ip` and
`request.remote_ip`; `tako access --verbose` labels the distinct TCP peer.

The proxy certificate store lives at `/var/lib/tako/certs/<domain>/` on each
node. It is node-global and cross-project by design: all projects already share
one Caddy TLS boundary, so an ingress project can push `*.platform.example.com`
and customer-project routes on the same node can consume it. Selection is
deterministic per hostname: exact entry, then the most-specific covering
wildcard, then unchanged automatic HTTPS. Internal HTTP routes never consume a
certificate-store entry.

`tako certs push` sends the PEM pair in the takod request body. Before atomic
0600 publication, takod parses the chain and key, proves the keypair matches,
checks validity and hostname/wildcard coverage, and rejects expired material.
Caddyfiles reference only entries revalidated from disk. The store is mounted
read-only into stock Caddy, and proxy recreation regenerates and validates the
Caddyfile before starting the container, guarding the reboot/startup path that
`caddy adapt` alone cannot validate. On enrolled nodes, push/remove operations
carry controller fencing for the owning project/environment and persisted
certificate ownership must match before removal. Local serialization and
atomic replacement keep reloads coherent with route rendering.

Certificate files and keys are intentionally excluded from application state,
drift, and volume backups. The root-only `tako platform backup create` recovery
bundle includes their protected node-local store together with PaaS state and
uploads it outside node 1. `tako certs ls` exposes source and expiry without
reading private keys back through an application API.

Embedded DNS-01 is the second writer into this same store. Its environment
provider credentials deliberately persist in a 0600 node-local file so takod's
single daily renewal driver can operate after the CLI exits. This is a narrow
exception to request-scoped credential handling: the expanded token never
enters Caddy's environment, route manifests, replicated state, results, or
events, and is removed with the owning environment configuration. Only the
declaring project/environment may issue; other projects can consume a covering
node-global wildcard without receiving the credential or starting an order.
Replacement ownership and credentials are staged separately and become active
only after the new route manifest is valid, preserving the previous route's
renewal state on a failed deployment. The scheduler refreshes ARI synchronously
and serializes renewal with reconcile and removal operations.
Managed certificate material is retained when unreferenced, marked orphaned,
and never renewed or automatically deleted; explicit removal purges both the
exported store copy and CertMagic's managed key material.

One-node deployments use the same proxy path with only local upstreams and do
not publish mesh host ports. Multi-node upstream ports are allocated and
recorded by the target node's `takod` agent. The CLI sends a
deterministic
app/stage/service/slot preferred port, but `takod` checks existing Docker port
bindings and its allocation registry before accepting it, then returns the
actual assigned port for container publish and proxy rendering. This lets
unrelated apps with common service names such as `web` share the same server
without taking each other's mesh upstream port. Each signed allocation has a
monotonic node-local generation and is bound to the exact active controller
operation ID/token. The controller verifies worker identity, mesh address,
operation binding, and schedulability, while durable per-node/key high-water
tombstones prevent an omitted proof from being replayed. Authorization is
publish-before-commit: node 1 signs a durable proposal, every non-controller
edge must acknowledge it, and only then does the controller commit that exact
state. An unavailable edge therefore prevents a withdrawal from committing.
Omission revokes prior generations. A separately monotonic allocation-authority
generation is distributed in the signed snapshot, so route churn does not
invalidate an in-flight membership fence. Edge nodes stop and quarantine
invalid stored routes before committing a revocation locally.
This boundary is checked before mutation and across the complete stored route
set on every render. Enrollment quarantines legacy remote manifests and stops
an already-running proxy before readiness.

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

Raw TCP/UDP workloads (game servers, MQTT, SMTP, externally reachable
databases) opt out of the proxy entirely with `ports`, which publishes host
ports on the node through the same container-publish primitive the mesh
upstreams use. Because a host port binds once per node, `ports` services are
restricted to the recreate strategy and a single replica, host ports 80/443
stay reserved for the proxy in proxied environments, and multi-node
environments require pinned or global placement so the endpoint is
deterministic. These constraints are enforced at config validation, and port
changes participate in the service config hash so redeploys rebind cleanly.

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
  connect to the controller and explicit mutation targets
       |
       v
  acquire controller authority + activate target fences
```

`upgrade servers` is a separate privileged node-lifecycle transaction, not a
deployment phase. On enrolled clusters it validates the lifecycle protocol
range and pinned host identities across the authoritative cluster inventory,
upgrades a worker canary and remaining workers, then rechecks every worker and upgrades the sole
controller last. Each node retains a durable rollback binary until the new
agent and protected platform worker report the target contract. Explicitly
targeting the controller is rejected unless all other enrolled workers already
run the target release and protocol. The authoritative controller holds one
renewable cluster upgrade lease for the full plan, while each target holds a
separate short renewable transaction lease. Candidate transfer occurs before
that node lease. Contending coordinators and per-node transactions are
rejected. Node enrollment, removal, and authoritative inventory publication
take the same node guard and reject an active upgrade lease, so a lifecycle
change cannot interleave between upgrade phases. Under the acquired node lease,
Tako revalidates immutable identity,
membership generation, lifecycle, and roles. The checksum-verified candidate
then checks the same contract against protected identity and inventory files
immediately before publishing itself. It also
refuses a target release older than the running agent; protected downgrades use
the cold disaster-recovery workflow instead.

Interrupted node transactions are recovered before `takod.service` or
`tako-platform-worker.service` starts on boot. Commit and rollback publish a
durable terminal marker before cleaning evidence, so power loss can never turn
a committed upgrade into a rollback or destroy the only rollback copy. The
commit marker remains until the next transaction acknowledges it, which also
resolves a lost SSH success response without rolling back a completed upgrade.
If a CLI dies after publication, the short node lease expires and the next
coordinator restores pending rollback evidence without requiring a reboot.
Boot recovery removes only the local node lease; a controller-global lease
owned by an external coordinator is retained until release or expiry.

Enrolled clusters use the control node as a cluster-global single-writer
operation-ID and lease authority. Payloads remain strictly bound to one
project/environment, but shared Docker tags, pruning, proxy state, and
allocation generations cannot be mutated concurrently by another project. It
issues a monotonic signed fence bound to the current membership generation and
an explicit target set; only those nodes are contacted and every structured
mutation carries both the renewable fence and its private random holder
credential. The credential hash is part of the signed fence, while status and
contender responses redact bearer authority. Renewal keeps the immediately
previous signed grant valid only during target fan-out, then revocation clears
both grants.
Unrelated unavailable workers do not block local-only work, while an
unavailable target fails closed. Worker high-water marks and controller phases
are durable, expired operations reconcile into history, and partitioned
writers cannot obtain a second token. Legacy unenrolled configurations retain
their per-node lease behavior. The local `.tako` lock remains a same-machine
guard.

`tako platform backup create` requires a 256-bit out-of-band
`TAKO_RECOVERY_KEY` plus either externally verified persistent-workload object
evidence or a no-persistent-workloads attestation. It takes an exclusive
control-plane snapshot lock, proves that the local controller key owns the
authoritative membership and exact published inventory, refuses an active
controller operation, quiesces
HTTP and background state writers, derives required volumes from authoritative
desired state, downloads and hashes every fresh workload backup, and archives
identity, membership, operation journals, environment bundles, PaaS data, and
certificate state. The bounded traversal-safe tar/gzip stream is fed directly
into chunked AES-256-GCM, so no plaintext controller archive is written to
disk. The exact encrypted size and SHA-256 are authenticated locally, uploaded
only over HTTPS, downloaded with a strict size bound, and compared again before
success. `tako platform backup verify` requires the expected cluster ID and
performs offline authenticated verification. `tako platform backup restore`
decrypts, verifies, and extracts the same single input stream into a private
sibling directory, then atomically publishes the requested staging directory
only after final authentication; it never overwrites live controller paths.

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
15. Config validation requires embedded DNS-01 configuration for wildcard
    public hostnames.
16. `tako state forget-node` explicitly prunes retired nodes from replicated
    runtime state.
17. `tako discovery exports` lists exported service records from reachable
    nodes.
18. `tako doctor` verifies takod agent versions, rootful remote Docker, and live
    proxy runtime shape.
19. Every command has a machine interface: versioned result documents
    (`--output json`), ndjson event streams (`--events ndjson`), and stable
    exit codes, pinned by golden tests.
20. `tako exec` runs commands in running service containers or one-off
    containers; per-service `release:` hooks gate deploys.
21. `kind: job` services run on cron schedules through takod with run
    history, manual trigger, and declarative per-node reconciliation.
22. Services can serve multiple public domains (`proxy.domains`).
23. Private registry credentials flow request-scoped from a `registries:`
    config block or `tako run --registry-user/--registry-password-stdin`.
24. SIGKILL crash recovery is proven by a repeatable harness
    (`scripts/kill-mid-deploy-e2e.sh`): in-progress history, lease
    force-release, clean redeploy.
25. Interactive exec: `tako exec -it` runs a real PTY session over an SSH
    direct-streamlocal channel and the documented ptystream frame protocol
    (resize, exit codes, idle/absolute timeouts, disconnect cleanup). This
    resolves the interactive-exec deferral from the exec design (ADR 7).
26. Raw TCP/UDP host port publishing (`ports`) for non-HTTP services, with
    recreate/single-replica/placement guardrails validated at config time.

Next:
1. Add distributed certificate handling for multi-edge deployments.
2. Evaluate background peer anti-entropy after the explicit repair workflow is proven.
3. Add layer-delta peer image distribution.
4. Expand e2e validation across one-node and multi-node meshes.
```
