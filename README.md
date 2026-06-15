# 🐙 Tako CLI

**タコ - Deploy containerized apps to your own VPS with one small takod mesh.**

## What is Tako?

**Tako** (タコ) is Japanese for "octopus" - pronounced "tah-koh".

Tako has one job: reconcile a Git-backed app config onto one or more owned
servers. A single server is a one-node mesh; adding more nodes keeps the same
commands, config shape, proxy model, and state workflow.

The CLI uses SSH for bootstrap and talks to a node-local `takod` agent for
runtime work. `takod` owns Docker reconciliation, proxy config, WireGuard mesh
state, remote leases, and replicated deployment state.

[![Version](https://img.shields.io/badge/version-0.2.2-blue)](https://github.com/redentordev/tako-cli/releases)
[![Go Version](https://img.shields.io/badge/go-%3E%3D1.21-blue)](https://golang.org/)
[![License](https://img.shields.io/badge/license-MIT-green)](./LICENSE)
[![Runtime](https://img.shields.io/badge/runtime-takod_mesh-blue)](https://github.com/redentordev/tako-cli)

---

## Why Tako CLI?

Tako keeps deployment boring: one config, one CLI, one runtime path.

### Key Benefits

- Use your own servers (DigitalOcean, Hetzner, AWS EC2, any VPS)
- Health-checked deployments with recorded rollback state
- Automatic HTTPS certificates through tako-proxy
- Git-clean deployments with full version history
- Meshed takod orchestration from one server to many
- App/stage isolation so unrelated projects can share a node
- Remote leases for laptop and CI safety
- State pull/repair workflows for switching computers

See [docs/ORCHESTRATION-MODEL.md](./docs/ORCHESTRATION-MODEL.md) for the
runtime, state, mesh, and CI model.

---

## Quick Start

### Installation

#### Recommended: Direct Binary Install

Download the release binary for your platform and install it onto your PATH:

```bash
curl -fL https://github.com/redentordev/tako-cli/releases/latest/download/tako-linux-amd64 -o /tmp/tako
sudo install -m 0755 /tmp/tako /usr/local/bin/tako
rm /tmp/tako
```

Use the manual section below for Linux ARM64, macOS, and Windows binaries.

<details>
<summary>📦 Homebrew (macOS & Linux)</summary>

Install directly from the tap:

```bash
brew install redentordev/tako/tako
```

Or tap it first:

```bash
brew tap redentordev/tako
brew trust redentordev/tako # if Homebrew asks you to trust the tap
brew install tako
```

Benefits:
- Automatic updates with `brew upgrade`
- Works on macOS and Linux
- Installs the current release binary from GitHub Releases

</details>

<details>
<summary>🔧 Manual Installation</summary>

**Linux (AMD64):**
```bash
curl -L https://github.com/redentordev/tako-cli/releases/latest/download/tako-linux-amd64 -o /usr/local/bin/tako
chmod +x /usr/local/bin/tako
```

**Linux (ARM64):**
```bash
curl -L https://github.com/redentordev/tako-cli/releases/latest/download/tako-linux-arm64 -o /usr/local/bin/tako
chmod +x /usr/local/bin/tako
```

**macOS (Intel):**
```bash
curl -L https://github.com/redentordev/tako-cli/releases/latest/download/tako-darwin-amd64 -o /usr/local/bin/tako
chmod +x /usr/local/bin/tako
```

**macOS (Apple Silicon):**
```bash
curl -L https://github.com/redentordev/tako-cli/releases/latest/download/tako-darwin-arm64 -o /usr/local/bin/tako
chmod +x /usr/local/bin/tako
```

**Windows (PowerShell):**
```powershell
Invoke-WebRequest -Uri "https://github.com/redentordev/tako-cli/releases/latest/download/tako-windows-amd64.exe" -OutFile "tako.exe"
# Add to your PATH
```

</details>

<details>
<summary>🛠️ Build from Source</summary>

Requires Go 1.21+

```bash
git clone https://github.com/redentordev/tako-cli.git
cd tako-cli
make build

# Or install directly
make install
```

</details>

#### Verify Installation

```bash
tako --version
# Output:
# Tako CLI v0.2.2
# Commit:  abc1234
# Built:   2025-01-01T12:00:00Z
```

#### Shell Completion (Optional)

Enable tab completion for faster workflows:

```bash
# Bash
sudo cp completions/tako.bash /etc/bash_completion.d/tako

# Zsh
mkdir -p ~/.zsh/completion
cp completions/tako.zsh ~/.zsh/completion/_tako

# Fish
mkdir -p ~/.config/fish/completions
cp completions/tako.fish ~/.config/fish/completions/
```

See [completions/README.md](./completions/README.md) for detailed instructions.

#### Uninstalling

```bash
sudo rm -f /usr/local/bin/tako
# Remove any PATH entries from ~/.bashrc, ~/.zshrc, etc.
```

### Deploy Your First App (5 minutes)

1. **Initialize your project:**

```bash
tako init my-app
```

This creates:
- `tako.yaml` - Deployment configuration with comprehensive examples
- `.env.example` - Environment variables template
- `.gitignore` - Git ignore rules

2. **Configure your environment:**

Copy `.env.example` to `.env` and set your values:

```bash
cp .env.example .env
```

Edit `.env`:
```bash
SERVER_HOST=203.0.113.10              # Your VPS IP
LETSENCRYPT_EMAIL=admin@example.com   # For SSL certificates
```

3. **Review and customize `tako.yaml`:**

The generated `tako.yaml` includes a working web service example:

```yaml
project:
  name: my-app
  version: 1.0.0

servers:
  production:
    host: ${SERVER_HOST}
    user: root
    sshKey: ~/.ssh/id_ed25519

environments:
  production:
    servers: [production]
    services:
      web:
        build: .                                      # Build from current directory
        # dockerfile: Dockerfile.prod                 # Optional Dockerfile path inside build context
        port: 3000                                    # Container port
        proxy:
          domain: my-app.${SERVER_HOST}.sslip.io         # Auto-DNS with sslip.io
          email: ${LETSENCRYPT_EMAIL}
        env:
          NODE_ENV: production
```

The template includes commented examples for:
- Database services (PostgreSQL, Redis)
- Background workers
- Health checks
- Secrets management
- Multi-server deployments
- And more!

4. **Commit your app and Tako config:**

```bash
git add .
git commit -m "Initial Tako deployment config"
```

5. **Setup your server (one-time):**

```bash
tako setup -e production
```

This installs Docker, WireGuard, the node-local `takod` runtime, firewall rules,
monitoring, and security hardening. Released CLI builds also refresh the
server-side `/usr/local/bin/tako` binary used by `takod` during setup, deploy,
scale, and rollback so the node agent keeps pace with the CLI.

6. **Deploy your application:**

```bash
tako deploy -e production
```

Your app is now live with automatic HTTPS at `https://my-app.YOUR-SERVER-IP.sslip.io`!

---

## Features

### Deployment & Operations

- **Reconciled Deployments** - Recreate containers to match the desired takod state with health checks
- **Parallel Deployment** - Deploy multiple services concurrently (default behavior)
- **Instant Rollback** - Revert to any previous deployment with one command
- **Git-Based Versioning** - Every deployment tied to a Git commit
- **State Management** - Deployment history tracked on the server with local sync for new machines
- **Remote Lease** - CI, laptops, and mutating operations share remote operation locks
- **Automatic HTTPS** - tako-proxy provisions SSL certificates via Let's Encrypt
- **Domain Redirects** - Automatic www → non-www (or vice versa) with path preservation
- **Health Checks** - Ensure containers are healthy after reconciliation
- **Secrets Management** - Secure handling of environment secrets with automatic redaction
- **Drift Detection** - Detect configuration drift with `tako drift`

### Servers & Scaling

- **Multi-Server** - Deploy across multiple servers with takod mesh placement
- **Server Setup** - Configure existing VPS hosts with Docker, local proxy, firewall rules, and monitoring
- **Placement Strategies** - Control where services run (spread, pinned, global, label constraints)

### Developer Experience

- **Simple YAML Configuration** - Intuitive and readable
- **Environment Variables** - Full support with .env files
- **Local Development Workflow** - Pull remote state, use `.env` files, and deploy through the same takod path
- **Verbose Logging** - Detailed output for debugging
- **Cross-Platform** - Single binary for Windows, macOS, Linux
- **No Dependencies** - Just the binary and SSH access

---

## Core Commands

### Deployment & Management

| Command | Description |
|---------|-------------|
| `tako init` | Initialize new project with template config |
| `tako setup` | Set up or refresh an existing server with Docker, WireGuard, takod, firewall, and security hardening |
| `tako deploy` | Deploy application to environment |
| `tako rollback [id]` | Rollback to previous/specific deployment |
| `tako destroy` | Remove all services from server |

### Operations & Monitoring

| Command | Description |
|---------|-------------|
| `tako ps` | List running services, replica counts, and health status |
| `tako inspect [SERVICE]` | Inspect app-owned containers on takod nodes |
| `tako logs` | Stream container logs |
| `tako metrics` | View system metrics from servers |
| `tako history` | View deployment history |
| `tako image ls` | List app-owned Docker images on takod nodes |
| `tako image prune --force` | Prune unused app-owned Docker images while keeping images used by app containers |
| `tako volume ls` | List app-owned Docker volumes on takod nodes |

### Service Control

| Command | Description |
|---------|-------------|
| `tako scale` | Scale service replicas |
| `tako exec SERVICE [COMMAND...]` | Run a command inside a service replica |
| `tako run SERVICE [COMMAND...] --one-off` | Run a one-off task with service env and mounts |

### Backup & Recovery

| Command | Description |
|---------|-------------|
| `tako volume backup SERVICE VOLUME` | Back up a service Docker volume on nodes where it exists |
| `tako volume backups [SERVICE] [VOLUME]` | List volume backups |
| `tako volume restore SERVICE VOLUME BACKUP_ID --force` | Restore a service volume backup |
| `tako volume backup delete SERVICE VOLUME BACKUP_ID --force` | Delete a stored volume backup |
| `tako drift` | Detect configuration drift between config and running services |
| `tako drift --watch` | Continuously monitor for drift |

### Secrets Management

| Command | Description |
|---------|-------------|
| `tako env push [environment] --from-file .env.production` | Encrypt and upload an env file bundle to takod state |
| `tako env pull [environment] --force` | Restore the newest reachable encrypted env bundle |
| `tako secrets init` | Initialize secrets storage for project |
| `tako secrets set <KEY>=<value>` | Set a secret value |
| `tako secrets list` | List all secrets (redacted) |
| `tako secrets delete <KEY>` | Delete a secret |
| `tako secrets validate` | Validate all required secrets are set |

### Development & Utilities

| Command | Description |
|---------|-------------|
| `tako state pull` | Sync remote deployment state into local `.tako/` |
| `tako state status` | Compare local/remote state and show the remote lease |
| `tako state repair` | Repair deployment and runtime state across reachable mesh nodes |
| `tako state lease` | Show remote operation leases across reachable nodes |
| `tako state lease release --id <id> --force` | Release an exact stale remote lease |

CI/CD runners use the same takod path as a laptop. See
[CI/CD Deployments](./docs/CI-CD.md) and the
[meshed takod E2E checklist](./docs/MESH-E2E-CHECKLIST.md).

### Common Flags

- `-v, --verbose` - Show detailed output
- `-e, --env <name>` - Target specific environment
- `--service <name>` - Target specific service
- `--config <path>` - Use custom config file

---

## Configuration Examples

### Simple Web Application

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

### Existing VPS Deployment

```yaml
servers:
  production:
    host: ${SERVER_HOST}
    user: root
    sshKey: ~/.ssh/id_ed25519

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

### Multi-Server Deployment

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

### Full-Stack Application

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
    volumes:
      - db_data:/var/lib/postgresql/data
    env:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
```

### Multi-Server with takod Mesh

```yaml
servers:
  server1:
    host: ${SERVER1_HOST}
  server2:
    host: ${SERVER2_HOST}

environments:
  production:
    servers: [server1, server2]
    services:
      web:
        build: .
        port: 3000
        replicas: 4
        placement:
          strategy: spread  # Distribute across matching servers
          constraints:
            - node.labels.role==web
```

### Background Workers

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
    volumes:
      - redis_data:/data
```

### Storage Model

Tako treats Docker volumes as node-local by default. This keeps a one-node setup
and a multi-node mesh on the same operational path: each service can keep
persistent data on the node where it runs, and stateful services should use
`pinned` placement unless they are designed for multi-writer operation.

For data that must be shared across nodes, use an external storage service or an
application-level replication system such as a managed database, object storage,
or purpose-built clustered datastore. Tako does not provision shared filesystem
storage.

Volume backups are node-local too. `tako volume backup SERVICE VOLUME` resolves
the service volume from `tako.yaml`, then creates a backup only on nodes where
that Docker volume exists. `tako volume restore SERVICE VOLUME BACKUP_ID
--force` restores only on nodes that hold that backup. Custom Docker volume
names are allowed only when the volume has Tako app/stage ownership labels.

### Config Files

Use managed config files for non-secret runtime config such as a Caddyfile.
Tako uploads the local file to each target `takod`, stores it under a
project/stage-scoped path, and mounts it read-only into the container.

```yaml
configs:
  caddyfile:
    source: ./ops/caddy/Caddyfile

services:
  edge:
    image: caddy:2.9-alpine
    configs:
      - source: caddyfile
        target: /etc/caddy/Caddyfile
        mode: "0444"
```

Config content changes are part of reconciliation. Do not put credentials in
config files; use `env`, `envFile`, or Tako secrets for sensitive values.

Config artifacts can also be generated. The first supported generator renders
a Caddyfile from cross-project imports before the deployment plan is computed:

```yaml
imports:
  app_admin:
    project: app
    environment: production
    service: admin
    port: web
    servers:
      - app-node
  app_renderer:
    project: app
    environment: production
    service: renderer
    port: web
    servers:
      - app-node

configs:
  caddyfile:
    generate:
      caddy:
        email: ops@example.com
        adminHost: admin.example.com
        siteHost: sites.example.com
        adminImport: app_admin
        rendererImport: app_renderer
        askImport: app_admin
        askPath: /api/platform/domains/ask
        onDemandTLS: true
```

Generated config content is hashed after import resolution, so changed healthy
upstreams trigger reconciliation for services that mount the generated file.

### Cross-Project Exports

Services stay private unless they explicitly export named ports. Edge or
consumer projects declare imports at the project level:

```yaml
# app project
services:
  renderer:
    image: ghcr.io/acme/renderer:latest
    port: 3000
    export:
      ports:
        web: 3000

# edge project
imports:
  app_renderer:
    project: app
    environment: production
    service: renderer
    port: web
    servers:
      - app-node
```

Use `tako discovery --import app_renderer` from the edge project to resolve the
exported target from remote desired state and show live healthy endpoints. For
edge config workflows, `--format upstreams` prints a space-separated list of
HTTP upstream URLs suitable for manual Caddy environment placeholders:

```sh
export APP_RENDERER_UPSTREAMS="$(tako discovery --import app_renderer --format upstreams | tr -d '\n')"
```

For dedicated Caddy edge services, prefer generated config artifacts so the
deploy command resolves imports, renders the Caddyfile, hashes it, uploads it,
and reconciles the edge container in one run.

### Shared Nodes

Unrelated projects can deploy to the same server. Treat `project.name` as the
app name and the environment as the stage; that app/stage pair scopes state,
leases, env bundles, Docker labels, networks, containers, proxy files, and
generated volume names. The `tako-proxy` container is shared per node for ports
80 and 443, while each app/stage owns its own dynamic routes.

Services that bind host ports directly are reserved through `takod` before
containers are reconciled. If a service tries to bind public `80` or `443` on a
node where shared `tako-proxy` already owns those ports, deployment fails with
dedicated-edge guidance instead of replacing shared ingress for unrelated
projects.

For a node intentionally dedicated to a project-owned edge service, run
`tako setup --dedicated-edge`. This disables shared `tako-proxy` only when the
node has no active Tako proxy route files.

### Secrets Management

Use env bundles when a deployment needs to move `.env` files between a laptop,
a fresh checkout, and CI without committing them. Bundles are encrypted locally
with `TAKO_ENV_PASSPHRASE` or an interactive passphrase, then stored in takod
state on reachable environment nodes.

```bash
# Seed production env from an SSM/CapRover export or local stage file
TAKO_ENV_PASSPHRASE='correct horse battery staple' \
  tako env push production --from-file .env.production

# Restore on another machine or CI runner
TAKO_ENV_PASSPHRASE='correct horse battery staple' \
  tako env pull production --force
```

Files named `.env` and `.env.<stage>` restore with that basename. Other
`--from-file` source names restore as `.env`. `.tako/secrets*` files are bundled
alongside env files; `tako secrets list` remains redacted and env bundle
commands never print secret values.

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

**Usage:**
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

### Domain Redirects (www → non-www)

Automatically redirect traffic from one domain to another (e.g., www to non-www) with proper SSL and path preservation:

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

**Features:**
- Automatic SSL certificates for all domains (primary + redirect domains)
- 301 permanent redirects for SEO
- Path preservation (`www.example.com/api/users` → `example.com/api/users`)
- Works with both HTTP and HTTPS traffic

### Parallel Deployment (Default)

Tako CLI deploys services in parallel by default for faster deployments. You can customize this behavior:

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
    type: local       # local or registry
    # ref: ghcr.io/acme/my-app/buildcache  # required for type: registry
    # builder: mesh-builder                # optional docker buildx builder
```

**Benefits:**
- Faster deployments for multi-service apps
- Dependency-aware scheduling
- Local build cache reuse from existing node-local images
- Optional registry cache through Docker buildx
- Concurrent builds and deploys

---

## Architecture

### Single-Server Mode

```
┌─────────────────┐
│   Tako CLI      │
│  (Local)        │
└────────┬────────┘
         │ SSH
         ▼
┌─────────────────┐
│   VPS Server    │
├─────────────────┤
│ takod           │
│ Local Proxy     │
│ Container Runtime │
│ Your Containers │
│ State Cache     │
└─────────────────┘
```

### Multi-Server Mesh Mode

```
            Tako CLI
                |
          connect to any
                |
      +---------+---------+
      |         |         |
   takod A   takod B   takod C
      |         |         |
   Runtime   Runtime   Runtime
   Proxy     Proxy     Proxy
      +---- private mesh ----+
```

---

## Examples

Check out the [examples/](./examples) directory for ready-to-deploy projects:

### Web Frameworks & Applications
- **01-simple-web** - Basic Node.js web application
- **02-web-database** - Web app with PostgreSQL
- **03-fullstack** - Frontend + backend + database
- **04-monorepo** - Multiple services in one repo
- **09-nextjs-todos** - Next.js with SQLite
- **12-hono** - Hono (ultra-fast Edge framework)
- **13-sveltekit** - SvelteKit (full-stack Svelte)
- **15-astro** - Astro (content-driven framework)
- **16-php** - Vanilla PHP 8.3 application
- **17-laravel** - Laravel (PHP framework)
- **18-rails** - Ruby on Rails application

### Scaling & Infrastructure
- **05-workers** - Background job processing
- **06-scaling** - Multi-replica deployment
- **07-backend-api** - RESTful API service
- **08-frontend-consumer** - Frontend consuming external API

### Third-Party Applications
- **17-n8n** - n8n (workflow automation)
- **18-plausible** - Plausible (web analytics)
- **19-umami** - Umami (web analytics)
- **20-ghost** - Ghost (headless CMS)

### Testing & Advanced
- **test-parallel** - Parallel deployment testing
- **test-placement-strategies** - Placement strategy testing
- **test-secrets** - Secrets management

Each example includes complete documentation and is ready to deploy.

---

## Development

### Prerequisites

- Go 1.21 or higher
- Git
- Container runtime such as Docker (for local builds and tests)
- Make (optional)

### Building from Source

```bash
# Clone repository
git clone https://github.com/redentordev/tako-cli.git
cd tako-cli

# Install dependencies
go mod download

# Build binary
go build -o tako .

# Or use Makefile
make build

# Build for all platforms
make build-all
```

### Project Structure

```
tako-cli/
├── cmd/                  # CLI commands
│   ├── deploy.go         # Deployment logic
│   ├── setup.go          # Server setup
│   ├── rollback.go       # Rollback functionality
│   └── ...               # Other commands
├── pkg/                  # Reusable packages
│   ├── config/           # Configuration management
│   ├── deployer/         # Core deployment engine
│   ├── git/              # Git operations
│   ├── ssh/              # SSH client with pooling
│   ├── provisioner/      # Server setup
│   └── ...               # Other packages
├── internal/             # Internal packages
│   └── state/            # Deployment state management
├── examples/             # Example projects and deployment templates
├── docs/                 # Documentation
└── Makefile              # Build automation
```

---

## Support & Community

- **Issues**: [GitHub Issues](https://github.com/redentordev/tako-cli/issues)
- **Discussions**: [GitHub Discussions](https://github.com/redentordev/tako-cli/discussions)

---

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
