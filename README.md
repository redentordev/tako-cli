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

#### Recommended: Homebrew (macOS & Linux)

```bash
brew install redentordev/tako/tako
```

Or tap first:

```bash
brew tap redentordev/tako
brew install tako
```

#### Direct Binary Install

Download the release binary for your platform and install it onto your PATH:

```bash
curl -fL https://github.com/redentordev/tako-cli/releases/latest/download/tako-linux-amd64 -o /tmp/tako
sudo install -m 0755 /tmp/tako /usr/local/bin/tako
rm /tmp/tako
```

Use the manual section below for Linux ARM64, macOS, and Windows binaries.

#### Container Image

Each release also publishes a multi-arch Linux image for AMD64 and ARM64:

```bash
docker run --rm ghcr.io/redentordev/tako-cli:latest --version
```

For CI jobs that run Tako from the image, mount the project checkout, SSH key,
and Docker socket into the container so builds and remote deploys use the same
Git-backed config as the host runner.

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

Requires Go 1.26.4 or newer.

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
# Tako CLI vX.Y.Z
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

#### Upgrading

```bash
# Homebrew installs
brew upgrade redentordev/tako/tako

# Direct binary installs
# Check for updates
tako upgrade --check

# Upgrade to latest version
tako upgrade
```

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
- **Modern HTTP Proxying** - HTTP/1.1, HTTP/2, HTTP/3, and WebSocket traffic route through the Traefik-backed tako-proxy
- **Agent Upgrades** - Patch stale server-side takod agents with `tako upgrade servers`
- **Domain Redirects** - Automatic www → non-www (or vice versa) with path preservation
- **Health Checks** - Ensure containers are healthy after reconciliation
- **Secrets Management** - Secure handling of environment secrets with automatic redaction
- **Volume Backup/Restore** - Backup and restore service volumes with `tako backup`
- **Drift Detection** - Detect configuration drift with `tako drift`

### Servers & Scaling

- **Multi-Server** - Deploy across multiple servers with takod mesh placement
- **Build Image Streaming** - Broker locally built images between node-local takod agents without node-to-node SSH keys
- **Server Setup** - Configure existing VPS hosts with Docker, local proxy, firewall rules, and monitoring
- **Placement Strategies** - Control where services run (spread, pinned, global, label constraints)

### Developer Experience

- **Simple YAML Configuration** - Intuitive and readable
- **Environment Variables** - Full support with .env files
- **Local Development Workflow** - Pull remote state, use `.env` files, and deploy through the same takod path
- **Auto-Update** - Built-in upgrade mechanism with `tako upgrade`
- **Verbose Logging** - Detailed output for debugging
- **Cross-Platform** - Single binary for Windows, macOS, Linux
- **No Dependencies** - Just the binary and SSH access

---

## Core Commands

### Deployment & Management

| Command | Description |
|---------|-------------|
| `tako init` | Initialize new project with template config |
| `tako validate` | Validate config locally before Git, SSH, build, or deploy work |
| `tako setup` | Set up or refresh an existing server with Docker, WireGuard, takod, firewall, and security hardening |
| `tako deploy` | Deploy application to environment |
| `tako rollback [id]` | Rollback to previous/specific deployment |
| `tako destroy` | Remove all services from server |

### Operations & Monitoring

| Command | Description |
|---------|-------------|
| `tako ps` | List running services and their status |
| `tako logs` | Stream container logs |
| `tako access` | Stream proxy access logs |
| `tako metrics` | View system metrics from servers |
| `tako monitor` | Continuously monitor deployed services |
| `tako history` | View deployment history |

### Service Control

| Command | Description |
|---------|-------------|
| `tako start` | Start stopped services (scales to configured replicas) |
| `tako stop` | Stop running services (scales to 0) |
| `tako scale` | Scale service replicas |

### Backup & Recovery

| Command | Description |
|---------|-------------|
| `tako backup --volume <name>` | Backup a service volume across environment nodes |
| `tako backup --list` | List available backups across environment nodes |
| `tako backup --server <node> --volume <name> --restore <id>` | Restore a node-local volume from backup |
| `tako backup --cleanup <days>` | Delete backups older than N days across environment nodes |
| `tako drift` | Detect configuration drift between config and running services |
| `tako drift --watch` | Continuously monitor for drift |

### Secrets Management

| Command | Description |
|---------|-------------|
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
| `tako upgrade` | Upgrade Tako CLI to the latest version |
| `tako upgrade servers` | Upgrade and verify server-side takod agents to this CLI version |
| `tako live` | Disable maintenance mode and restore service traffic |
| `tako cleanup` | Clean up old node runtime resources |

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

### Shared Nodes

Unrelated projects can deploy to the same server. Treat `project.name` as the
app name and the environment as the stage; that app/stage pair scopes state,
leases, env bundles, Docker labels, networks, containers, proxy files, and
generated volume names. The Traefik-backed `tako-proxy` container is shared per
node for HTTP on port 80, HTTPS on TCP 443, and HTTP/3 on UDP 443, while each
app/stage owns its own dynamic routes. Proxy upstreams target deterministic
project/stage-scoped container aliases instead of generic service names like
`web`, so unrelated projects can safely use the same service names on the same
node. By default every selected environment node with public routes reconciles
the shared proxy for that app/stage. Built-in ACME TLS currently requires the
proxy placement to resolve to one node, because distributed certificate
issuance/storage is not implemented yet. To keep public ingress on a dedicated
edge node, set `environment.proxy.placement` with a pinned server or node-label
constraint:

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

Services can opt into cross-project access with `export: true`. Tako attaches
each exported service to its own export network, so importing another project
does not expose that project's private services. Consumers declare
`imports: [other-app.api]` and call the service through the readable DNS alias
`other-app-production-api` in the same environment. Export networks carry
`tako.discovery=export` labels with app, environment, service, and alias
metadata for future service-discovery tooling.

### Secrets Management

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
    type: local       # Cache type
```

**Benefits:**
- Faster deployments for multi-service apps
- Dependency-aware scheduling
- Automatic build caching
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
- **14-solidstart** - SolidStart (fine-grained reactivity)
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
├── cmd/                  # CLI commands (23 commands)
│   ├── deploy.go         # Deployment logic
│   ├── setup.go          # Server setup
│   ├── rollback.go       # Rollback functionality
│   ├── access.go         # Proxy access logs
│   ├── monitor.go        # Service monitoring
│   └── ...               # Other commands
├── pkg/                  # Reusable packages
│   ├── config/           # Configuration management
│   ├── deployer/         # Core deployment engine
│   ├── git/              # Git operations
│   ├── ssh/              # SSH client with pooling
│   ├── provisioner/      # Server setup
│   ├── monitoring/       # Service health monitoring
│   ├── accesslog/        # Access log formatting
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
