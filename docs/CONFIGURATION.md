# Configuration Guide

Recipes for common `tako.yaml` shapes. For the runtime, state, mesh, and CI
model behind them, see [ORCHESTRATION-MODEL.md](./ORCHESTRATION-MODEL.md).
Run `tako config explain -e <env>` to see the effective defaults Tako infers
for any config without copying them into your YAML.

## Snippet conventions

Services always live under `environments.<env>.services`; there is no
top-level `services:` key. Short snippets on this page show only the keys they
discuss, using real paths from the config root. Merge them into a config that
already has `project`, `servers`, and an `environments.production` entry that
targets your servers, like the one in
[Existing VPS Deployment](#existing-vps-deployment).

## Simple Web Application

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      web:
        build: .
        port: 3000
        env:
          NODE_ENV: production
        proxy:
          domain: app.example.com
          email: admin@example.com
```

## Existing VPS Deployment

<!-- tako:snippet complete -->
```yaml
project:
  name: my-app
  version: 1.0.0


servers:
  production:
    host: ${TAKO_PRODUCTION_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}

environments:
  production:
    servers:
      - production
    services:
      web:
        build: .
        port: 3000
        proxy:
          domain: app.example.com
          email: admin@example.com
```

```bash
tako setup && tako deploy
```

### SSH port

Set `port` on a server when sshd does not listen on 22 (the default):

<!-- tako:snippet partial -->
```yaml
servers:
  production:
    host: ${TAKO_PRODUCTION_HOST}
    port: 2222
    user: root
    sshKey: ${TAKO_SSH_KEY}
```

When `tako setup` configures UFW it allows the SSH port set before enabling
the firewall: `22/tcp`, the configured `port`, and the server-side port of the
SSH session it is running over (these differ behind provider port forwarding).
It then opens a fresh SSH connection to `host:port`; if that fails, it disables
UFW again and fails the firewall step instead of locking you out. The fail2ban
SSH jail bans on the same ports.

Setup never closes an SSH port on its own. After you move SSH to a new `port`
and re-run `tako setup`, the old port stays open and setup prints a warning
(and reports `firewall.staleSshPorts` in `--output json`). Once you have
confirmed the new port works, remove the old rule manually:

```bash
sudo ufw delete allow 2200/tcp
```

On hosts without UFW outside the Debian family (for example RHEL, Rocky, Alma,
SUSE, Alpine), setup does not manage the firewall. It warns and lists the ports
to open in your provider or native firewall, including the SSH port.

## First PaaS Node With Local Deployments

Install Tako on the server that will host the control plane and most initial
workloads, then initialize the protected single-node foundation:

```bash
sudo tako platform inspect
sudo tako platform init --node node-1
```

`platform inspect` is read-only and should be run before initialization when a
server's history is uncertain. It reports the immutable cluster and node IDs,
last published roles, lifecycle, and controller from the root-published local
binding and trusted inventory, including the snapshot generation and update
time. It does not claim that the published controller is currently reachable.
When run with `sudo`, it also validates and cross-checks the protected identity
and, on the controller, compares protected membership with the published
generation. On an enrolled worker it explains that running Tako or another
project locally does not promote that worker. `platform init` refuses an
existing worker, conflicting identity, or orphaned platform artifacts before
creating accounts, staging binaries, or changing services, and prints the
existing enrollment plus safe next steps. An identity-only interrupted
first-controller bootstrap requires recovery because custom account and policy
choices may predate the identity without having been persisted. A runtime-only
interruption receives an exact `platform init` resume command only when the
protected configuration is present and membership exactly matches the
published inventory. Tako never automatically detaches a node or replaces its
create-once identity; use another server or VM for an independent cluster.

Running the CLI from an enrolled worker is only an operator location; it never
makes that worker a project-specific controller. Before the first deployment
from a workspace on any enrolled node, Tako shows the detected cluster, local
node, published controller, and project name, then requires an explicit
attachment. The clearest workflow is:

```bash
tako project attach --cluster 11111111-1111-4111-8111-111111111111
tako deploy
```

The acknowledgement creates the workspace-local, create-once
`.tako/platform-cluster.json` binding. All project-mutating commands require
the binding; read-only inspection remains available without it. Later config
loads refuse if the project name or detected cluster changes. This also makes
the namespace consequence clear:
an existing identical `project.name` is the same project inside that cluster,
not an isolated second PaaS.

Interactive deploys can approve an attachment prompt. Non-interactive deploys
can supply the exact immutable ID from the structured
`ClusterAttachmentRequired` result:

```bash
tako deploy --accept-cluster 11111111-1111-4111-8111-111111111111
```

`--yes` does not imply cluster attachment. The exact-ID acknowledgement is
separate because a deployment confirmation and choosing which PaaS owns a
workspace are different decisions. Deploy only publishes the binding after a
valid plan and any destructive confirmation, immediately before apply;
`--plan-only`, plan mismatch, failed planning, and cancelled confirmation never
attach the workspace. The explicit `project attach` command is itself the
requested workspace mutation and can be used before any other mutating command.

The binding stores the inventory generation and timestamp for audit context,
but only `clusterId` and `project` are stable matching authorities; moving the
same workspace between nodes of that cluster does not require rebinding. An
attached workspace also remains usable from an off-node operator/CI host when
its explicit SSH server entries all carry the same matching immutable
`clusterId`/`nodeId`. `tako project attach --cluster CLUSTER_ID` works from
that off-node host too; its binding records only the cluster and project because
there is no local enrolled node or locally published controller to claim. Mixed,
partially identified, or unidentifiable clusters fail closed. Any local platform
artifact without a complete trusted inventory—including a residual protected
WireGuard identity—also fails closed and directs the operator to
`sudo tako platform inspect` before mutation.

Automation can consume the same diagnosis without parsing prose:

```bash
sudo tako platform inspect --output json
```

The `PlatformEnrollmentInspection` result has status `not-enrolled`,
`enrolled`, or `incomplete`, plus a stable `nextAction`. The first two statuses
exit 0. Incomplete or residual state emits the result and exits 6 so automation
cannot mistake it for permission to initialize. Malformed or contradictory
trusted files fail the command.

The command prints the immutable `clusterId`, `nodeId`, and numeric
`workerUid`. On an enrolled platform node, Tako materializes server targets,
mesh addresses, lifecycle, roles, and pinned SSH identities from a root-owned,
read-only cluster inventory plus a public local-node binding; it never needs to
read the root-only installation identity or platform configuration. Application
configuration cannot override them. The production inventory and local binding
must remain root-owned, singly linked, regular `0644` files inside a root-owned
non-group/other-writable `/etc/tako`; directory or file ownership drift and
group/other writability are rejected before materialization. An
environment may still select a member by canonical node name or an old server
alias carrying the immutable `nodeId`. The explicit form below remains the
legacy/off-node compatibility form:

<!-- tako:snippet partial -->
```yaml
servers:
  node-1:
    host: ${TAKO_NODE_1_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}
    transport: auto
    clusterId: 11111111-1111-4111-8111-111111111111
    nodeId: 22222222-2222-4222-8222-222222222222
    workerUid: 997
```

`transport: auto` makes one safe decision before connecting: when Tako is
running on the exact enrolled node, it verifies the local Unix-socket peer and
immutable installation identity and uses the protected platform-worker
ingress. Everywhere else it uses SSH and verifies the remote node identity.
An SSH failure never causes a retry against the local machine. Omit
`transport` to preserve legacy SSH behavior; use `transport: local` only when
the command must fail instead of falling back to SSH.

Routine deploy, status, monitor, logs, exec, scale, rollback, state,
environment-bundle, backup, and cleanup traffic uses the structured node-agent
surface. Privileged setup, join, repair, and server upgrades remain explicit
SSH operations. Local image builds and reuse are accepted only after exact
image digest, Docker platform, and freshly attested daemon identity checks; a
matching mutable tag alone is not trusted.

All active workers remain connectivity targets so read-only status and logs,
plus controller-owned lifecycle recovery, can still reach ready, cordoned, or
draining members. Application cleanup and other all-node mutations fail before
fanout when the selected set contains an unschedulable member; reschedule it or
use the explicit controller lifecycle workflow first. Only schedulable workers
may receive new assignments. Proxy reconciliation is limited to nodes
with the `edge` role, and node-side image builds are limited to schedulable
`builder` nodes; a worker-only join cannot acquire either responsibility from
application placement. A controller builder may build once and transfer the
exact image digest to worker targets.

Platform initialization also creates protected controller membership and a
root-owned inventory snapshot. Membership is generationed control-plane state;
application route manifests cannot add nodes or change roles.

To add a worker, first run normal `tako setup` with the current binary and
verify its SSH key is present in `~/.tako/known_hosts` or
`~/.ssh/known_hosts`. Choose a new UUID, create a short-lived token on node 1,
then enroll from node 1:

```bash
sudo tako platform join-token create --node-id 33333333-3333-4333-8333-333333333333 --ttl 15m
sudo env TAKO_JOIN_TOKEN='tako_join_v1....' tako platform node enroll \
  --node-id 33333333-3333-4333-8333-333333333333 \
  --node worker-2 --mesh-ip 10.210.0.2 \
  --host 203.0.113.20 --controller-mesh-host node-1.example.com \
  --controller-host 203.0.113.10 \
  --user root --ssh-key /root/.ssh/tako
```

Enrollment pins the exact in-memory SSH host key, creates the worker-only
immutable identity and WireGuard key remotely, binds the actual mesh public
key in controller membership, consumes the cluster/node-bound token once (the
plaintext token is never placed in the Tako process arguments),
publishes the trusted inventory, reconciles the membership-owned mesh on every
member, restarts and attests takod, and leaves the
node `ready` but unschedulable. Admit it only after review:

```bash
sudo tako platform node schedulable \
  --node-id 33333333-3333-4333-8333-333333333333 \
  --ssh-key /root/.ssh/tako
```

The lifecycle is `joining -> ready -> schedulable -> cordoned -> draining ->
removed`. Lifecycle updates are published to every active worker over the
immutable SSH key captured during enrollment; later `known_hosts` edits cannot
silently repin it. If password authentication is unavoidable, place the secret
in `TAKO_SSH_PASSWORD` (or name another variable with `--password-env`) so it
never appears in process arguments. For controller-initiated routine access to
remote members, set the platform-wide private-key path in
`TAKO_PLATFORM_SSH_KEY`; an application server entry cannot replace it.

`--controller-mesh-host` is the host or IP that workers use as node 1's
WireGuard endpoint. Worker endpoints default to their enrolled SSH host.
`--controller-host` is node 1's worker-reachable SSH provisioning address;
its exact key must already be verified in Tako or OpenSSH `known_hosts`.
Enrollment publishes that pin with the controller membership record so a CLI
on a joined worker never falls back to an empty or unpinned controller
transport.
Enrolled takod instances reject application-owned `/v1/mesh/apply` requests;
an application deployment therefore cannot restore a tombstoned peer.

Removal is a durable, retryable revoke-first operation. Tako rewrites and
attests the persistent WireGuard configuration on the controller and every
remaining peer without the target's bound public key before it commits the
membership tombstone. Target-local shutdown and private-key deletion happen
afterward and can be resumed by rerunning the same remove command.

Cordon, drain, and removal set and attest a durable deployment-deny latch on
the target before committing controller membership, so a crash cannot leave a
stale target accepting direct workload mutations. The target then receives the
changed inventory before unrelated workers. `schedulable` also recovers a
cordoned node and clears the latch only after its schedulable inventory is
attested. Tako refuses to drain or remove the final controller.

Remote mesh proxy routes require an operation-bound worker allocation and a
controller-committed generation. Worker signatures alone are insufficient:
the controller keeps replay high-water tombstones, publishes a signed proposal
to every edge before commit, and durably records each publication target before
contacting it. If the client dies, the next cluster-global operation commits a
strictly newer copy of the last controller authority and converges every
recorded target before publishing ordinary inventory. Omission remains
revocation, and edge nodes stop and quarantine routes that no longer match.
One-node local runtime-alias routes remain enabled without host-port allocation.

Platform recovery requires `TAKO_RECOVERY_KEY` to be standard base64 for 32
random bytes. Keep it outside node 1 and outside the recovery bucket. Recovery
storage endpoints must use HTTPS. Creation verifies persistent-volume coverage
from controller desired state, reads back and hashes every referenced workload
object, refuses an active controller operation, streams the archive directly
through encryption without a plaintext temporary, then uploads and reads back
the exact size- and digest-matched authenticated controller bundle. Offline
verify and restore require `--cluster-id`.

## Full-Stack Application

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      web:
        build: ./frontend
        port: 3000
        proxy:
          domain: app.example.com
          email: admin@example.com

      api:
        build: ./backend
        port: 4000
        replicas: 2
        dependsOn: [database]
        env:
          DATABASE_URL: postgresql://db:5432/myapp

      database:
        image: postgres:15
        persistent: true
        volumes:
          - db_data:/var/lib/postgresql/data
        placement:
          strategy: pinned
          servers: [production]
        env:
          POSTGRES_PASSWORD: ${DB_PASSWORD}
```

## Multi-Server Deployment

Built-in ACME TLS needs a single proxy node, so a multi-server environment
with public services pins its proxy placement to one node.

<!-- tako:snippet complete -->
```yaml
project:
  name: my-app
  version: 1.0.0

servers:
  node1:
    host: ${NODE1_HOST}
    user: root
    sshKey: ~/.ssh/id_ed25519
    labels:
      role: web
  node2:
    host: ${NODE2_HOST}
    user: root
    sshKey: ~/.ssh/id_ed25519
    labels:
      role: web

environments:
  production:
    servers:
      - node1
      - node2
    proxy:
      placement:
        strategy: pinned
        servers: [node1]
    services:
      web:
        build: .
        replicas: 3
        port: 3000
        placement:
          strategy: spread
          constraints:
            - node.labels.role==web
        proxy:
          domain: app.example.com
          email: admin@example.com
```

## Environment Inheritance And Shared Fragments

Environments that differ only in a domain, a replica count, or a server list
do not need to repeat every service. Use top-level `x-` keys for shared
fragments and `extends` for environment overlays.

### Shared fragments with `x-` keys

Top-level keys that start with `x-` are accepted and ignored in `tako.yaml`
and `tako.json`. Their content is not validated, so they are a scratch area
for YAML anchors that the rest of the file reuses with `*name` or `<<: *name`.
`x-` keys are only allowed at the top level; inside `environments`, services,
or servers they are still unknown-field errors.

<!-- tako:snippet complete -->
```yaml
x-web: &web
  image: ghcr.io/example/app:1.4.2
  port: 3000
  healthCheck:
    path: /health

project:
  name: my-app
  version: 1.0.0

servers:
  production:
    host: ${TAKO_PRODUCTION_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}
  staging:
    host: ${TAKO_STAGING_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}

environments:
  production:
    servers: [production]
    services:
      web:
        <<: *web
        replicas: 2
        proxy:
          domain: app.example.com
  staging:
    servers: [staging]
    services:
      web:
        <<: *web
        proxy:
          domain: staging.example.com
```

### Extending an environment

An environment can declare `extends: <name>`. Tako resolves it by
deep-merging the environment's own keys over the fully resolved parent before
validation, so every command sees one flattened environment. An environment
marked `abstract: true` is a template only: it can be extended, but it is not
validated on its own, never deployed, never picked as the default
environment, and `-e base` fails with a message that lists the environments
that extend it.

<!-- tako:snippet complete -->
```yaml
project:
  name: my-app
  version: 1.0.0

servers:
  production:
    host: ${TAKO_PRODUCTION_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}
  staging:
    host: ${TAKO_STAGING_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}

environments:
  base:
    abstract: true
    services:
      web:
        image: ghcr.io/example/app:1.4.2
        port: 3000
        env:
          LOG_LEVEL: info
          REGION: eu
        proxy:
          domain: app.example.com
      worker:
        image: ghcr.io/example/app:1.4.2
        command: ["bin/worker"]

  production:
    extends: base
    servers: [production]
    services:
      web:
        replicas: 2

  staging:
    extends: production
    servers: [staging]
    services:
      web:
        replicas: 1
        env:
          LOG_LEVEL: debug
        proxy:
          domain: staging.example.com
      worker: null
```

Here `staging` runs `web` with one replica, `LOG_LEVEL=debug`, `REGION=eu`,
and the staging domain, and has no `worker`. `production` is unchanged.

Merge rules, applied at every path of the child environment:

| Parent value | Child value | Result |
|---|---|---|
| any | not set | parent value |
| mapping | mapping | merged key by key (services merge by name); parent keys keep their order, new child keys follow |
| any | scalar or list | child value; lists are replaced, never appended |
| any | value of a different kind | child value (for example `build: .` replaced by `build: {context: ., target: prod}`) |
| any | `null` | key removed |

- `null` removes an inherited key only in an environment that declares
  `extends`. `services: {worker: null}` drops an inherited service, and
  `servers: null` plus a `serverSelector` switches a child from a server list
  to label selection. In an environment without `extends`, `null` keeps its
  usual meaning (unset).
- Lists replace. That includes `servers`, `volumes`, `ports`, `command`, and
  environment `secretSources`. Top-level `secretSources` still apply first to
  every environment, followed by the resolved environment's own list:

<!-- tako:snippet complete -->
```yaml
project:
  name: my-app
  version: 1.0.0

secretSources:
  - provider: doppler
    project: my-app
    providerEnv: prd

servers:
  production:
    host: ${TAKO_PRODUCTION_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}
  staging:
    host: ${TAKO_STAGING_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}

environments:
  production:
    servers: [production]
    secretSources:
      - provider: aws-ssm
        path: /my-app/production
        region: us-east-1
    services:
      web:
        image: ghcr.io/example/app:1.4.2
        secrets: [DATABASE_URL]

  staging:
    extends: production
    servers: [staging]
    # Replaces the inherited aws-ssm source; doppler still applies first.
    secretSources:
      - provider: aws-ssm
        path: /my-app/staging
        region: us-east-1
```

- A structured `build` is a mapping, so it merges key by key: a child can
  set only `build: {target: prod}` (or new `args`) and inherit `context` from
  its parent. The resolved environment must still have a `context`; if no
  environment in the chain supplies one, validation fails with the chain in
  the error.
- Chains are allowed (`staging -> production -> base`) up to 8 `extends`
  hops. A cycle (including `extends` naming itself), an unknown parent, or a
  chain deeper than 8 hops fails validation with the full chain in the error.
- `extends`, `abstract` and `preview` are not inherited. Abstract
  environments do not need `servers` or `serverSelector`. An abstract
  environment that nothing extends produces a `tako validate` warning, unless
  it declares `preview:` (see [Preview Environments](#preview-environments)).
- An unknown or mistyped field is reported against the environment that
  declared it, and validation errors in an inheriting environment end with
  the chain, for example `(environment staging extends production)`.
- `tako config explain -e staging` prints `Environment: staging (extends
  production -> base)` and labels each environment-scoped value with the
  environment that supplied it, for example `replicas: 2 (config, from
  production)`.
- Deployed state, history, and `tako config export` contain only the
  flattened environment; `extends` and `abstract` never reach a node.

### Default environment

Without `-e`, Tako uses the only deployable (non-abstract) environment, else
`production`, else the first environment name in alphabetical order. In that
last case it prints `no -e and no 'production'; using '<name>'; pass -e` to
stderr, so pass `-e` in scripts and CI.

### Preview environments

A `preview:` block makes an environment the base of short-lived preview
environments. `tako deploy --preview pr-42` synthesizes `preview-pr-42`, which
extends the base, and deploys it. The usual base is an abstract template:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      web:
        image: ghcr.io/example/app:1.4.2
        port: 3000
        replicas: 2
        proxy:
          domain: app.example.com
  preview:
    abstract: true
    extends: production
    preview:
      domain: "{name}.preview.example.com"
      ttl: 72h
      max: 5
      maxReplicas: 1
      keepJobs: false
      env:
        PUBLIC_URL: "{url:web}"
      services:
        web:
          env:
            PREVIEW_HOST: "{host:web}"
```

| Key | Default | Meaning |
|---|---|---|
| `domain` | `{name}.{domain}` | Host template for proxied services. Placeholders: `{name}` (preview name), `{service}` (service name), `{domain}` (the service's own domain, or internal `host`). |
| `ttl` | `168h` | Go duration or `none`; the preview expires this long after its last successful deploy. |
| `max` | `5` | Previews this base may have at once. |
| `maxReplicas` | `1` | Replica cap for every service except global placement. |
| `keepJobs` | `false` | Keep `kind: job` services, which are dropped by default. |
| `env` | | Merged into every service. Placeholders: `{name}`, `{host:<service>}`, `{url:<service>}`. |
| `services.<name>.env` | | Merged into one service, with the same placeholders. |

`preview` is not inherited: an environment that extends a base is not itself
a base. Preview services must not publish host ports, use `imports`,
bind-mount host paths, or mount `name`/`external`/driver volumes; `tako
validate` warns about these and a preview deploy rejects them. A preview reads
the secrets files of its base (`.tako/secrets.preview` above). See
[PREVIEW-ENVIRONMENTS.md](./PREVIEW-ENVIRONMENTS.md) for DNS, certificates,
TTL and pruning.

### Environment variables and `-e`

`${VAR}` references are expanded from the process environment and the `.env`
file next to the config. Unset variables are an error, but which ones count
depends on whether the command was given `-e`:

- `tako validate`, `tako deploy`, and `tako config explain` with `-e <env>`
  require only the variables used by the parts of the config that environment
  can reach:
  - every top-level section (`project`, `registries`, `secretSources`,
    `notifications`, `volumes`, `builds`, `hooks`, `runtime`, `state`, `mesh`,
    ...)
  - the selected environment and every environment it `extends`, including
    concrete parents, all of whose values are required
  - the servers it can use: the servers listed in its (or an inherited)
    `servers`, `proxy.placement.servers`, and service `placement.servers`
  - any `x-` fragment or anchor it uses through a YAML alias, even one
    declared inside another environment
- When the selected environment uses `serverSelector`, has no `servers` list,
  or any server is an enrolled platform node (`clusterId`/`nodeId`), every
  server is in scope and every server's variables are required.
- A variable used only by other environments, unused `x-` fragments, or
  servers outside that scope may be unset. The command then leaves every
  other environment, and each out-of-scope server it could not expand, out of
  the loaded config, so their unexpanded values are never used and those
  servers are never contacted. `tako validate -e <env>`
  names the skipped variables in a warning; `tako deploy -v` prints them.
- When every variable is set, nothing is left out: `-e` only changes the
  outcome for a config that would otherwise fail to expand.
- Without `-e` (including the default-environment pick), and for every other
  command (for example `tako setup`, `tako upgrade servers`, and `tako doctor`),
  every variable in the file is required. Run `tako validate` without `-e` to
  check all environments at once.
- With `--preview` (`tako deploy`, `tako config explain`, `ps`, `logs`,
  `exec`, `history`, `rollback`), `-e` names the preview's base and the
  scope is that base and its `extends` chain: the base is never left out and
  its variables are required, even when it is abstract. Without `-e` every
  variable is required.

A CI job for one environment therefore only needs that environment's
variables:

```bash
TAKO_PREVIEW_HOST=203.0.113.10 tako validate -e preview
```

## Background Workers

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      worker:
        build: ./worker
        replicas: 3
        env:
          REDIS_URL: redis://redis:6379
        # No port = background service

      redis:
        image: redis:7-alpine
        persistent: true
        volumes:
          - redis_data:/data
        placement:
          strategy: pinned
          servers: [production]
```

## Container Command, Entrypoint, Health, And Labels

`command` accepts either the legacy string form (executed through `sh -c`) or
an argv list passed directly to the image without a shell. Use list form for
distroless images and exact Compose-style exec semantics. `entrypoint` accepts
a single executable or a list; additional list entries are placed before the
service command arguments.

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      consumer:
        image: getsentry/sentry:26.6.0
        entrypoint: [/etc/sentry/entrypoint.sh, run]
        command: [sentry, run, consumer, errors]
        labels:
          com.example.role: errors-consumer
        healthCheck:
          command: test -f /tmp/healthy
          interval: 30s
          timeout: 5s
          retries: 3
          startPeriod: 10m
```

`healthCheck.command` becomes Docker's container health command and is mutually
exclusive with `healthCheck.path` and `healthCheck.tcpPort`. Custom label keys
starting with `tako.` are rejected because Tako reserves that namespace for
runtime identity, revision, and drift metadata. Command/entrypoint lists accept
at most 256 arguments (64 KiB total), health commands at most 4096 bytes, and
services at most 256 custom labels (64 KiB total).

List-form commands and all entrypoints require a matching takod. Release deploys
refresh the agent during runtime setup; when using a local/dev CLI build,
upgrade the agent with `tako upgrade servers --takod-binary <linux-binary>`
before relying on the new forms. The CLI checks the agent capability before
container or schedule reconciliation, so a stale agent rejects these configs
explicitly rather than silently changing their semantics.

## Stateful Services And Volumes

Containers are disposable; Docker volumes are the data boundary. Databases,
queues, CMS storage, and tools such as n8n should use prebuilt images with
`persistent: true` and at least one named or external volume:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      postgres:
        image: postgres:16-alpine
        port: 5432
        persistent: true
        volumes:
          - postgres_data:/var/lib/postgresql/data
        placement:
          strategy: pinned
          servers: [production]
        env:
          POSTGRES_USER: app
          POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
          POSTGRES_DB: app

volumes:
  postgres_data:
    driver: local
    owner: "999:999"
```

`volumes.<name>.owner` is an optional numeric `uid` or `uid:gid` policy for a
fresh, explicitly declared Tako-managed local volume. Takod records a durable
creation token before `docker volume create`, verifies that token and the local
driver, then changes only the new volume root ownership. Existing volumes,
external volumes, binds, custom drivers, and local volumes with `driver_opts`
are never modified. The policy is immutable for a Docker volume identity:
changing `owner` later is rejected. Use an explicit `kind: run` migration and a
new volume identity when ownership must change. Automatic named-volume deletion
is disabled because Docker has no atomic compare-and-delete operation; cleanup
reports a warning instead. Remove a volume explicitly only after verifying its
identity and backups.

Tako validates that persistent services declare a volume before deploy. Normal
commit deploys do not recreate unchanged image-only services, so a source
change in `web` does not bounce `postgres`. In multi-node environments, Tako
also requires explicit `placement.strategy: pinned` or `global` for persistent
services so node-local data has a known home. Broad `tako deploy --force` skips
persistent services; targeted `tako deploy --service postgres --force` is the
explicit opt-in when you need to recreate one container while keeping the same
volume.

Persistent services are singleton by design: `replicas` must stay at 1. If you
need more than one copy of a stateful system, use an external managed service
or the datastore's own clustering/replication model, then scale stateless app
containers that connect to it. `placement.strategy: global` is the exception
for one independent node-local instance per selected node; it is not one
replicated database.

Tako treats Docker volumes as node-local by default. For data that must be
shared across nodes, use an external storage service or an application-level
replication system such as a managed database, object storage, or
purpose-built clustered datastore. Tako does not provision shared filesystem
storage.

### Sticky assignments and movement review

Successful deploy, scale, promote, rollback, and direct-image operations save
each replica slot's logical node plus immutable node ID in desired state. A new
node does not trigger rebalance, and scale-out preserves every healthy existing
slot. If a saved node ID disappears or changes, Tako stops before mutation.
Tako replicates the resolved desired assignments before workload mutation; an
interrupted or partially failed operation leaves visible desired/actual drift
and retries reuse the same slots.
Before removing a stateless service or job, Tako persists its prior safe
configuration, image, and assignment with `removalPending: true`. The marker
and binding survive an interrupted cleanup; the service disappears from
desired state only after cleanup succeeds. A removal whose prior configuration
cannot be recovered fails closed instead of discarding placement authority.
Targeted deploy, scale, promote, rollback, and direct-image workflows preserve
every unrelated prior desired-service record, including a removal marker, in
both their pre-mutation intent and final state write. Only a full deploy that
owns cleanup may clear the marker. An actual-only workload with no proven
assignment must be explicitly adopted before it can be removed.
On the first upgrade from assignment-less desired state, deterministic
container identities and per-node actual snapshots are used to adopt the
existing slot locations. Missing or ambiguous evidence fails closed. Placement
planning reads only from a schedulable state source and prefers the enrolled
control-plane node, so a cordoned caller's stale local copy is never treated as
authoritative.

Use `tako placement plan cordon --node NAME --file plan.json` to review the
replicas that will remain in place, `tako placement plan drain --node NAME
--file plan.json` to review work needed before draining a node, or `tako
placement plan rebalance --file plan.json` to review a conservative stateless
rebalance. Record the printed
digest and verify the reviewed file with `tako placement verify plan.json
--plan-id sha256:...`. The plan is tied to the exact desired revision and is
read-only; it does not weaken the durable deployment-deny latch used by cordon
and drain. Persistent or volume-backed moves are blockers with no automatic
destination and require a separate backup/restore/cutover plan.
Do not delete a running `persistent: true` service from the configuration:
Tako rejects that deploy so its placement is not forgotten. Keep it declared
until a separately explicit persistent-workload removal has completed.

## Secrets Management

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      api:
        build: .
        port: 3000
        env:
          NODE_ENV: production
        secrets:
          - DATABASE_URL           # Secret from .tako/secrets
          - JWT_SECRET
          - API_KEY:STRIPE_KEY    # Alias: container sees API_KEY, reads STRIPE_KEY
```

```bash
# Initialize secrets storage
tako secrets init

# Set secrets per environment
tako secrets set DATABASE_URL=postgresql://... --env production
tako secrets set JWT_SECRET=super-secret-token --env production

# List secrets (redacted)
tako secrets list --env production

# Deploy with secrets
tako deploy --env production
```

A value passed as `KEY=value` is visible to other local users (`ps`,
`/proc/<pid>/cmdline`) while the command runs and can end up in shell history.
In scripts and CI, keep values off the command line:

```bash
# Read one value from stdin (one trailing newline is stripped)
printf '%s' "$DB_PASSWORD" | tako secrets set DB_PASSWORD --stdin --env production
op read op://Prod/api/token | tako secrets set API_TOKEN --stdin --env production

# Read one value from a file (one trailing newline is stripped)
tako secrets set TLS_KEY --from-file ./certs/server.key --env production

# Import many KEY=value lines from a dotenv stream (dry run until --write)
vault-export-script | tako secrets import --format env --env production --write
tako secrets import --format env --file prod.env --env production --write
```

`--stdin` and `--from-file` are mutually exclusive and take a bare `KEY` (no
`=value`); values are capped at 1 MiB. `KEY=value` stores the value verbatim,
including a leading `@`. `import --format env` keeps dotenv keys as
written (rename with `--map SOURCE=DEST`), skips existing keys unless
`--overwrite` is set, and never prints values.

### Resolving secrets from external stores at deploy time

Like Kamal's `.kamal/secrets` + `kamal secrets fetch`, `secretSources` keep the
secret store as the source of truth: Tako reads the values live every time it
resolves secrets (`deploy`, `exec`, `jobs`, `logs`, `rollback`, `promote`,
`scale`, `secrets validate`, `doctor`), and nothing is written to disk. Sources hold only provider selectors, so they are
safe to commit.

<!-- tako:snippet partial -->
```yaml
# Applies to every environment
secretSources:
  - provider: doppler
    project: api
    providerEnv: prd

environments:
  production:
    servers: [production]
    # Environment sources apply after the top-level ones
    secretSources:
      - provider: aws-ssm
        path: /myapp/prod          # /myapp/prod/db/url -> DB_URL
        prefixStrip: /myapp/prod
        region: us-east-1
        map:
          /myapp/prod/db/url: DATABASE_URL
      - provider: 1password
        vault: Production
        names: [Stripe]            # Stripe/api-key -> STRIPE_API_KEY
    services:
      api:
        build: .
        secrets: [DATABASE_URL, STRIPE_API_KEY, SENTRY_DSN]
```

Precedence, lowest to highest: `.tako/secrets`, `.tako/secrets.<env>`,
top-level `secretSources`, then environment `secretSources` (later sources
override earlier ones). Each source is fetched once per command and its values
are registered with the output redactor, so `tako logs` and health-check
failures in `rollback`/`promote`/`scale` mask them (aliased refs such as
`API_KEY:STRIPE_KEY` mask the value of `STRIPE_KEY`). If a source cannot be
fetched, `logs`, `rollback`, `promote`, and `scale` print one warning that the values will not
be redacted and continue. Source fields mirror the
`tako secrets fetch` flags below (`names`, `path`, `from`, `profile`,
`region`, `vault`, `project`, `providerEnv`, `prefixStrip`, `map`). Provider
credentials come from each CLI's own login or environment variables (for
example `AWS_PROFILE`, `VAULT_TOKEN`, `OP_SERVICE_ACCOUNT_TOKEN`,
`DOPPLER_TOKEN`, `BWS_ACCESS_TOKEN`), so CI only needs those set.

### Importing from external secret stores

`tako secrets fetch` (redacted preview) and `tako secrets import` copy secrets
from an external store into the encrypted `.tako/secrets` files. Each adapter
shells out to the provider's official CLI, so authentication, SSO, and
credential chains work exactly as they do for that CLI. `import` is a redacted
dry run until `--write` is passed, and existing keys are kept unless
`--overwrite` is set.

| Provider | CLI | Selectors |
| -------- | --- | --------- |
| `aws-ssm` | `aws` | names (`--from` prefix) or `--path` recursive; `--profile`, `--region` |
| `aws-secrets-manager` | `aws` | names or `--path` name prefix; JSON secrets flatten to `NAME/field` |
| `vault` / `openbao` | `vault` / `bao` | KV v1/v2 paths or `--path` recursive; fields flatten to `PATH/field` |
| `1password` | `op` | item names (`--vault`), `op://` references, or every item in `--vault` |
| `doppler` | `doppler` | `--project`, `--provider-env` (config); optional names |
| `infisical` | `infisical` | `--project` ID, `--provider-env` slug, `--path` folder; optional names |
| `gcp-secret-manager` | `gcloud` | names or `--path` name prefix; `--project` |
| `azure-key-vault` | `az` | `--vault` with names or `--path` name prefix |
| `bitwarden` | `bws` | optional `--project` ID; optional names (`BWS_ACCESS_TOKEN`) |

Provider names are turned into env keys by uppercasing and replacing
separators with `_`; strip a shared prefix with `--prefix-strip` or rename
individual keys with `--map SOURCE=DEST`.

```bash
# See which provider CLIs are installed
tako secrets providers

# Preview, then write, an SSM path into production secrets
tako secrets import aws-ssm --path /myapp/prod --prefix-strip /myapp/prod --env production
tako secrets import aws-ssm --path /myapp/prod --prefix-strip /myapp/prod --env production --write

# Doppler config and Vault KV
tako secrets import doppler --project api --provider-env prd --env production --write
tako secrets import vault --path secret/myapp/prod --prefix-strip secret/myapp/prod --env production --write
```

## Domain Redirects (www → non-www)

Automatically redirect traffic from one domain to another with proper SSL and
path preservation:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      web:
        build: .
        port: 3000
        proxy:
          # Primary domain where traffic is served
          domain: example.com
          # These domains 308-redirect to the primary domain
          redirectFrom:
            - www.example.com
            - old.example.com
          email: admin@example.com
```

- Automatic SSL certificates for all domains (primary + redirect domains)
- 308 permanent redirects: permanent for SEO, and unlike 301 they preserve the
  request method and body
- Path preservation (`www.example.com/api/users` → `example.com/api/users`)
- Works with both HTTP and HTTPS traffic
- Wildcard domains such as `*.example.com` use the environment's embedded
  DNS-01 provider configuration described below.

Serve one service on several hostnames with `proxy.domains`, each with
automatic HTTPS.

### Domain Readiness Checks

#### Do you need DNS-01?

| DNS and hostname model | Recommended certificate path | DNS API token? |
| --- | --- | --- |
| Grey-cloud `*.domain` A record with explicit hostnames | Caddy HTTP-01 (default) | No |
| CDN-proxied/orange-cloud hostname | Tako DNS-01 or a pushed CDN Origin CA certificate | Yes for DNS-01; no for a pushed certificate |
| More than 50 new hostnames per week under one registered domain | One DNS-01 wildcard certificate | Yes |

Prefer the first row when it fits: a wildcard DNS record is not the same as a
wildcard certificate. It can route any explicit hostname to Tako while Caddy
issues an ordinary HTTP-01 certificate for each configured hostname, with no
DNS token. Add DNS credentials only when the CDN prevents direct HTTP-01 or
one wildcard certificate is the safer issuance model at scale.

Deployments do not fail by default when DNS is still propagating. After a
successful reconciliation, `tako deploy` checks DNS and TLS for public domains
for up to two minutes, reports states such as `pending_dns`, `wrong_dns`,
`pending_tls`, or `active`, and exits successfully unless `--strict-domains`
is set. Use `--domain-timeout 0` for a single non-waiting check,
`--skip-domain-check` to skip the check, and `--domain-target <host-or-ip>`
when domains should point at a custom edge, CNAME target, or external proxy
instead of the inferred proxy server host.

Check domains without redeploying:

```bash
tako domains status -e production
tako domains status staging.example.com --target sites.example.com --wait 5m
tako domains status --strict --wait 2m
```

When DNS intentionally terminates at a CDN, declare that fact in config:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      web:
        build: .
        port: 3000
        proxy:
          domain: example.com
          cdn: cloudflare # or generic
```

When `proxy.cdn` is declared, valid HTTPS served from addresses other than the
Tako origin reports the additive `dns: proxied` state and passes `--strict` and
`--strict-domains`. Without the declaration, the same network observation is
only a suspected-CDN human diagnostic; machine state remains `wrong_dns` /
`wrong_target` and exits 6. Genuinely unpointed or TLS-pending domains also exit
6 even with a CDN declaration.

A CDN-fronted origin often accepts port 443 only from the CDN's address ranges
(for example CloudFront's origin-facing prefix list), so the public TLS probe
from the deploying machine can never connect. For a `proxy.cdn` domain whose
DNS routes to the origin but whose public TLS probe fails, Tako asks every
proxy node's takod for its own certificate state (Tako-pushed, DNS-01, or
Caddy-managed HTTP-01). When every proxy node holds a currently valid
certificate covering the hostname, the domain is reported as
`active (origin-restricted)` on the first check, without waiting for
`--domain-timeout`, and passes `--strict` and `--strict-domains`. If a node has
no valid certificate yet the domain stays `pending_tls` and the message names
the node. Nodes whose takod predates the `proxy.cert-status-v1` capability keep
the previous behaviour (`pending_tls` until the timeout, with a message that
explains the CDN restriction and suggests `tako upgrade servers`). Domains
without `proxy.cdn` never contact the nodes and are unchanged.

#### Cloudflare encryption modes

| Cloudflare mode | Tako support | Certificate at the Tako origin |
| --- | --- | --- |
| Flexible | Unsupported | Cloudflare uses HTTP to the origin, which loops against Tako's `:80` HTTPS redirect |
| Full | Works | Any certificate accepted by Cloudflare, including a Tako public certificate or pushed Origin CA certificate |
| Full (Strict) | Recommended | A valid public certificate from Tako DNS-01, or a matching unexpired Cloudflare Origin CA certificate pushed with `tako certs push` |

Cloudflare's **Always Use HTTPS** redirect can prevent HTTP-01 issuance or
renewal from reaching Tako. Use DNS-01 or a pushed origin certificate before
enabling it on proxied hostnames. The same reachability caveat applies to
`dynamicDomains`: those customer hostnames remain Caddy-managed on-demand
HTTP-01, so a CDN that intercepts challenges can turn the first TLS request
into an issuance failure. Config validation warns when `dynamicDomains` and
`proxy.cdn` are combined.

When a service is behind a CDN or reverse proxy, declare only that provider's
published egress CIDRs so Tako can recover the original client IP safely:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      web:
        build: .
        port: 3000
        proxy:
          domain: example.com
          cdn: cloudflare
          allowIps:
            - 198.51.100.0/24
          trustedProxies:
            - 203.0.113.0/24
            - 2001:db8::/32
```

`trustedProxies` accepts CIDRs only. IPv4 prefixes broader than `/8` and IPv6
prefixes broader than `/24` are rejected, including the catch-all networks
`0.0.0.0/0` and `::/0`. Without this setting, `allowIps` continues to match the
direct TCP peer. Networked domain checks emit a warning when access controls
appear to sit behind an external proxy/CDN without trusted proxies configured.
All routes sharing one proxy node that set `trustedProxies` must use the same
CIDR set because Caddy's client-IP trust policy is server-global; conflicting
sets are rejected rather than merged.

Per-service route policy can hide private endpoints, cap request bodies, and
set security response headers without adding another proxy container:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      web:
        build: .
        port: 3000
        proxy:
          domain: example.com
          denyPathPrefixes:
            - /api/internal
            - /.well-known/workflow
          allowPathPrefixes:
            - /api
            - /health
          maxRequestBody: 10MB
          responseHeaders:
            X-Content-Type-Options: nosniff
            Content-Security-Policy: "default-src 'self'"
```

Each `denyPathPrefixes` entry is a literal, segment-aware prefix. For example,
`/api/internal` denies both `/api/internal` and `/api/internal/users` with 404,
but does not deny `/api/internalized`. Do not add `*`; encoded paths, query
strings, and fragments are rejected in config. Denials run before basic auth,
request-body reads, and upstream proxying.

When `allowPathPrefixes` is nonempty, every other path receives 404. Allowed
prefixes use the same case-sensitive exact-or-segment-child matching, so `/api`
allows `/api` and `/api/users` but not `/API` or `/apiized`. Literal `.` and
`..` path segments, trailing slashes, and repeated slashes are rejected in
allow rules so canonicalization cannot broaden them. Deny rules always win on overlap, including
when a broader allowed prefix contains a denied internal subtree. `/` allows
all paths except explicit denials. An omitted or empty allow list leaves paths
unrestricted. The policy applies on public HTTP, the serving hostname, redirect
aliases, and internal HTTP routes. Allow-list rejections also run before
redirects, authentication, body reads, and proxying. This setting requires agents advertising
`proxy.allow-path-prefixes-v1`; deployment fails before mutation on older
agents rather than silently exposing unmatched paths.

`maxRequestBody` accepts positive `B`, `KB`, `MB`, or `GB` sizes up to 1TB.
Declared bodies over the limit return 413 before proxying. Mutation and custom
methods without `Content-Length` return 411 without reading the body. A body on
`GET`, `HEAD`, `OPTIONS`, or `TRACE` is rejected after a one-byte presence
check, while ordinary bodyless requests remain unchanged. Rejected unknown-length
requests close the client connection instead of draining an untrusted stream.
Denied paths, oversized declarations, and allowlist rejections use the same
close-on-reject behavior. Caddy also enforces the configured limit while reading
as defense in depth. `responseHeaders` names and values are validated, safely quoted, and
deferred so they replace upstream values. HTTP hop-by-hop and
message-framing headers such as `Connection`, `Content-Length`, and
`Transfer-Encoding` cannot be overridden.

### Bring-your-own TLS certificates

Push a pre-issued public or origin certificate to every proxy node in the
selected environment:

```bash
tako certs push '*.platform.example.com' \
  --cert ./fullchain.pem \
  --key ./private-key.pem \
  -e production
tako certs ls -e production
tako certs rm '*.platform.example.com' -e production
```

Use `--server node-a` to target one proxy node explicitly. Certificate and key
PEM are sent in the request body, never command arguments. The node rejects
garbage, mismatched, not-yet-valid, expired, or hostname-incompatible pairs
before changing the Caddyfile. Exact-domain certificates take precedence over
the most-specific covering wildcard; a route with no covering store entry
continues to use automatic HTTPS. Removing an entry regenerates the proxy
configuration and returns affected routes to automatic handling.

The store is node-global and deliberately shared by all projects on that node,
matching the single-operator trust model of the shared Caddy ingress. Files are
0600 and node-local: they are not replicated, drift-checked, or backed up, and
node replacement requires a re-push. Keep your own secure certificate copies.

### Embedded DNS-01 certificates

Use environment-scoped DNS-01 when HTTP-01 cannot reach the origin (for
example, an orange-clouded CDN route) or when one wildcard certificate should
cover many app hostnames. Grey-cloud routes do not need this configuration and
continue to use Caddy's automatic HTTP-01 flow unchanged.

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    proxy:
      placement:
        strategy: pinned
        servers: [production]
      acme:
        dnsProvider: cloudflare
        credentials:
          apiToken: ${CLOUDFLARE_DNS_API_TOKEN}
    services:
      web:
        build: .
        port: 3000
        proxy:
          domain: "*.platform.example.com"
          email: ops@example.com
          tls:
            challenge: dns
            provider: letsencrypt
```

Supported providers are `cloudflare`, `hetzner`, and `digitalocean`. Each
requires `credentials.apiToken`; Cloudflare also accepts an optional
`credentials.zoneToken` when zone lookup uses a separate token. Credential
values must be whole `${ENV_VAR}` references. Literal tokens are rejected
before environment expansion. Scope tokens to DNS-edit access for only the
delegated zone: each edge node stores the expanded value in a 0600 file under
the node-local certificate store so scheduled renewal can run, but the value is
never placed in the proxy container, route manifests, replicated state, result
documents, or events.

Wildcards imply DNS-01. An exact hostname opts in with
`tls.challenge: dns`; both require the environment `proxy.acme` block.
`dynamicDomains` remains Caddy-managed HTTP-01. It may coexist with a DNS-01
wildcard or explicit hostname on the same service: the explicit host block uses
the stored certificate and takes precedence over the on-demand `:443`
catch-all. DNS-01 never applies to customer hostnames admitted by the dynamic
authority. `tls.provider` supports `letsencrypt` and `zerossl`; ZeroSSL requires
`proxy.email`, and `tls.staging` uses the Let's Encrypt staging CA.

The declaring project/environment owns issuance and renewal. A different
project on the same node may consume a covering wildcard, but never issues it.
First issuance is synchronous and must finish before the route manifest is
published. Provider credentials and ownership changes are staged during
issuance and promoted only after that manifest succeeds, so a failed domain
replacement leaves the prior live route eligible for renewal. A valid stored
certificate with matching CA/staging settings makes later deploys skip
issuance. After a crash in the narrow post-route window, the scheduler promotes
a matching staged document; an unmatched abandoned stage expires after one
hour and its certificate becomes orphaned. Provider/CA failures enter a
cooldown reported by typed errors and
`tako certs ls`; rate-limit failures use the CA hint when available and a
conservative backoff otherwise. The daily scheduler asks CertMagic to refresh
ARI before applying the normal 30-day renewal window. Removing the owner route
marks the certificate orphaned and stops renewal; it is never deleted
automatically. Use `tako certs rm` after confirming it is no longer needed;
explicit removal also deletes CertMagic's managed certificate and private key.

`tako doctor` reports orphaned managed certificates and failed issuance or
renewal attempts inside the renewal window. If DNS propagation checks fail on
a split-DNS host, fix the node resolver or authoritative delegation; Tako does
not bypass propagation validation.

## Dynamic Customer Domains

For CMS-style apps that authorize generated or customer domains at runtime,
add an internal ask endpoint. The ask endpoint should return approval only for
domains your app owns.

The ask endpoint is the security boundary for dynamic domains. Return success
only for exact domains that are registered to the current app and environment.
Do not approve broad suffixes such as `*.example.com` unless every matching
hostname should route to that renderer, because Caddy will issue TLS and route
any hostname that the ask endpoint approves. Keep the lookup fast and backed
by an indexed domain table; Caddy calls this endpoint during on-demand
certificate authorization, so slow database scans can turn first requests for
new domains into user-visible TLS failures.

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      admin:
        build: ./admin
        port: 3000
      renderer:
        build: ./renderer
        port: 3000
        proxy:
          dynamicDomains:
            ask: admin:/api/domains/authorize
```

You can combine a fixed app domain and dynamic customer domains on the same
service:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      admin:
        build: ./admin
        port: 3000
      renderer:
        build: ./renderer
        port: 3000
        proxy:
          domain: sites.example.com
          dynamicDomains:
            ask: admin:/api/domains/authorize
```

## Internal Proxy Routes Without Public DNS

Use `proxy.visibility: internal` when a service should be reachable through
the shared proxy from a private network, VPN, or hosts-file mapping, but
should not be treated as a public DNS/ACME domain. Internal routes are
HTTP-only in this release and are skipped by public DNS/TLS readiness checks.

<!-- tako:snippet partial -->
```yaml
servers:
  edge:
    host: ${TAKO_EDGE_HOST}
    privateHost: 10.0.1.10
    user: root
    sshKey: ${TAKO_SSH_KEY}

environments:
  production:
    servers: [edge]
    services:
      admin:
        build: ./admin
        port: 3000
        proxy:
          visibility: internal
          # Optional. Defaults to admin.production.<project>.tako.internal.
          host: admin.production.example.tako.internal
```

Print host-file entries for clients that can reach the private address:

```bash
tako domains hosts -e production
tako domains hosts -e production --address private
tako domains hosts -e production --address mesh
```

`--address auto` is the default: it uses `servers.<name>.privateHost` when set
and falls back to the deterministic Tako mesh IP for the proxy node.

## Shared Nodes

Unrelated projects can deploy to the same server. Treat `project.name` as the
app name and the environment as the stage; that app/stage pair scopes state,
leases, env bundles, Docker labels, networks, containers, proxy files, and
generated volume names. The Caddy-backed `tako-proxy` container is shared per
node for HTTP on port 80, HTTPS on TCP 443, and HTTP/3 on UDP 443, while each
app/stage owns its own route manifest. Route manifests also record the owning
app/stage network, so if the shared proxy is recreated it reconnects to every
live network represented on the node instead of only the project that
triggered the deploy. Proxy upstreams target deterministic
project/stage-scoped container aliases instead of generic service names like
`web`, so unrelated projects can safely use the same service names on the same
node. `tako doctor` checks the server-side takod agent version, then inspects
proxy nodes and verifies that the live shared proxy has the required Caddy
config watcher, TCP 80/443 publishes, UDP 443 publish for HTTP/3, and
persistent certificate, runtime-config, route-manifest, and access-log mounts.

Destructive app operations are scoped to that same app/stage boundary.
`tako remove`, `tako destroy`, and default `tako cleanup` do not remove
unrelated project containers, volumes, proxy routes, or images. Node-wide
Docker builder cache and dangling image cleanup can affect other projects'
future build performance. Successful deploys and `tako cleanup --docker-cache`
prune builder cache with a default `20GB` keep-storage budget instead of
wiping the whole cache; takod also prunes Docker builder cache on a daily
background interval using that same budget. Override explicit cleanup with
`--docker-cache-keep-storage <size>`. Dangling image cleanup only runs during
deploy cleanup or when `tako cleanup --docker-cache` is explicitly requested.

By default every selected environment node with proxy routes reconciles the
shared proxy for that app/stage. Built-in ACME TLS for public routes currently
requires the proxy placement to resolve to one node, because distributed
certificate issuance/storage is not implemented yet. To keep ingress on a
dedicated edge node, set `environment.proxy.placement` with a pinned server or
node-label constraint:

<!-- tako:snippet partial -->
```yaml
servers:
  edge-1:
    host: edge.example.com
    user: deploy
    sshKey: ${TAKO_SSH_KEY}
    labels:
      role: edge
  app-1:
    host: app.example.com
    user: deploy
    sshKey: ${TAKO_SSH_KEY}
    labels:
      role: app

environments:
  production:
    servers: [edge-1, app-1]
    proxy:
      placement:
        constraints:
          - node.labels.role==edge
    services:
      web:
        build: .
        port: 3000
        proxy:
          domain: example.com
```

### Cross-Project Service Exports

Services can opt into cross-project access with `export: true`. Tako attaches
each exported service to its own export network, so importing another project
does not expose that project's private services. Consumers declare
`imports: [other-app.api]` and call the service through the readable DNS alias
`other-app-production-api` in the same environment. Export networks carry
`tako.discovery=export` labels with app, environment, service, and alias
metadata. Use `tako discovery exports` to inspect those records on reachable
nodes, or `tako discovery exports --all-environments --server <node>` when
auditing a shared host.

## Deploy Order and Concurrency

Tako deploys services one at a time, in dependency order. It resolves
`dependsOn` into a single deterministic order (services that become ready at
the same time are ordered by name), and each service finishes its rollout
before the next one starts. If a service fails, the services after it are not
deployed.

Within one service, the rollout to each assigned node runs concurrently, so a
service with replicas on several nodes updates those nodes in parallel.
Per-service rollout behavior (recreate, rolling, or blue/green) is set with
`services.<name>.deploy.strategy`; see [Deploy Strategies](#deploy-strategies)
for the default.

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      postgres:
        image: postgres:16-alpine
        persistent: true
        volumes:
          - postgres_data:/var/lib/postgresql/data
        env:
          POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
      api:
        build: ./api
        port: 4000
        dependsOn: [postgres]   # deploys after postgres
        deploy:
          strategy: rolling     # how this service's own replicas roll out
      web:
        build: ./web
        port: 3000
        dependsOn: [api]        # deploys after api
        proxy:
          domain: app.example.com
          email: admin@example.com
```

Build location is configured separately under `deployment.build` (see
[Build Strategy](#build-strategy)):

<!-- tako:snippet partial -->
```yaml
deployment:
  build:
    strategy: remote  # remote, local, or auto
```

## Deploy Strategies

`services.<name>.deploy.strategy` controls how one service replaces its
containers:

- **`rolling`** starts the new revision next to the old one, waits for it to
  become ready, moves proxy traffic, then stops the old revision. No downtime.
- **`blue_green`** warms a full new revision and cuts traffic over at once
  (optionally after `promotion: manual` or a `gracePeriod`).
- **`recreate`** stops the old containers, then starts the new ones. There is
  a short gap while the new container starts.

### The default

When `deploy.strategy` is not set, Tako picks `rolling` for a service that can
run two revisions side by side, and `recreate` otherwise. A service defaults to
`rolling` only when all of these hold:

- it is a long-running service (not `kind: job` or `kind: run`) and not
  `persistent`;
- it has a `port` and no raw `ports` or `volumes`;
- `deploy.maxUnavailable` is not set, and `deploy.maxSurge` is unset or at
  least `replicas`;
- it does not set `deploy.gracePeriod`, `deploy.promotion` or
  `deploy.smokeTest`, which belong to `blue_green`;
- its other `deploy` settings are valid for `rolling`.

Everything else (databases, services with volumes or host ports, workers
without a port) defaults to `recreate`. A defaulted service never fails
validation because of the default; `tako config` prints
`deploy.strategy: rolling (default)` so you can see which one Tako picked, and
`tako deploy --plan-only` shows the strategy for every changed service.

With the default `rolling`, `maxSurge` is the replica count: the whole new
revision runs next to the old one before traffic moves. That briefly doubles
the service's memory and CPU on its nodes, so leave headroom on small hosts.

The first deploy after upgrading to a CLI with this default redeploys every
service that ran with the old `recreate` default. The plan lists those
services with the reason `Deploy strategy default changed: recreate → rolling`,
and the redeploy itself is rolling, so it has no downtime.

### Readiness for defaulted rolling services

Traffic only moves to a new revision once it is ready. If a defaulted
`rolling` service has no `healthCheck` and no `deploy.readiness`, Tako waits
for the container to accept TCP connections on its `port`, for about a minute.
If it never does, the rollout fails, the old revision keeps serving, and the
error names the implicit probe. Fix it by making the app listen on `0.0.0.0`,
adding a `healthCheck` that matches your app, or opting out with
`deploy.strategy: recreate`.

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      web:
        build: .
        port: 3000
        healthCheck:
          path: /health        # replaces the implicit TCP probe
        proxy:
          domain: app.example.com
          email: admin@example.com
```

### Recovering from a failed rollout

`deploy.rollbackOnFailure` defaults to `true`. When a rollout fails:

- **`rolling` and `blue_green`**: the old revision never stopped and traffic
  never moved, so it keeps serving. The failed containers are removed.
- **`recreate`**: Tako stops the old containers and keeps them (renamed and
  stopped) instead of removing them. If the new revision fails to start,
  connect, or pass its health check on any node, Tako removes the new
  containers and restarts the old ones under their original names on every
  node of that service. The old containers are removed once the rollout
  succeeds everywhere.

The deploy still fails (exit code 1) and reports the service as `restored`.
Restore applies only to the service that failed: services deployed earlier in
the same run keep their new revision, and later services are not deployed.

Restore restarts the previous containers; it does not undo changes the new
revision made to data. If a new image migrated a database or wrote to a volume
before failing, the restored container sees that data. Use a `deploy.release`
command for migrations so they run before any container is replaced, and keep
migrations backward compatible.

Set `rollbackOnFailure: false` to keep failed containers stopped for
debugging instead. The old revision of a `rolling` service still serves; a
`recreate` service is left down until you fix it and deploy again.

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      worker:
        build: .
        deploy:
          strategy: recreate
          rollbackOnFailure: false   # keep failed containers for `docker logs`
```

Restoring `recreate` services and keeping failed containers
(`rollbackOnFailure: false`) need the `deploy.restore-on-failure-v1` takod
capability, which `tako upgrade servers` installs. Defaulted `rolling` services
do not need it. If a node lacks it and `rollbackOnFailure` is not set, Tako
warns and deploys that service with the old remove-then-start behavior on all
of its nodes. If `rollbackOnFailure` is set explicitly, the deploy stops before
touching any container and asks you to run `tako upgrade servers`.

### Opting out

To keep the previous behavior for a service, set the strategy explicitly:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      api:
        build: .
        port: 4000
        deploy:
          strategy: recreate
```

### Deprecated keys

The keys below are still accepted but have no effect. `tako validate` and
`tako deploy` print one warning per key that is present (shown as a `warn`
finding in `tako validate --output json`), starting in `v0.14.0`. They will be
rejected starting in `v0.15.0`.

| Key | What to do instead | Rejected from |
| --- | --- | --- |
| `deployment.strategy` | Remove it. Services deploy sequentially in dependency order; use `dependsOn` for ordering and `services.<name>.deploy.strategy` for rollout. | `v0.15.0` |
| `deployment.parallel` (`maxConcurrentBuilds`, `maxConcurrentDeploys`, `strategy`) | Remove it, as for `deployment.strategy`. | `v0.15.0` |
| `deployment.cache` (`enabled`, `type`, `retention`) | Remove it. Docker manages build cache; use `tako cleanup --docker-cache` or the takod `--build-cache-keep-storage` setting. | `v0.15.0` |
| `environments.<env>.labels` | Remove it. Set `servers.<name>.labels` and select servers with `serverSelector.labels` or placement constraints. | `v0.15.0` |

## Rolling Back

`tako rollback` returns an environment to an earlier deployment. It needs no
configuration; it reads the replicated deployment history (`tako history`).

```bash
tako rollback --plan-only             # review: newest stable deployment before the current one
tako rollback -y                      # roll every eligible service back to it
tako rollback 1767225600 -y           # a deployment ID, the short ID from `tako history`, or a git commit (7+ chars)
tako rollback --service web           # one service only, previous deployment that contains it
tako rollback --service web 1767225600
```

Project-wide rollback rebuilds each service's state at the target from the
newest stable history record at or before it, so services the target
deployment did not touch return to the version they ran at that time. It
shows the plan, then asks for confirmation (`[y/N]`); pass `-y/--yes` in
scripts. Non-interactive sessions (`CI`, `TAKO_NONINTERACTIVE`, no TTY)
without `--yes` stop with exit 2. `--plan-only` prints the plan and changes
nothing, and `--plan FILE` applies only when the fresh plan still matches a
reviewed `--plan-only --output json` document.

How each service is handled:

| Service | Project rollback |
| --- | --- |
| Long-running service | Rolled back in dependency order. Already at the target image, replica count and operator-file content: `unchanged`. |
| `persistent: true` | Skipped. Pass `--include-persistent` to include it; Tako warns that the old image must still understand the current data on its volumes. |
| `kind: run` | Skipped. Deploy-time runs are not replayed. |
| `kind: job` | Skipped with a warning: job images and schedules stay at the current deployment. |
| Added after the target | Skipped (`not present at target`), left running. Planning fails with exit 2 if history has been pruned to its 50-entry limit and the service's state at the target can no longer be proven; use `tako rollback --service` instead. |

`deploy.release` commands are skipped by default because migrations are
usually not reversible; pass `--run-release` to run them for the rolled-back
services. Rollout stops at the first failed service; the remaining services
are reported `not_attempted`, and proxy routes, node state and one history
entry are still written for the services that changed. The history entry has
status `rolled_back` (or `failed` after a partial rollback), so running
`tako rollback -y` again returns to the deployment you rolled back from.

Before changing anything, Tako checks that every image it needs is available:
the exact image still retained on the environment's nodes (reused without a
build), the target commit in the local git repository (rebuilt), or a registry
image (pulled). `tako cleanup` keeps the newest images per service and
environment (`--keep`, default 3), so older targets of build-backed services
usually need the commit to rebuild; shared-build images must still be retained.
Cleanup only removes images the node's deployment history attributes to the
cleaned environment alone. It never removes an image another environment's
history references, an image in any current desired or actual state on the
node, or an image a container still uses. If that state cannot be read, it
removes no images. If any image is
unavailable, the plan fails with exit 1 and nothing is changed. Finding
retained images needs a node agent with the `image.descriptor-v1` capability;
run `tako upgrade servers` if the plan asks for it.

## Build Strategy

Build-backed services use `deployment.build.strategy: remote` by default: Tako
streams the build context to each assigned server and builds there with takod.
For stronger developer or CI machines, use local build mode:

<!-- tako:snippet partial -->
```yaml
deployment:
  build:
    strategy: auto # try local buildx, fall back to remote takod build
```

`local` builds once per target architecture with `docker buildx build
--platform linux/amd64|linux/arm64 --load`, selects the resulting immutable
image ID, and streams that archive to each assigned node through the same
structured local-or-SSH agent transport used by deployment. It does not open
a separate shell or require `docker pussh`. `auto` uses the same path when
local Docker and buildx are available and otherwise falls back to `remote`.

Use `remote` when the server is intentionally the build host, `local` when CI
or a developer workstation should build and push images to the VPS, and `auto`
for portable config that prefers local builds but preserves the older
server-build path.

Build services may use the legacy context string or structured build options.
Both remote takod builds and local buildx builds receive the same arguments and
target stage. Build-arg values travel in the streamed request body, not its URL.
Build arguments are ordinary build configuration, not a secret channel: their
values affect one-way config/artifact digests and must not contain passwords,
tokens, or other low-entropy secrets. Use runtime `secrets:` or operator files
for secret material instead.

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      web:
        build:
          context: .
          args:
            BASE_IMAGE: node:24-alpine
          target: runtime
```

When several services are variants of one image, declare a top-level shared
build and reference it with `imageFrom`. Tako assigns one image repository and
revision tag to the build, builds/transfers it once across the union of consumer
placement nodes, then starts every consumer from that exact image without
rebuilding or pulling it again.

<!-- tako:snippet partial -->
```yaml
builds:
  application:
    context: ./sentry
    args:
      SENTRY_IMAGE: getsentry/sentry:26.6.0
    target: runtime
    dockerfile: Dockerfile

environments:
  production:
    services:
      web:
        imageFrom: application
        command: [sentry, run, web]
      worker:
        imageFrom: application
        command: [sentry, run, worker]
      cleanup:
        kind: job
        imageFrom: application
        schedule: "0 0 * * *"
        command: [sentry, cleanup, --days, "30"]
```

Shared build definitions accept `context`, `args`, `target`, and `dockerfile`.
Their definition fingerprint joins every consumer's drift/revision identity, so
changing the context path, arguments, target, or Dockerfile reconciles all
consumers. For `kind: run`, `imageFrom` may still reference another service;
Tako rejects a run reference that is ambiguous between a build and a service.
Shared images use a dedicated `<project>/shared/<build>` repository and a tag
that combines the deploy revision with the build-definition fingerprint, so a
service of the same name cannot overwrite them and changed build options cannot
silently reuse an older image. Scale and rollback transfer the exact selected
image from a same-architecture environment node to newly assigned nodes; they
fail before reconciliation with guidance to run a normal deploy when no source
node retains it.

Use `envFiles` when a service composes multiple environment files. Files load
in list order (later files override earlier files), then explicit `env:` and
Tako `secrets:` take precedence. `envFile` remains supported for one file;
configuring both forms is rejected.

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      worker:
        image: example/worker:latest
        envFiles: [.env.base, .env.production]
```

Credential-like values loaded from `envFile`/`envFiles` are masked in
command output by the same rule as inline `env:` values (a credential-like
name, a URL with credentials or a token-like part, or a long token mixing
letters and digits) and show as `[REDACTED:envFile.<NAME>]`. Commands that
only read logs or state (`logs`, `rollback`, `promote`, `scale`, `remove`,
`destroy`) warn once when an envFile cannot be read on the operator machine,
because its values will not be masked.

Every mask names its source: `[REDACTED:secrets.STRIPE_KEY]`,
`[REDACTED:env.DATABASE_URL]`, `[REDACTED:servers.app.password]`, or
`[REDACTED:pattern.jwt]` for a heuristic match. Set
`TAKO_REDACTION_LABELS=off` to print plain `[REDACTED]` markers instead. To
see, before deploying, what Tako will mask and why, run:

```bash
tako config explain --redaction -e production
```

```text
Redaction: production (config tako.yaml)
Labels: on (TAKO_REDACTION_LABELS)

Masked
  servers.app.password                   ssh password
  services.mongo.backup.secretAccessKey  backup storage credential

Services
  admin:
    masked   secrets.STRIPE_KEY      secrets list (from .tako/secrets.production)
    masked   envFile.SMTP_PASSWORD   credential-like name (.env.production)
    visible  env.CMS_RUNTIME_MODE    plain setting
```

The report never prints a value, never contacts servers and never runs
secret provider CLIs; keys that would come from `secretSources` are listed
as `from secretSources[i] <provider>; not resolved here`, and `secrets:`
references that nothing can resolve are listed under "Not resolvable".

Container runtime controls map directly to Docker run options and work for
long-running services and scheduled jobs:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      database:
        image: postgres:17
        user: "999:999"
        workingDir: /var/lib/postgresql
        stopGracePeriod: 60s
        init: true
        extraHosts: [host.docker.internal:host-gateway]
        ulimits:
          nofile:
            soft: 262144
            hard: 262144
        shmSize: 256m
```

`stopGracePeriod` uses whole seconds and is capped at 24 hours. `ulimits`
accepts either a positive scalar (same soft/hard value) or explicit positive
`soft`/`hard` values. These controls require an agent advertising
`container.runtime-controls-v1`; Tako checks before container or schedule
reconciliation and fails with upgrade guidance when a development node is
stale.

Long-running services can opt into bounded Docker log rotation independently
of the Docker daemon defaults:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      worker:
        image: example/worker:latest
        logging:
          driver: local
          maxSize: 25m
          maxFiles: 5
```

`driver`, `maxSize`, and `maxFiles` are all required when `logging` is set.
The supported drivers are Docker's `local` and `json-file` drivers, both of
which remain readable through `tako logs`. `maxSize` is a positive integer
followed by `k`, `m`, or `g`, is normalized to lowercase, and cannot exceed
`10g`; `maxFiles` is from 1 through 100. The setting applies only to
long-running services: deploy-time runs and scheduled jobs reject it.

When a service omits `logging`, a takod with `node.disk-hygiene-v1` bounds
its new containers with `json-file` rotation (`max-size=20m`, `max-file=5`),
so one chatty container cannot fill the disk. The default is node policy: it
is not part of the desired document or the config hash, so it never causes a
redeploy, and existing containers keep their log options until they are next
replaced. takod leaves the daemon's logging untouched (and adds nothing) when
the Docker default driver is not `json-file`, when `/etc/docker/daemon.json`
sets `log-driver` or any `log-opts` key, or when `daemon.json` is unreadable.
To opt out on a node, run takod with `--default-container-logging=off` (see
the systemd drop-in in [Resource Monitoring](RESOURCE-MONITORING.md#disk-hygiene));
`--default-container-log-max-size` and `--default-container-log-max-file`
change the bounds. An explicit `logging` block always wins.

Configured logging requires an agent advertising `container.logging-v1`, and
Tako fails capability preflight before any deployment mutation on a stale
agent.

## Scheduler Alerts

`notifications.alerts` enables durable alerts for cron-fired jobs and native
scheduled volume backups. The existing Slack, Discord, and generic webhook
targets are reused:

<!-- tako:snippet partial -->
```yaml
notifications:
  slack: ${SLACK_WEBHOOK_URL}
  webhook: ${OPERATIONS_WEBHOOK_URL}
  alerts:
    jobs:
      consecutiveFailures: 2
      consecutiveTimeouts: 1
      consecutiveSkipped: 2
      noSuccessFor: 30m
    backups:
      maxAge: 25h
```

Thresholds set to `0` and omitted durations are disabled. Durations must use
whole seconds between `1m` and `8760h`. Job counters cover scheduled firings only; manually
triggered runs remain in job history but cannot open or recover a scheduler
episode. A status change resets the other consecutive-status counters, and a
successful scheduled run resets all three. `noSuccessFor` starts when alerting
is configured, or from the latest successful scheduled run when one exists.

`backups.maxAge` tracks successful backups made by Tako's native
`backup.schedule`. A custom PostgreSQL `pg_dump` S3 service declared as
`kind: job` is intentionally governed by the job policy instead. This avoids
claiming that a generic Docker volume archive is a PostgreSQL-consistent dump.

Each owning node persists secret-free attempt facts, one deduplicated episode,
and a bounded delivery outbox. A `running` fact is committed before scheduled
work starts. After a takod restart, an interrupted job container is removed and
the attempt is closed as failed; an interrupted native backup is likewise
closed as failed before schedules restart. Scheduler shutdown cancels active
callbacks and waits for them to publish terminal state. Recovery verifies the
helper's persisted name and immutable project/environment/service/attempt labels
before removing it. Cancellation uses a fresh bounded cleanup context; if helper
absence cannot be confirmed, the durable attempt remains `running` for startup
recovery instead of being falsely terminalized. Delivery is attempted at most three times; daemon shutdown leaves
an interrupted HTTP attempt pending instead of exhausting it. A
recovery is sent once, and only to providers that successfully received the
opening event. Generic webhook events carry a stable `eventId` and `episodeId`
so receivers can make at-least-once delivery idempotent. Alert status and
delivery errors never include run output, webhook URLs, or credentials; target
credentials are kept in a separate root-only (`0600`) node-local config file.
Removing a subject or disabling the policy first retires any externally opened
episode; credentials remain until matching recoveries are delivered or
exhausted.
Dispatch intent is persisted before the HTTP request. A restart retries the
same event ID, including an opening that raced policy removal. Target rotation
closes the old generation and retains its credential file until that recovery
finishes, while new openings use the new generation.

Alert targets must use HTTPS. Slack and Discord URLs must use their official
webhook hosts; generic webhooks may use another public host. takod resolves and
dials the validated public address directly, rejects local/private/special-use
addresses, and does not follow redirects.

Deploys apply the policy, including explicit removal when `alerts` is absent,
before an otherwise no-op deploy returns. Enabled policies reach every
schedulable node in the environment, not only this deploy's workload targets.
Every other environment node receives a disable request so a newly cordoned or
draining node cannot retain an old policy or credentials. A valid controller
operation fence can disable alerts on a cordoned or draining node; enable
requests additionally require an active schedulable node. The deploy operation fence includes every node that
receives alert reconciliation while workload reconciliation remains
placement-scoped. Stale owners are disabled only after every replacement owner
accepts the policy, so a failed deploy preserves the last working alert path.
Enabling
alerts requires every target agent to advertise
`scheduler.alerts-v1`; an older node fails preflight with upgrade guidance
instead of silently ignoring the policy. When alerts are absent, incapable
legacy agents are skipped because they cannot retain alert state, while capable
agents receive the disable request and delete their credentials.

Node-local alert delivery cannot detect that its own node is down. Run
`tako prometheus` from an independent observer and alert on
`tako_scheduler_alert_owner_present == 0`,
`tako_scheduler_alert_owner_reachable == 0`, active episodes, or exhausted
deliveries.

## Health Monitoring Alerts

takod also checks service health and node, container and certificate
conditions (`monitoring.health-v1`). It sends alerts to the same targets:

<!-- tako:snippet partial -->
```yaml
notifications:
  slack: ${SLACK_WEBHOOK_URL}
  alerts:
    services:
      alertAfter: 3
      recoverAfter: 2
    host:
      disk: 90
      inodes: 90
      memory: 90
      cpu: 95
      cpuFor: 10m
      load: 4
      conditions: true
    containers:
      oom: true
      crashLoop: 3
      crashLoopWindow: 10m
    certificates:
      warnBefore: 336h
      criticalBefore: 72h
environments:
  production:
    services:
      web:
        image: nginx:1.27
        port: 80
        proxy:
          domain: www.example.com
        monitoring:
          enabled: true
          interval: 30s
          timeout: 5s
          alertAfter: 2
          checkType: http
          webhook: https://hooks.example.com/tako/web
```

Defaults apply whenever an `alerts` block exists, even when these sections
are omitted:

| Setting | Default |
|---|---|
| `services.alertAfter` / `recoverAfter` | 3 / 2 |
| `host.disk` / `host.inodes` | 90% (critical at 95%) |
| `host.memory`, `host.cpu`, `host.load` | off |
| `host.conditions` | on |
| `containers.oom` | on |
| `containers.crashLoop` | 3 restarts in 10m |
| `certificates.warnBefore` / `criticalBefore` | 336h / 72h |

To disable a threshold, set it to `0`. Percentages range from 1 to 99.

`host` and `certificates` subjects belong to the whole node. They require a
shared target (`slack`, `discord` or `webhook`), and each target is alerted
once per node, even when several projects share it.

Per-service settings:

- `monitoring.interval`: 10s to 1h, default 60s. An out-of-range value from
  an older config produces a warning and is clamped.
- `monitoring.timeout`: 1s to 60s, shorter than the interval, default 10s.
- `monitoring.alertAfter` / `recoverAfter`: override the service defaults.
- `monitoring.webhook`: an extra public HTTPS target that receives only that
  service's alerts. A value that fails alert target validation was accepted
  by earlier releases. It now produces a deprecation warning and is never
  sent.
- `monitoring` on job and run services is ignored.

`http` checks run on the node that serves the proxy route (the first
schedulable proxy server). takod dials its local proxy using the service
domain and checks the health check path, the status code, and the
certificate's hostname and validity. A service with no routable domain falls
back to a `container` check, with a warning. Container checks compare
running replicas on each node that has assignments.

Checks report `unknown` until the service is deployed. They are paused
while a deploy holds the lease, for `max(startPeriod, 30s)` after a deploy,
during maintenance mode, and while removal is pending.

On nodes without `monitoring.health-v1`, deploy prints "node-side monitoring
inactive on <node>; run tako upgrade servers" and continues. Deploy fails
with exit code 2 only when the new keys (`alerts.services`, `host`,
`containers`, `certificates`, or `monitoring.timeout`, `alertAfter`,
`recoverAfter`) are set explicitly. Read state with `tako monitor`. The
payload schema, flap control and Prometheus series are described in
[Resource Monitoring](RESOURCE-MONITORING.md#4-alerting).

## Operator Files

`files:` distributes local regular files or whole directories to the node and
mounts them read-only into a service. Sources are resolved relative to
`tako.yaml`. Content is transported in the reconcile request, written
atomically into a content-addressed version below
`/var/lib/tako/files/<project>/<environment>/<service>/`, and is not stored in
desired state or deployment history.

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      relay:
        image: getsentry/relay:26.6.0
        files:
          - source: ./relay/config.yml
            target: /work/.relay/config.yml
          - source: ./relay/credentials.json
            target: /work/.relay/credentials.json
            secret: true
            owner: "1000:1000"

      nginx:
        image: nginx:1.27-alpine
        files:
          - source: ./nginx
            target: /etc/nginx
```

File bytes are binary-safe, and directory sources preserve their relative
tree, empty directories, and executable bits. Symlinks and special files are
rejected. `secret: true` forces directories to `0700` and files to `0600`
(or `0700` when the source is owner-executable). `owner` accepts a numeric
`uid` or `uid:gid`; it defaults to a numeric service `user`, then `root:root`.
Set it whenever a non-root image user must read a secret tree. A service may
declare up to 128 roots, 16,384 total entries, 128 MiB of raw file content, and
256 MiB in the encoded bundle; service-file HTTP requests are capped at 384
MiB. File target, mode, and content digests join the service fingerprint;
changing only file bytes therefore triggers reconciliation without exposing
the bytes in state, events, labels, or machine results.

Published versions are immutable. Standard-service deploys replicate each set,
including `secret: true` content, to every server currently configured for the
environment so later placement changes within that fleet can reuse it. A failed
rollout leaves earlier versions untouched, and deployment history records the
selected content hash and mount metadata so rollback can remount the exact
prior bytes without rereading today's source files. A server added or replaced
after that revision does not have its retained set; rollback fails before
reconciliation if the selected placement needs such a node. Versions are
removed only when the standard service/project is removed or deployment files
are explicitly cleaned. Deploy-time `kind: run` sets are deleted after the
one-off exits, and `kind: job` retains only the currently scheduled set once no
run still references an older one.

## Deploy-Time Runs

`kind: run` declares a run-to-completion step inside the deployment dependency
graph. Its `command` must be an argv list. The step runs in a fresh `--rm`
container at its topological slot, and dependent services start only after it
exits 0. A non-zero exit aborts the deployment.

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      migrate:
        kind: run
        image: getsentry/sentry:26.6.0
        command: [sentry, upgrade, --noinput]
        timeout: 30m

      web:
        image: getsentry/sentry:26.6.0
        dependsOn: [migrate]
```

Use `imageFrom` to reuse another service's resolved image. Tako adds that
service as an implicit dependency; build-backed image sources must share at
least one eligible node with the run.

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      app:
        build: .
      migrate:
        kind: run
        imageFrom: app
        command: [bin/app, migrate]
```

Successful runs record their resolved-image/config fingerprint, command, node,
exit code, and duration in deployment history and machine-readable deploy
results. The fingerprint includes a non-reversible digest of resolved env-file
and secret values, so value-only input changes rerun without storing those
values. A later deploy skips an already-completed fingerprint. `tako deploy
--force` reruns it. Run services accept env/envFiles, secrets, volumes,
entrypoint, labels, placement, resources, and the runtime controls above; they
do not create a persistent service, route, schedule, or health check.

Use a one-off override from CI or a dev machine:

```bash
tako deploy --build-strategy local
tako deploy --build-strategy auto
```

Local build mode needs only Docker with buildx on the client. Image handoff uses
Tako's structured node transport and does not require an additional Docker CLI
plugin.

## Command Aliases

`aliases:` records project shortcuts for long Tako invocations next to
`tako.yaml`. `tako <alias> [args...]` runs `tako <alias args...> [args...]`.

<!-- tako:snippet partial -->
```yaml
aliases:
  # List form (preferred): each item is one Tako argument, used verbatim.
  console: [exec, -it, web, --, bin/rails, console]
  migrate: [exec, web, --oneoff, --, php, artisan, migrate]
  stg-console: [exec, -it, web, -e, staging, --, bin/rails, console]
  # String form: split into arguments without a shell.
  logsw: logs --service web --follow
  shell: exec -it web -- sh -c "cd /app && exec bash"
```

```bash
tako console                       # tako exec -it web -- bin/rails console
tako logsw --tail 50              # tako logs --service web --follow --tail 50
tako -e staging logsw              # tako -e staging logs --service web --follow
tako --config deploy/tako.yaml -v migrate
tako aliases                       # list aliases and what they run
```

Rules:

- **Names** match `^[a-z0-9][a-z0-9_-]*$` (at most 63 characters). `help`,
  `completion`, `__complete`, `__completeNoDesc`, and `aliases` are reserved
  and rejected.
- **Values** are Tako arguments, not shell commands. The first argument must
  be a visible top-level Tako command (`exec`, `logs`, `deploy`, ...); it
  cannot be a flag, `tako` itself, another alias, `help`, `completion`,
  `takod`, or a hidden command. Expansion is single-level, so aliases never
  reference each other.
- **String form** splits on unquoted whitespace. `'...'` is literal, `"..."`
  honors `\"` and `\\`, and `\x` outside quotes yields `x`. There is no
  variable expansion, globbing, `~`, command substitution, or operator
  handling: `;`, `|`, `&&`, `$(...)`, and `*` are passed to Tako as ordinary
  text. To run a shell pipeline on the server, pass it to a remote shell as in
  the `shell` example above. An unterminated quote or an empty value is a
  validation error.
- **Extra arguments** typed after the alias name are appended unchanged. When
  the alias already contains `--`, they land after it and reach the remote
  command: with `rails: [exec, -it, web, --, bin/rails]`, `tako rails
  db:migrate:status` runs `bin/rails db:migrate:status`.
- **Global flags** that affect alias lookup (`--config`) go before the alias
  name. Aliases are read from that config file.
- **Environments**: an alias may pin one with `-e`/`--env` before any `--`.
  Passing a different `-e` on the command line fails with exit code 2
  (`alias stg-console pins environment staging; ...`); the same value is
  accepted. Unpinned aliases follow `-e` and the default environment as usual.
- **Forbidden flags**: `--config` and `--host-key-mode` (either spelling)
  are rejected before `--`, so a committed alias cannot redirect the config
  or weaken SSH host key checks. After `--` they are remote command data.
- **Literal values**: `${...}` inside an alias is a validation error, reported
  before environment expansion. Aliases are never env-expanded.
- **Built-ins win.** A name that matches a built-in command or built-in
  command alias always runs the built-in, without reading the config. Such an
  alias is not a load error (a future Tako release adding a command must not
  break an existing `tako.yaml`), but `tako validate` and `tako aliases` warn
  that it is shadowed and can never run.

An invoked alias behaves exactly like the expanded command: same flags, exit
codes, `--output json`/`--events ndjson` support or human-only rejection, and
project-mutation preflight. `-v` prints the expansion to stderr
(`alias logsw -> tako -v logs --service web --follow`); nothing extra is
written to stdout, and no extra NDJSON event is emitted.

`tako --help` lists aliases under "Project Aliases", `tako help <alias>` shows
the expansion, and shell completion offers alias names, all when a valid
config is present. Completion does not offer the target command's flags after
an alias name. Generated man pages and the tracked completion scripts never
include project aliases.

`tako aliases` lists every alias with its expansion and shadowing status
without contacting servers or requiring env vars referenced elsewhere in the
config. `--output json` returns an `AliasList` document. Invoking an alias
also reads only the `aliases` section, so an invalid alias fails with exit
code 2 while built-in commands are unaffected. Commands that load the whole
config (`deploy`, `validate`, ...) reject invalid aliases like any other
invalid section.

`aliases:` requires a Tako release with alias support; older releases reject
the key as `unknown field "aliases"`.

## Lifecycle Hooks

`hooks:` configures local scripts that run on the machine executing the CLI
around `tako deploy`, `tako rollback`, `tako promote`, and `tako setup`. Hooks
never run on a node. Hook files live in `.tako-hooks/` next to `tako.yaml`;
the block is only needed to change the defaults or to declare argv commands.
See [HOOKS.md](./HOOKS.md) for hook names, placement, the `TAKO_*`
environment, and failure semantics.

<!-- tako:snippet partial -->
```yaml
hooks:
  path: .tako-hooks          # default; relative to the config file's directory
  timeout: 5m                # per hook, 1s..1h
  output: verbose            # verbose | quiet (quiet emits hook output at debug level)
  passEnv:                   # process variables a hook may see
    - GITHUB_TOKEN
    - SLACK_*                # PREFIX* matches every SLACK_... variable
  commands:                  # argv form, run without a shell
    pre-deploy: [make, check]
    post-deploy: [./scripts/notify.sh]
```

| Key | Default | Rules |
|-----|---------|-------|
| `path` | `.tako-hooks` | Relative to the config file's directory. Absolute paths, paths that escape the project, and paths under `.tako/` (gitignored, so hooks there would never be committed) are rejected. |
| `timeout` | `5m` | Go duration from `1s` to `1h`, applied to each hook. On timeout the hook's whole process tree is killed and the hook counts as failed. |
| `output` | `verbose` | `quiet` keeps hook output out of the terminal unless `--verbose`; it still appears as debug-level `hook.output` events. |
| `passEnv` | none | Exact variable names or `PREFIX*` patterns. Names starting with `TAKO_` are rejected. |
| `commands` | none | Map from hook name to a non-empty argv list. Unknown hook names are rejected. `argv[0]` is looked up on PATH; a value containing a path separator is resolved relative to the project directory. |

Secret rules:

- A hook gets a minimal baseline environment (`PATH`, home and temp
  directories, locale, terminal, Windows system variables, `CI`), the
  variables matched by `passEnv`, and the `TAKO_*` context. Nothing else is
  inherited: `.env` keys, `.tako/secrets`, and `secretSources` values never
  reach a hook unless you pass the same process variable through `passEnv`.
- `${VAR}` references inside `hooks.commands` are a validation error, so a
  secret is never expanded into a committed argv or echoed in hook events.
  Read secrets from the environment inside the hook and list them in
  `passEnv`.
- Values of passed variables whose names look like credentials (`TOKEN`,
  `SECRET`, `KEY`, `PASSWORD`, `PASS`, `CREDENTIAL`, `AUTH`) are redacted from
  hook output and events.

A hook defined both as a file and in `hooks.commands` is an error. `tako
validate` checks hooks without running them.

## Container Resource Limits

Set a Docker memory limit per service with `resources.memory`:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      web:
        build: .
        resources:
          memory: 512m
```

Tako passes this to Docker as `--memory`. Accepted units are Docker-style
byte, k, m, or g values such as `512m`, `1g`, or `768mb`.

## Raw TCP/UDP Ports

`proxy` covers HTTP(S) traffic. For protocols the proxy cannot terminate —
game servers, MQTT brokers, SMTP, externally reachable databases — publish
host ports directly on the node with `ports`:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      minecraft:
        image: itzg/minecraft-server
        persistent: true
        volumes:
          - mc-data:/data
        ports:
          - "25565:25565"       # host:container, TCP by default
          - "19132:19132/udp"
          - "127.0.0.1:9090:9090"  # bind a specific interface only
```

Entries use docker-compose publish syntax (`PORT`, `HOST:CONTAINER`,
`IP:HOST:CONTAINER`, optional `/tcp` or `/udp`) and are passed to Docker as
`--publish`. Traffic reaches the container directly — no TLS termination,
health-gated routing, or basic auth from tako-proxy applies. Constraints:

- **`deploy.strategy` must be `recreate`**, which is the default for services
  with `ports` (see [Deploy Strategies](#deploy-strategies)). Rolling and
  blue-green keep the old and new revision running at once, and the second
  container cannot bind an already-bound host port, so raw port services
  take a brief listener gap on deploy.
- **At most one replica** — a host port binds once per node.
- Host ports **80 and 443 are reserved** for tako-proxy in environments
  that route any service through it.
- A host port (per protocol) can be published by **only one service** per
  environment.
- In **multi-node environments** the service must set `placement.strategy`
  to `pinned` or `global` so the published endpoint has an explicit home
  (`global` binds the port on every node).
- On multi-node setups, prefer host ports below 20000 — Tako allocates
  20000–65000 dynamically for cross-node proxy upstreams.

## Volume Backups

Off-node backups are opt-in per service. Tako schedules them on the takod node
that owns the service volume and can upload archives to any S3-compatible
object store, including AWS S3, Cloudflare R2, MinIO, Backblaze B2 S3, and
DigitalOcean Spaces. Manual `tako backup --all` and `tako backup --volume
<name>` runs reuse the same storage settings when the target volume belongs to
a service with `backup.storage` configured. If remote upload or remote
retention cleanup fails after the local archive is created, Tako keeps the
local archive and reports a warning so the backup remains available for
node-local restore:

<!-- tako:snippet partial -->
```yaml
environments:
  production:
    services:
      postgres:
        image: postgres:16-alpine
        persistent: true
        volumes:
          - pgdata:/var/lib/postgresql/data
        placement:
          strategy: pinned
          servers: [production]
        backup:
          schedule: "0 2 * * *" # daily at 02:00 UTC
          retain: 14
          storage:
            provider: r2 # s3, r2, or s3-compatible
            bucket: ${TAKO_BACKUP_BUCKET}
            region: auto
            endpoint: ${TAKO_BACKUP_ENDPOINT}
            prefix: postgres
            versioned: false # true only when bucket version APIs are enabled
            accessKeyId: ${TAKO_BACKUP_ACCESS_KEY_ID}
            secretAccessKey: ${TAKO_BACKUP_SECRET_ACCESS_KEY}
```

`accessKeyId` and `secretAccessKey` are optional and must be set together.
When both are omitted, takod uses the AWS default credential chain on the node
that runs the backup: environment variables, shared config, then the EC2
instance profile. On EC2, prefer an instance role scoped to the backup bucket
over long-lived keys. The node needs `s3:PutObject`, `s3:GetObject`,
`s3:DeleteObject` and `s3:ListBucket`, plus `s3:GetBucketVersioning` for the
`s3` provider. Keyless storage requires takod v0.12.2 or newer.

`retain` is a number of days. It applies to scheduled backups and to on-demand
`tako backup --all` / `--volume` runs alike: after each backup, takod deletes
node-local archives for that volume older than `retain` days and, when
`storage` is configured, the matching remote objects. Volumes without a
`backup` block are never pruned automatically — if CI runs `tako backup --all`
before every deploy, give each persistent service a `backup.retain` (or run
`tako backup --cleanup <days>`) so node-local archives cannot fill the data
volume.

Native volume backups are live filesystem archives. They are useful for files
and application-managed snapshots, but they are not a transactionally
consistent PostgreSQL backup. Keep PostgreSQL backups as an explicit generic
job that runs `pg_dump --format=custom` and uploads the resulting object. Tako
does not inspect that job's command or reserve a job name.

Each new native archive is published atomically with a collision-resistant
artifact ID, exact size, and SHA-256 manifest. A remote copy is marked verified
only after Tako reads the uploaded object back and matches its size and digest.
For versioned buckets, set `backup.storage.versioned: true`. Tako then records
and deletes exact object version IDs, and storage credentials need version-list
and version-delete permissions plus bucket-versioning status access in addition
to object read/write permissions.
Leave it false for R2 and unversioned S3-compatible stores so retention uses the
widely supported object-list API.
Legacy node-local archives remain listable and restorable, while new manifests
are checked before destructive extraction. Production volume restore requires
explicit approval with `tako backup ... --restore <id> --yes`. If restore had
to create the target and then fails or is interrupted, Tako preserves both the
token-labelled target and transaction journal for explicit recovery; it never
deletes the named volume automatically.

When that fail-closed state prevents takod from starting, recover locally on
the affected node. Do not move or edit the journal. The command verifies the
recorded Docker volume fingerprint and ownership/creation token before acting:

```bash
sudo tako backup recover \
  --journal /var/lib/tako/backups/PROJECT/ENVIRONMENT/.restore-transaction-ID.json \
  --action inspect

# Pre-existing target only: restore the durable rollback archive.
sudo tako backup recover --journal /var/lib/tako/backups/PROJECT/ENVIRONMENT/.restore-transaction-ID.json --action rollback --yes

# Keep the current restored contents.
sudo tako backup recover --journal /var/lib/tako/backups/PROJECT/ENVIRONMENT/.restore-transaction-ID.json --action accept --yes

# Transaction-created target only: keep the volume for explicit inspection or removal.
sudo tako backup recover --journal /var/lib/tako/backups/PROJECT/ENVIRONMENT/.restore-transaction-ID.json --action abandon --yes
```

Successful recovery removes the guard container, staging volume, rollback
archive, and transaction marker durably. It never deletes the target volume.
After `abandon`, inspect and remove that preserved volume explicitly if it is
not needed.

Schema-v1 transaction journals predate target fingerprints and creation tokens,
so the recovery command deliberately refuses them. Keep takod stopped and:

1. Copy the entire `/var/lib/tako/backups/PROJECT/ENVIRONMENT` directory to
   separate storage before changing anything.
2. Read the journal and use `docker volume inspect TARGET` plus deployment
   inventory to independently confirm the intended target. If identity is
   ambiguous, preserve all evidence and stop.
3. For a pre-existing target in `mutating`, validate the rollback tar with
   `tar -tzf ROLLBACK` and restore it only after independent operator approval.
   Never restore into a target whose identity is uncertain.
4. For a transaction-created target, preserve it for inspection or explicit
   removal; never infer safety from its name alone.
5. After the target outcome is resolved, remove only the journal's uniquely
   named guard container and staging volume, retain or archive the rollback tar,
   remove the journal, and durably sync its parent directory. Restart takod only
   after a second operator verifies the retained evidence and chosen outcome.

Use the exact key, byte count, and lowercase SHA-256 emitted by your custom dump
job to perform a non-destructive remote restore proof:

```bash
tako backup verify-postgres \
  --server production \
  --service postgres \
  --artifact-id pgdump-20260729T020000Z \
  --object-key postgres/dumps/pgdump-20260729T020000Z.dump \
  --size 184320 \
  --sha256 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  --scratch-size 1073741824 \
  --postgres-image postgres:16-alpine@sha256:<64-lowercase-hex-digest> \
  --database app \
  --query "SELECT count(*) FROM schema_migrations" \
  --expected 42
```

The selected service supplies `backup.storage`; credentials should continue to
use `${ENV_VAR}` references. The target agent downloads exactly the named
object, verifies size and SHA-256 before Docker mutation, starts a hardened
PostgreSQL container with quota-bounded scratch storage and `--network none`, restores with
`pg_restore --exit-on-error --no-tablespaces`, and runs the query as a temporary
read-only role. The digest-pinned verifier image must provide PostgreSQL 14 or
newer. Only one verification runs at a time, and takod reserves the requested
scratch bytes against its disk-admission budget before downloading. It stores
only artifact and query hashes in the durable verification record, then removes
the container, volume, and downloaded dump on success or failure. The verifier
never accepts a production volume name.

The target takod must run with a non-zero `--minimum-free-disk-bytes`. The
agent reserves disk for the artifact and applies `--scratch-size` as the hard
size of a dedicated tmpfs mounted directly at `PGDATA`. The scratch request plus
512MiB of PostgreSQL overhead must fit in currently available host memory after
preserving takod's `--reserved-memory-bytes` budget (512MiB by default).

## Disk Admission

takod refuses disk-growing operations — deploy leases, service reconciles that
add containers, image imports and builds, backups, restores, and jobs — when the
affected filesystem has less free space than `--minimum-free-disk-bytes`
(10GiB by default; `0` disables admission). Refusals return HTTP 507 with the
path, free bytes, and the floor that was crossed.

Small control-plane writes — deployment state documents and events, proxy route
manifests, and proxy reconciliation — are admitted against a reduced floor of
`min(256MiB, --minimum-free-disk-bytes)`. A rollout that drops below the
platform minimum partway through (typically while pulling new images) can then
still publish its proxy routes and record its outcome, instead of leaving
services half-applied and the deployment record stuck in progress. New deploys
still need the full minimum because the deploy lease uses the platform floor.

## Docker Build Cache Pruning

Successful deploy cleanup and `tako cleanup --docker-cache` prune Docker
builder cache above the default `20GB` storage budget. Installed takod agents
also run a scheduled builder-cache prune every `24h`:

```bash
tako takod run --build-cache-prune-interval 24h --build-cache-keep-storage 20GB
```

Set `--build-cache-prune-interval 0` to disable the scheduled prune, or lower
`--build-cache-keep-storage` on small VPS disks.

On agents with `node.disk-hygiene-v1`, this prune is part of the daily disk
hygiene run, which also removes old Tako-built images beyond the newest 3 per
service and environment (`--image-retention`). `--hygiene-interval` sets the
run interval; when only `--build-cache-prune-interval` is set it keeps
controlling the interval. See
[Resource Monitoring](RESOURCE-MONITORING.md#disk-hygiene).

## Host Metrics Collection

takod samples host metrics natively and keeps a bounded on-node history for
`tako metrics --since`. These are `takod run` flags with defaults, so
`tako.yaml` needs no setting:

```bash
tako takod run --metrics-interval 60s --metrics-retention 168h --metrics-history-max-bytes 67108864
```

- `--metrics-interval` (default `60s`, minimum `5s`): `0` disables native
  collection and history; takod then serves the legacy agent file if present.
- `--metrics-retention` (default `168h`, `1h`–`744h`): history kept under
  `/var/lib/tako/metrics/history/`.
- `--metrics-history-max-bytes` (default 64 MiB): hard cap, oldest files first.
- `--metrics-textfile-dir` (off): write `tako_node.prom` atomically after each
  sample for node_exporter's textfile collector. takod never opens a network
  listener.

See [Resource Monitoring](RESOURCE-MONITORING.md).
