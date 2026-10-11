# 🐙 Tako CLI

**タコ - Deploy containerized apps to your own VPS with one small takod mesh.**

## What is Tako?

**Tako** (タコ) is Japanese for "octopus" - pronounced "tah-koh".

Tako has one job: reconcile a Git-backed app config onto one or more servers
you own. A single server is a one-node mesh; adding more nodes keeps the same
commands, config shape, proxy model, and state workflow.

The CLI uses SSH for bootstrap and talks to a node-local `takod` agent.
`takod` owns the runtime reconciliation loop: Docker service state, tako-proxy
routes and certificates, WireGuard mesh state, remote operation leases, and
replicated deployment state.

## Why Tako?

- Use your own servers (DigitalOcean, Hetzner, AWS EC2, any VPS)
- Health-checked deployments (rolling by default for stateless services, recreate, blue-green) that restore the previous revision when a rollout fails
- Automatic HTTPS through tako-proxy, including DNS-01 and on-demand customer domains
- Git-clean deployments with full version history
- Meshed takod orchestration from one server to many
- App/stage isolation so unrelated projects can share a node
- Replicated state and remote leases so laptops and CI can deploy safely
- Encrypted env bundles and state pull/repair for switching computers
- Volume backups with schedules, retention, and S3-compatible off-node storage
- Machine interface (`--output json`, `--events ndjson`, stable exit codes)
  for control planes and automation

Docs: [Orchestration model](./docs/ORCHESTRATION-MODEL.md) ·
[Configuration guide](./docs/CONFIGURATION.md) ·
[Machine interface](./docs/MACHINE-INTERFACE.md) ·
[CI/CD](./docs/CI-CD.md) · [API/SDK](./docs/API-SDK.md) ·
[Server setup contract](./docs/SERVER-SETUP-VERSIONING.md) ·
[Resource monitoring](./docs/RESOURCE-MONITORING.md)

---

## Install

**Install script (Linux & macOS)** — downloads the latest release, verifies
its checksum, and installs man pages:

```bash
curl -fsSL https://raw.githubusercontent.com/redentordev/tako-cli/master/install.sh | bash
```

**Direct binary:**

```bash
curl -fL https://github.com/redentordev/tako-cli/releases/latest/download/tako-linux-amd64 -o /tmp/tako
sudo install -m 0755 /tmp/tako /usr/local/bin/tako
rm /tmp/tako
```

Release assets: `tako-linux-amd64`, `tako-linux-arm64`, `tako-darwin-amd64`,
`tako-darwin-arm64`, `tako-windows-amd64.exe`, plus `checksums.txt` and
`tako-manpages.tar.gz`.

**Homebrew (macOS & Linux):**

```bash
brew install redentordev/tako/tako
```

The Homebrew tap can lag behind GitHub releases; check `tako --version`
against the [latest release](https://github.com/redentordev/tako-cli/releases/latest).

**Container image (multi-arch, AMD64/ARM64):**

```bash
docker run --rm ghcr.io/redentordev/tako-cli:latest --version
```

For CI jobs that run Tako from the image, mount the project checkout, SSH key,
and Docker socket into the container.

<details>
<summary>Build from source, completions, man pages, upgrading</summary>

**Build from source** (Go 1.26.4+):

```bash
git clone https://github.com/redentordev/tako-cli.git
cd tako-cli
make build      # or: make install
```

**Shell completion** — copy the matching file from
[`completions/`](./completions) (`tako.bash`, `tako.zsh`, `tako.fish`) into
your shell's completion directory; see
[completions/README.md](./completions/README.md).

**Manual pages** — Homebrew and `install.sh` install them automatically. For
direct binary installs:

```bash
gh release download --repo redentordev/tako-cli --pattern tako-manpages.tar.gz
sudo mkdir -p /usr/local/share/man/man1
sudo tar -xzf tako-manpages.tar.gz -C /usr/local/share/man/man1
```

**Upgrading:**

```bash
tako upgrade --check                 # direct binary / install.sh installs
tako upgrade
tako upgrade servers -e production   # bring takod on your servers to this CLI version
brew upgrade redentordev/tako/tako   # Homebrew installs
```

`tako upgrade servers` is a no-op on nodes already running the target
version; pass `--force` to reinstall and restart takod anyway.

**Uninstalling:** `sudo rm -f /usr/local/bin/tako` (or run
[`uninstall.sh`](./uninstall.sh)).

</details>

---

## Quick Start (5 minutes)

1. **Initialize your project** — creates `tako.yaml`, `.env.example`, and
   `.gitignore`:

```bash
tako init my-app
cp .env.example .env
```

Edit `.env`:

```bash
TAKO_PRODUCTION_HOST=203.0.113.10     # Your VPS IP
TAKO_SSH_KEY=~/.ssh/id_ed25519        # SSH key path
LETSENCRYPT_EMAIL=admin@example.com   # For TLS certificates
```

2. **Review `tako.yaml`** — the generated template includes a working web
   service:

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
    servers: [production]
    services:
      web:
        build: .                                      # Build from current directory
        port: 3000                                    # Container port
        proxy:
          domain: my-app.${TAKO_PRODUCTION_HOST}.sslip.io # Auto-DNS with sslip.io
          email: ${LETSENCRYPT_EMAIL}
        healthCheck:
          path: /
        env:
          NODE_ENV: production
```

Tako infers the meshed `takod` runtime, replicated state, and WireGuard mesh
settings; run `tako validate` to check the file offline and
`tako config explain -e production` to see the effective defaults.

3. **Commit, set up the server (one-time), and deploy:**

```bash
git add . && git commit -m "Initial Tako deployment config"
tako setup -e production     # Docker, WireGuard, takod, tako-proxy, ufw (keeps your SSH port open), fail2ban, hardening
tako deploy -e production
```

Your app is now live with automatic HTTPS at
`https://my-app.YOUR-SERVER-IP.sslip.io`.

Build-backed services are tagged with the current Git commit hash, so
committing app changes is enough to produce a new deploy artifact. Targeted
deploy inputs let automation update one configured service without a full
rebuild: `--service web` with `--image <ref>`, `--source .`, or
`--archive app.tar.gz`. Use `--force` to intentionally reconcile unchanged
services (broad force skips `persistent: true` services unless targeted).

**Moving to another machine or CI:** push your `.env` and `.tako/secrets*`
files as a passphrase-encrypted bundle (Argon2id + AES-256-GCM) stored in takod
state, then pull them elsewhere. Set `TAKO_ENV_PASSPHRASE` for non-interactive
use; `--allow-missing` lets CI succeed when no bundle has been pushed yet.

```bash
tako env push -e production
tako env pull -e production --allow-missing
```

### Configless Deploys (`tako run`)

Deploy a public image to an existing VPS/takod node without a local
`tako.yaml`. `--server` takes the **SSH host or IP address** — there is no
config to resolve named servers against in this mode:

```bash
tako run nginx:1.27 --name web --port 80 --server 203.0.113.10 --user root
```

Tako derives a config server key from the host (`203.0.113.10` becomes
`ip-203-0-113-10`, `vps1.example.com` becomes `vps1-example-com`) and records
it in remote state; pass `--server-name` to choose it explicitly. `tako run`
synthesizes Tako desired state and still uses takod, labels, leases, history,
and proxy reconciliation. Private registry pulls work with
`--registry-user` + `--registry-password-stdin`. When updating an existing
service identity from automation, pass `--yes` to accept the update plan
noninteractively.

To materialize remote takod state into a local config afterwards — or from
another machine:

```bash
tako config export --project web --server 203.0.113.10 --user root -o tako.yaml
tako config pull --project web --server 203.0.113.10 --user root -o tako.yaml
```

Both read Tako-managed desired/actual/history state; they do not discover
arbitrary Docker containers. Generated config is best-effort: env values are
redacted placeholders, and `--password` is never written to the output.

### Platform Mode (first PaaS node)

For a PaaS installed on its first workload node, run `sudo tako platform inspect`
to check existing enrollment state, then `sudo tako platform init` on that
server, and attach workspaces with `tako project attach --cluster CLUSTER_ID`.
Worker enrollment uses expiring single-use join tokens and pinned SSH host
keys. See
[First PaaS Node With Local Deployments](docs/CONFIGURATION.md#first-paas-node-with-local-deployments).

---

## Features

**Deployment & operations** — reconciled deploys (zero-downtime rolling by
default for stateless services, recreate, blue-green with manual or automatic
promotion), restore-on-failure of the previous revision, graceful container
replacement, concurrent per-node rollout in dependency order, instant rollback, Git-based versioning,
replicated deployment state with remote operation leases, release hooks
(`release:` commands gate traffic), scheduled jobs (`kind: job` with cron, run
history, and manual trigger), remote exec in service containers, health checks,
drift detection, maintenance mode, service log rotation.

**Lifecycle hooks** — committable local scripts in `.tako-hooks/` (or argv
commands in `hooks.commands`) that run on the operator's machine or CI runner
before and after `deploy`, `rollback`, `promote`, and `setup`, plus
`on-failure`. A failing `pre-*` hook aborts before anything changes, hooks get
a minimal environment with `TAKO_*` context and opt-in `passEnv`, output is
redacted and streamed as `hook.*` events, and `.ps1`/`.cmd` variants run
natively on Windows ([hooks guide](./docs/HOOKS.md)).

**Backups** — volume backup/restore across the mesh, per-service `backup:`
schedules with `retain` days, off-node copies to S3, R2, or any S3-compatible
store (static keys via `${ENV_VAR}`, or keyless instance-role/default AWS
credentials with takod v0.12.2+), PostgreSQL backup verification
(`tako backup verify-postgres`), and interrupted-restore recovery.

**Proxy & domains** — automatic HTTPS via Let's Encrypt or ZeroSSL,
HTTP/1.1–HTTP/3 and WebSockets through the Caddy-backed shared tako-proxy,
DNS-01 issuance (Cloudflare, Hetzner, DigitalOcean) for wildcard and
CDN-fronted domains, dynamic customer domains via an ask endpoint, multiple
domains per service, www → non-www redirects with path preservation, route
policies (basic auth, IP allowlists), uploaded certificates (`tako certs`),
internal HTTP-only routes without public DNS.

**Servers & scaling** — multi-server takod mesh over WireGuard, placement
strategies (spread, pinned, global, label constraints), reviewable placement
plans, commit-tagged image reuse, one-command server setup (Docker, takod,
ufw firewall that keeps the configured SSH port open, fail2ban, validated SSH
hardening) and agent upgrades, shared nodes with
app/stage isolation, cross-project service exports/imports, raw TCP/UDP port
publishing.

**Security & automation** — SSH host-key verification (`tofu`, `strict`,
`ask`), secrets with automatic redaction and AWS SSM / Secrets Manager import,
encrypted env bundles, private registry auth via a `registries:` block with
`${ENV_VAR}` credentials, commands with the machine contract support
`--output json` (versioned result documents) and `--events ndjson` (typed
progress events) with stable exit codes, while a small human-only set
(such as `init` and `monitor`) rejects them
([machine interface and command coverage](./docs/MACHINE-INTERFACE.md#command-coverage));
single static binary for Linux/macOS/Windows.

---

## Commands

Every command has `--help` and a man page (`man tako-deploy`); regenerate man
pages with `make man` after flag changes.

| Command | Description |
|---------|-------------|
| `tako init` | Initialize new project with template config |
| `tako validate` | Validate `tako.yaml` without contacting servers |
| `tako config explain` | Show inferred runtime, state, mesh, server, and service defaults |
| `tako config export` / `tako config pull` | Materialize remote takod state into a local `tako.yaml` |
| `tako setup` | Set up or refresh a server: Docker, WireGuard, takod, proxy, firewall, hardening |
| `tako deploy` | Deploy configured application to environment |
| `tako run <image> --name <app> --port <p> --server <host-or-ip>` | Configless deploy of a public image to an existing node |
| `tako promote <service>` | Promote a warmed blue-green revision |
| `tako rollback [id]` | Roll back to previous/specific deployment |
| `tako history` | View deployment history |
| `tako ps` / `tako logs` / `tako access` | Service status, container logs, proxy access logs |
| `tako exec <service> -- <cmd>` | Run a command in a running service container |
| `tako start` / `tako stop` / `tako scale` | Service control |
| `tako maintenance` / `tako live` | Enable / disable maintenance mode for a service |
| `tako remove` / `tako destroy` | Remove services / the whole app stage (shared server setup is kept) |
| `tako jobs runs` / `tako jobs trigger` | Inspect and trigger scheduled jobs |
| `tako backup` | Volume backup, list, restore, delete, and retention cleanup; `verify-postgres`, `recover` |
| `tako env push` / `tako env pull` | Encrypted `.env` + secrets bundles stored in the takod mesh |
| `tako secrets` | Init, set, list, delete, validate, fetch, and import secrets |
| `tako certs push\|ls\|rm` | Manage uploaded TLS certificates on proxy nodes |
| `tako proxy hash-password` | Mint a bcrypt hash for `proxy.basicAuth` |
| `tako domains status` / `tako domains hosts` | Domain DNS/TLS readiness; hosts-file entries for internal routes |
| `tako doctor` | Diagnose config, SSH, agents, Docker, proxy, state, services, volumes |
| `tako drift` | Detect config drift between desired state and running services |
| `tako metrics` / `tako stats` / `tako monitor` / `tako prometheus` | System metrics, container stats, continuous monitoring, Prometheus export |
| `tako state pull\|status\|repair\|forget-node\|lease` | Sync, compare, repair state; manage remote leases |
| `tako placement plan cordon\|drain\|rebalance` | Create digest-bound, reviewable replica placement plans (`verify`, `apply`) |
| `tako discovery exports` | List exported cross-project services on reachable nodes |
| `tako clone-setup` | Set up a cloned project on a new machine |
| `tako platform` / `tako project attach` | Platform-mode node enrollment, backups, and workspace attachment |
| `tako upgrade` / `tako upgrade servers` | Upgrade the CLI / server-side takod agents |
| `tako cleanup` | Clean app/stage runtime resources and logs; `--docker-cache` prunes builder cache |

Common flags: `-v/--verbose`, `-e/--env <name>`, `--service <name>`,
`--config <path>`, `--host-key-mode <tofu|strict|ask>`.

CI/CD runners use the same takod path as a laptop — see
[CI/CD Deployments](./docs/CI-CD.md).

---

## Configuration

One config file describes servers, environments, and services:

```yaml
project:
  name: my-app
  version: 1.0.0

servers:
  node1:
    host: ${NODE1_HOST}
    user: root
    sshKey: ~/.ssh/id_ed25519

environments:
  production:
    servers: [node1]
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
        secrets:
          - JWT_SECRET
        env:
          DATABASE_URL: postgresql://database:5432/myapp

      database:
        image: postgres:16-alpine
        persistent: true
        volumes:
          - db_data:/var/lib/postgresql/data
        placement:
          strategy: pinned
          servers: [node1]
        backup:
          schedule: "0 2 * * *"   # nightly
          retain: 7               # days
          storage:
            provider: s3
            bucket: my-app-backups
            region: us-east-1     # omit keys to use the instance role
        env:
          POSTGRES_PASSWORD: ${DB_PASSWORD}
```

The [Configuration guide](./docs/CONFIGURATION.md) covers the full recipe set:
multi-server placement, background workers, stateful services and volumes,
secrets, domain redirects and readiness checks, DNS-01 and dynamic customer
domains, internal proxy routes, shared nodes and cross-project exports,
concurrent per-node rollout in dependency order, build strategies, resource limits, volume backups, and
build cache pruning.

---

## Architecture

```
┌─────────────────┐                    Tako CLI
│   Tako CLI      │                        |
│  (Local / CI)   │                  connect to any
└────────┬────────┘                        |
         │ SSH                   +---------+---------+
         ▼                       |         |         |
┌─────────────────┐           takod A   takod B   takod C
│   VPS Server    │              |         |         |
├─────────────────┤           Runtime   Runtime   Runtime
│ takod           │           Proxy     Proxy     Proxy
│ tako-proxy      │              +-- WireGuard mesh --+
│ Docker runtime  │
│ Your containers │
│ Replicated state│
└─────────────────┘
  Single server              Multi-server mesh
```

See [ORCHESTRATION-MODEL.md](./docs/ORCHESTRATION-MODEL.md) for the runtime,
state, mesh, and CI model.

---

## Examples

The [examples/](./examples) directory has ready-to-deploy projects: web
frameworks (Node.js, Next.js, Hono, SvelteKit, SolidStart, Astro, PHP,
Laravel, Rails), full-stack and monorepo setups, background workers, scaling
and placement, scheduled jobs, private registries, and third-party apps
(n8n, Plausible, Umami, Ghost, Sentry).

---

## Development

Requires Go 1.26.4+, Git, and a container runtime for local builds and tests.

```bash
git clone https://github.com/redentordev/tako-cli.git
cd tako-cli
go build ./...   # build all packages
go test ./...    # run tests
make build       # build the binary (make build-all for all platforms)
```

Layout: `cmd/` CLI commands · `pkg/` core packages (config, deployer, engine,
takod, ssh, provisioner, ...) · `internal/state/` deployment state ·
`examples/` example projects · `docs/` documentation.

---

## Support

- **Issues**: [GitHub Issues](https://github.com/redentordev/tako-cli/issues)
- **Releases**: [GitHub Releases](https://github.com/redentordev/tako-cli/releases)

## License

MIT License - see [LICENSE](./LICENSE) file for full text.

## Acknowledgments

Tako CLI is inspired by the excellent work of:

- [Kamal](https://kamal-deploy.org/) by DHH and 37signals
- [Dokku](https://dokku.com/) by Jeff Lindsay
- [Uncloud](https://github.com/psviderski/uncloud) by Pavel Sviderski
- The simplicity of Heroku's developer experience

---

**🐙 Built by [Redentor Valerio](https://github.com/redentordev) ([@redentor_dev](https://twitter.com/redentor_dev)) | Start deploying in minutes, not hours. Your servers, your rules.**
