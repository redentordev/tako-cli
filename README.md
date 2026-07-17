# 🐙 Tako CLI

**タコ - Deploy containerized apps to your own VPS with one small takod mesh.**

## What is Tako?

**Tako** (タコ) is Japanese for "octopus" - pronounced "tah-koh".

Tako has one job: reconcile a Git-backed app config onto one or more owned
servers. A single server is a one-node mesh; adding more nodes keeps the same
commands, config shape, proxy model, and state workflow.

The CLI uses SSH for bootstrap and talks to a node-local `takod` agent.
`takod` owns the runtime reconciliation loop: Docker service state, proxy
routes, WireGuard mesh state, remote leases, and replicated deployment state.

## Why Tako?

- Use your own servers (DigitalOcean, Hetzner, AWS EC2, any VPS)
- Health-checked deployments with recorded rollback state
- Automatic HTTPS certificates through tako-proxy
- Git-clean deployments with full version history
- Meshed takod orchestration from one server to many
- App/stage isolation so unrelated projects can share a node
- Remote leases for laptop and CI safety
- State pull/repair workflows for switching computers
- Machine interface (`--output json`, `--events ndjson`, stable exit codes)
  for control planes and automation

Docs: [Orchestration model](./docs/ORCHESTRATION-MODEL.md) ·
[Configuration guide](./docs/CONFIGURATION.md) ·
[Machine interface](./docs/MACHINE-INTERFACE.md) ·
[CI/CD](./docs/CI-CD.md) · [API/SDK](./docs/API-SDK.md)

---

## Install

**Homebrew (macOS & Linux):**

```bash
brew install redentordev/tako/tako
```

**Direct binary:**

```bash
curl -fL https://github.com/redentordev/tako-cli/releases/latest/download/tako-linux-amd64 -o /tmp/tako
sudo install -m 0755 /tmp/tako /usr/local/bin/tako
rm /tmp/tako
```

Release assets also cover `linux-arm64`, `darwin-amd64`, `darwin-arm64`, and
`windows-amd64.exe`.

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
gh release download vX.Y.Z --pattern tako-manpages.tar.gz
sudo mkdir -p /usr/local/share/man/man1
sudo tar -xzf tako-manpages.tar.gz -C /usr/local/share/man/man1
```

**Upgrading:**

```bash
brew upgrade redentordev/tako/tako   # Homebrew installs
tako upgrade --check                 # direct binary installs
tako upgrade
```

**Uninstalling:** `sudo rm -f /usr/local/bin/tako`

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
LETSENCRYPT_EMAIL=admin@example.com   # For SSL certificates
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
settings; run `tako config explain -e production` to see the effective
defaults.

For a PaaS installed on its first workload node, run `sudo tako platform init`
on that server and add the printed `clusterId`, `nodeId`, and `workerUid` to
the server entry with `transport: auto`. The same configuration then uses the
protected local worker ingress when Tako runs on that exact enrolled node and
identity-verified SSH from other machines. See
[First PaaS Node With Local Deployments](docs/CONFIGURATION.md#first-paas-node-with-local-deployments).

3. **Commit, set up the server (one-time), and deploy:**

```bash
git add . && git commit -m "Initial Tako deployment config"
tako setup -e production     # installs Docker, WireGuard, takod, firewall, hardening
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
redacted placeholders, and `--password` is never written to the output. For
multi-node state, `--server-name` must match a remote target node key.

---

## Features

**Deployment & operations** — reconciled deploys (recreate, rolling,
blue-green with manual or automatic promotion), parallel deployment,
instant rollback, Git-based versioning, replicated deployment state with
remote operation leases, release hooks (`release:` commands gate traffic),
scheduled jobs (`kind: job` with cron, run history, and manual trigger),
remote exec in service containers, health checks, drift detection, volume
backup/restore with optional S3-compatible off-node storage.

**Proxy & domains** — automatic HTTPS via Let's Encrypt, HTTP/1.1–HTTP/3 and
WebSockets through the Caddy-backed shared tako-proxy, multiple domains per
service, www → non-www redirects with path preservation, dynamic customer
domains via an ask endpoint, internal HTTP-only routes without public DNS.

**Servers & scaling** — multi-server takod mesh, placement strategies
(spread, pinned, global, label constraints), commit-tagged image reuse,
one-command server setup and agent upgrades (`tako setup`,
`tako upgrade servers`), shared nodes with app/stage isolation, cross-project
service exports/imports.

**Automation** — every command supports `--output json` (versioned result
documents), `--events ndjson` (typed progress events), and stable exit codes
([machine interface](./docs/MACHINE-INTERFACE.md)); private registry auth via
a `registries:` block with `${ENV_VAR}` credentials; secrets management with
automatic redaction; single static binary for Linux/macOS/Windows.

---

## Commands

The most-used commands. Every command has a man page (`man tako-deploy`) and
`--help`; regenerate man pages with `make man` after flag changes.

| Command | Description |
|---------|-------------|
| `tako init` | Initialize new project with template config |
| `tako validate` | Validate config locally before Git, SSH, build, or deploy work |
| `tako config explain` | Show inferred runtime, state, mesh, server, and service defaults |
| `tako setup` | Set up or refresh a server: Docker, WireGuard, takod, firewall, hardening |
| `tako deploy` | Deploy configured application to environment |
| `tako run <image> --name <app> --port <p> --server <host-or-ip>` | Configless deploy of a public image to an existing node |
| `tako config export` / `tako config pull` | Materialize remote takod state into a local `tako.yaml` |
| `tako promote <service>` | Promote a warmed manual blue-green revision |
| `tako rollback [id]` | Rollback to previous/specific deployment |
| `tako ps` / `tako logs` / `tako access` | Service status, container logs, proxy access logs |
| `tako exec <service> -- <cmd>` | Run a command in a running service container |
| `tako jobs runs` / `tako jobs trigger` | Inspect and trigger scheduled jobs |
| `tako doctor` | Diagnose config, SSH, agents, Docker, proxy, state, services, volumes |
| `tako metrics` / `tako monitor` | System metrics and continuous service monitoring |
| `tako history` | View deployment history |
| `tako start` / `tako stop` / `tako scale` | Service control |
| `tako backup` | Volume backup, list, restore, and retention cleanup |
| `tako drift` | Detect config drift between desired state and running services |
| `tako secrets` | Init, set, list, delete, and validate secrets |
| `tako state pull/status/repair/lease` | Sync, compare, repair state; manage remote leases |
| `tako domains status` / `tako domains hosts` | Domain DNS/TLS readiness; hosts-file entries for internal routes |
| `tako discovery exports` | List exported cross-project services on reachable nodes |
| `tako upgrade` / `tako upgrade servers` | Upgrade the CLI / server-side takod agents |
| `tako cleanup` | Clean app/stage node resources; `--docker-cache` prunes builder cache |
| `tako destroy` | Remove this app/stage services while preserving shared server setup |

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
        env:
          POSTGRES_PASSWORD: ${DB_PASSWORD}
```

The [Configuration guide](./docs/CONFIGURATION.md) covers the full recipe set:
multi-server placement, background workers, stateful services and volumes,
secrets, domain redirects and readiness checks, dynamic customer domains,
internal proxy routes, shared nodes and cross-project exports, parallel
deployment, build strategies, resource limits, volume backups, and build
cache pruning.

---

## Architecture

```
┌─────────────────┐                    Tako CLI
│   Tako CLI      │                        |
│  (Local)        │                  connect to any
└────────┬────────┘                        |
         │ SSH                   +---------+---------+
         ▼                       |         |         |
┌─────────────────┐           takod A   takod B   takod C
│   VPS Server    │              |         |         |
├─────────────────┤           Runtime   Runtime   Runtime
│ takod           │           Proxy     Proxy     Proxy
│ Local Proxy     │              +---- private mesh ----+
│ Container Runtime │
│ Your Containers │
│ State Cache     │
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
(n8n, Plausible, Umami, Ghost). Each example includes complete documentation.

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

## Support & Community

- **Issues**: [GitHub Issues](https://github.com/redentordev/tako-cli/issues)
- **Discussions**: [GitHub Discussions](https://github.com/redentordev/tako-cli/discussions)

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
