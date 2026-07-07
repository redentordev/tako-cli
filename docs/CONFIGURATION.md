# Configuration Guide

Recipes for common `tako.yaml` shapes. For the runtime, state, mesh, and CI
model behind them, see [ORCHESTRATION-MODEL.md](./ORCHESTRATION-MODEL.md).
Run `tako config explain -e <env>` to see the effective defaults Tako infers
for any config without copying them into your YAML.

## Simple Web Application

```yaml
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

```yaml
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

## Full-Stack Application

```yaml
services:
  web:
    build: ./frontend
    port: 3000
    proxy:
      domain: app.example.com

  api:
    build: ./backend
    port: 4000
    replicas: 2
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

```yaml
servers:
  node1:
    host: ${NODE1_HOST}
    user: root
    sshKey: ~/.ssh/id_ed25519
  node2:
    host: ${NODE2_HOST}
    user: root
    sshKey: ~/.ssh/id_ed25519

environments:
  production:
    servers:
      - node1
      - node2
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

## Background Workers

```yaml
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

## Stateful Services And Volumes

Containers are disposable; Docker volumes are the data boundary. Databases,
queues, CMS storage, and tools such as n8n should use prebuilt images with
`persistent: true` and at least one named or external volume:

```yaml
services:
  postgres:
    image: postgres:16-alpine
    port: 5432
    persistent: true
    volumes:
      - postgres_data:/var/lib/postgresql/data
    placement:
      strategy: pinned
      servers: [primary]
    env:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
      POSTGRES_DB: app
```

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

## Secrets Management

```yaml
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

## Domain Redirects (www → non-www)

Automatically redirect traffic from one domain to another with proper SSL and
path preservation:

```yaml
services:
  web:
    build: .
    port: 3000
    proxy:
      # Primary domain where traffic is served
      domain: example.com
      # These domains will 301 redirect to the primary domain
      redirectFrom:
        - www.example.com
        - old.example.com
      email: admin@example.com
```

- Automatic SSL certificates for all domains (primary + redirect domains)
- 301 permanent redirects for SEO
- Path preservation (`www.example.com/api/users` → `example.com/api/users`)
- Works with both HTTP and HTTPS traffic
- Wildcard domains such as `*.example.com` are rejected for now; the built-in
  proxy uses HTTP-01 certificate issuance and needs DNS-01 support before
  wildcard certificates can be automated.

Serve one service on several hostnames with `proxy.domains`, each with
automatic HTTPS.

### Domain Readiness Checks

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

```yaml
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

```yaml
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

```yaml
servers:
  edge-1:
    host: edge.example.com
    user: deploy
    labels:
      role: edge
  app-1:
    host: app.example.com
    user: deploy
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

## Parallel Deployment (Default)

Tako deploys services in parallel by default. Customize it:

```yaml
project:
  name: my-app
  version: 1.0.0

# Optional: Customize parallel deployment (these are the defaults)
deployment:
  strategy: parallel  # or "sequential"
  parallel:
    maxConcurrentBuilds: 4   # Max builds at once
    maxConcurrentDeploys: 4  # Max deploys at once
  cache:
    enabled: true     # Enable build caching
    type: local       # Cache type
  build:
    strategy: remote  # remote, local, or auto
```

## Build Strategy

Build-backed services use `deployment.build.strategy: remote` by default: Tako
streams the build context to each assigned server and builds there with takod.
For stronger developer or CI machines, use local build mode:

```yaml
deployment:
  build:
    strategy: auto # try local buildx/unregistry, fall back to remote takod build
```

`local` builds once per target architecture with `docker buildx build
--platform linux/amd64|linux/arm64 --load` and pushes the image to each
assigned server with psviderski/unregistry's `docker pussh`. `auto` uses the
same path when available and falls back to `remote` when Docker, buildx,
docker-pussh, SSH key/agent auth, or remote Docker permissions are not ready.

Use `remote` when the server is intentionally the build host, `local` when CI
or a developer workstation should build and push images to the VPS, and `auto`
for portable config that prefers local builds but preserves the older
server-build path.

Use a one-off override from CI or a dev machine:

```bash
tako deploy --build-strategy local
tako deploy --build-strategy auto
```

Install docker-pussh on the client machine:

```bash
brew install psviderski/tap/docker-pussh
mkdir -p ~/.docker/cli-plugins
ln -sf "$(brew --prefix)/bin/docker-pussh" ~/.docker/cli-plugins/docker-pussh
docker pussh --help
```

## Container Resource Limits

Set a Docker memory limit per service with `resources.memory`:

```yaml
services:
  web:
    build: .
    resources:
      memory: 512m
```

Tako passes this to Docker as `--memory`. Accepted units are Docker-style
byte, k, m, or g values such as `512m`, `1g`, or `768mb`.

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
            accessKeyId: ${TAKO_BACKUP_ACCESS_KEY_ID}
            secretAccessKey: ${TAKO_BACKUP_SECRET_ACCESS_KEY}
```

## Docker Build Cache Pruning

Successful deploy cleanup and `tako cleanup --docker-cache` prune Docker
builder cache above the default `20GB` storage budget. Installed takod agents
also run a scheduled builder-cache prune every `24h`:

```bash
tako takod run --build-cache-prune-interval 24h --build-cache-keep-storage 20GB
```

Set `--build-cache-prune-interval 0` to disable the scheduled prune, or lower
`--build-cache-keep-storage` on small VPS disks.
