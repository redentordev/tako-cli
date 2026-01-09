# 🐙 Tako CLI

**タコ - Deploy your applications to any VPS with zero configuration and zero downtime.**

## What is Tako?

**Tako** (タコ) is Japanese for "octopus" - pronounced "tah-koh". Just like an octopus has 8 arms to manage multiple tasks simultaneously, Tako CLI manages your deployments across multiple servers with precision and control.

Tako CLI is a powerful deployment automation tool that brings Platform-as-a-Service (PaaS) simplicity to your own infrastructure. Deploy Docker containers to your VPS servers with automatic HTTPS, health checks, zero-downtime deployments, and complete control over your infrastructure.

[![Version](https://img.shields.io/badge/version-0.2.1-blue)](https://github.com/redentordev/tako-cli/releases)
[![Go Version](https://img.shields.io/badge/go-%3E%3D1.21-blue)](https://golang.org/)
[![License](https://img.shields.io/badge/license-MIT-green)](./LICENSE)
[![Status](https://img.shields.io/badge/status-experimental-orange)](https://github.com/redentordev/tako-cli)

> **⚠️ Experimental Project**
>
> This is a personal pet project built by [Redentor Valerio](https://github.com/redentordev) ([@redentor_dev](https://twitter.com/redentor_dev)) for learning and experimentation. While functional, it is **not recommended for production use**.
>
> **For production deployments, check out [Uncloud](https://github.com/psviderski/uncloud)** - a more mature and actively maintained solution for deploying applications to your own servers.
>
> Feel free to explore Tako CLI, learn from it, or contribute - but use at your own risk. No liability or guarantees provided. See [License](#license) for details.

---

## Why Tako CLI?

Tako CLI brings PaaS-like simplicity with full infrastructure control - deploy to your own servers without vendor lock-in or monthly fees.

### Key Benefits

- Deploy in minutes, not hours or days
- Use your own servers (DigitalOcean, Hetzner, AWS EC2, any VPS)
- Zero-downtime deployments with automatic rollback
- Automatic HTTPS certificates (Let's Encrypt + Traefik)
- Git-based deployments with full version history
- No monthly PaaS fees - pay only for your server
- Multi-server orchestration with Docker Swarm
- Cross-project service networking

---

## Quick Start

### Installation

#### Recommended: Automated Install (Linux & macOS)

The install script automatically configures your PATH and verifies checksums:

```bash
curl -fsSL https://raw.githubusercontent.com/redentordev/tako-cli/master/install.sh | bash
```

**Features:**
- ✅ Automatic platform detection (Linux/macOS, AMD64/ARM64)
- ✅ SHA256 checksum verification for security
- ✅ Automatic PATH configuration for bash/zsh/fish
- ✅ Smart install directory selection (sudo-free when possible)
- ✅ Works immediately after installation

<details>
<summary>📦 Homebrew (macOS & Linux)</summary>

Coming soon! Homebrew tap is being prepared.

```bash
# This will be available soon
brew tap redentordev/tako
brew install tako
```

Benefits:
- Automatic updates with `brew upgrade`
- Managed dependencies
- Trusted by developers worldwide

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
# Tako CLI v0.2.1
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
# Check for updates
tako upgrade --check

# Upgrade to latest version
tako upgrade
```

#### Uninstalling

```bash
curl -fsSL https://raw.githubusercontent.com/redentordev/tako-cli/master/uninstall.sh | bash
```

Or manually:
```bash
sudo rm /usr/local/bin/tako
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
          domains:
            - my-app.${SERVER_HOST}.sslip.io         # Auto-DNS with sslip.io
          email: ${LETSENCRYPT_EMAIL}
        env:
          NODE_ENV: production
```

The template includes commented examples for:
- Database services (PostgreSQL, Redis)
- Background workers
- Health checks and lifecycle hooks
- Secrets management
- Multi-server deployments
- And more!

4. **Setup your server (one-time):**

```bash
tako setup -e production
```

This installs Docker, Traefik, configures firewall, and hardens security.

5. **Deploy your application:**

```bash
tako deploy -e production
```

Your app is now live with automatic HTTPS at `https://my-app.YOUR-SERVER-IP.sslip.io`!

---

## Features

### Deployment & Operations

- **Zero-Downtime Deployments** - Blue-green strategy with automatic health checks
- **Parallel Deployment** - Deploy multiple services concurrently (default behavior)
- **Instant Rollback** - Revert to any previous deployment with one command
- **Git-Based Versioning** - Every deployment tied to a Git commit
- **State Management** - Full deployment history tracked on server with CLI version tracking
- **Automatic HTTPS** - Traefik provisions SSL certificates via Let's Encrypt
- **Domain Redirects** - Automatic www → non-www (or vice versa) with path preservation
- **Health Checks** - Ensure containers are healthy before switching traffic
- **Secrets Management** - Secure handling of environment secrets with automatic redaction
- **Lifecycle Hooks** - Automate tasks at build, deploy, and start phases (migrations, cache warming, etc.)
- **Volume Backup/Restore** - Backup and restore Docker volumes with `tako backup`
- **Drift Detection** - Detect configuration drift with `tako drift`

### Infrastructure & Scaling

- **Cloud Infrastructure Provisioning** - Provision servers on DigitalOcean, Hetzner, Linode, or AWS with one command
- **Multi-Server Support** - Deploy to multiple servers simultaneously
- **Docker Swarm Integration** - Automatic orchestration for 2+ servers
- **Placement Strategies** - Control service placement (spread, pinned, any)
- **Cross-Project Networking** - Services communicate across projects
- **Service Discovery** - Built-in DNS and load balancing
- **Server Provisioning** - One-command setup with security hardening
- **NFS Shared Storage** - Shared volumes across multiple servers with automatic setup

### Developer Experience

- **Simple YAML Configuration** - Intuitive and readable
- **Environment Variables** - Full support with .env files
- **Local Development Mode** - Run production environment locally with `tako dev`
- **Auto-Update** - Built-in upgrade mechanism with `tako upgrade`
- **Verbose Logging** - Detailed output for debugging
- **Cross-Platform** - Single binary for Windows, macOS, Linux
- **No Dependencies** - Just the binary and SSH access

---

## Core Commands

### Infrastructure Provisioning

| Command | Description |
|---------|-------------|
| `tako provision` | Provision cloud infrastructure (servers, VPC, firewall) |
| `tako provision --preview` | Preview infrastructure changes |
| `tako infra outputs` | Show provisioned server IPs and details |
| `tako infra validate` | Validate infrastructure configuration |
| `tako infra destroy` | Destroy cloud infrastructure |

See [docs/INFRASTRUCTURE.md](./docs/INFRASTRUCTURE.md) for provider setup and credentials.

### Deployment & Management

| Command | Description |
|---------|-------------|
| `tako init` | Initialize new project with template config |
| `tako setup` | Provision server (Docker, Traefik, security hardening) |
| `tako deploy` | Deploy application to environment |
| `tako rollback [id]` | Rollback to previous/specific deployment |
| `tako destroy` | Remove all services from server |

### Operations & Monitoring

| Command | Description |
|---------|-------------|
| `tako ps` | List running services and their status |
| `tako logs` | Stream container logs |
| `tako access` | Stream access logs from Traefik (HTTP requests) |
| `tako metrics` | View system metrics from servers |
| `tako monitor` | Continuously monitor deployed services |
| `tako history` | View deployment history |

### Service Control

| Command | Description |
|---------|-------------|
| `tako start` | Start stopped services (scales to configured replicas) |
| `tako stop` | Stop running services (scales to 0) |
| `tako scale` | Scale service replicas |
| `tako exec` | Execute commands on remote server(s) or inside containers |

### Backup & Recovery

| Command | Description |
|---------|-------------|
| `tako backup --volume <name>` | Backup a Docker volume |
| `tako backup --list` | List available backups |
| `tako backup --restore <id>` | Restore a volume from backup |
| `tako backup --cleanup <days>` | Delete backups older than N days |
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

### Shared Storage

| Command | Description |
|---------|-------------|
| `tako storage status` | Show NFS storage status across all servers |
| `tako storage remount` | Remount NFS exports on all clients |

### Development & Utilities

| Command | Description |
|---------|-------------|
| `tako upgrade` | Upgrade Tako CLI to the latest version |
| `tako dev` | Run production environment locally |
| `tako live` | Live development mode with hot reload |
| `tako cleanup` | Clean up old Docker resources |
| `tako downgrade` | Downgrade from Docker Swarm to single-server mode |

### Common Flags

- `-v, --verbose` - Show detailed output
- `-e, --env <name>` - Target specific environment
- `-s, --server <name>` - Target specific server
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
      domains:
        - app.example.com
      email: admin@example.com
```

### Infrastructure Provisioning (Cloud Servers)

Provision servers automatically on DigitalOcean, Hetzner, Linode, or AWS:

```yaml
infrastructure:
  provider: hetzner           # or: digitalocean, linode, aws
  region: fsn1
  credentials:
    token: ${HCLOUD_TOKEN}    # Set via environment variable
  servers:
    web:
      size: cax11             # ARM-based, cost-effective
      role: manager

environments:
  production:
    servers: [web]            # Auto-populated from infrastructure
    services:
      app:
        build: .
        port: 3000
```

Then run:
```bash
tako provision   # Create cloud server
tako setup       # Install Docker, Traefik
tako deploy      # Deploy your app
```

See [docs/INFRASTRUCTURE.md](./docs/INFRASTRUCTURE.md) for all providers and options.

### Full-Stack Application

```yaml
services:
  web:
    build: ./frontend
    port: 3000
    proxy:
      domains:
        - app.example.com

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

### Multi-Server with Docker Swarm

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
          strategy: spread  # Distribute across all servers
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

### NFS Shared Storage

Share volumes across multiple servers using NFS. Tako automatically sets up the NFS server and clients during `tako setup`.

```yaml
# Configure NFS shared storage
storage:
  nfs:
    enabled: true
    server: auto  # Use manager node, or specify server name
    exports:
      - name: shared_repo
        path: /srv/nfs/repo
      - name: uploads
        path: /srv/nfs/uploads

servers:
  server1:
    host: ${SERVER1_HOST}
    role: manager    # NFS server will run here
  server2:
    host: ${SERVER2_HOST}
  server3:
    host: ${SERVER3_HOST}

environments:
  production:
    servers: [server1, server2, server3]
    services:
      session-manager:
        build: ./session-manager
        volumes:
          - nfs:shared_repo:/app/repo:ro    # Read-only NFS mount
          - sessions:/app/sessions          # Local volume for work
        
      content-server:
        build: ./content-server
        port: 3000
        replicas: 3
        volumes:
          - nfs:shared_repo:/app/repo:ro    # Same NFS mount, read-only
        placement:
          strategy: spread
```

**NFS Volume Format:**
```
nfs:<export_name>:<container_path>[:ro|:rw]
```

- `export_name` - Name of the export defined in storage.nfs.exports
- `container_path` - Mount path inside the container
- `:ro` - Read-only mount (default, recommended)
- `:rw` - Read-write mount

**Commands:**
```bash
# Check NFS storage status
tako storage status

# Remount NFS if mounts become stale
tako storage remount
```

**Notes:**
- NFS is best for read-heavy workloads
- For write-heavy operations (like git), use local volumes and sync/merge back
- All servers must be in the same datacenter for acceptable performance

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

### Lifecycle Hooks

Automate tasks at different deployment phases - perfect for migrations, cache warming, or notifications:

```yaml
services:
  api:
    build: .
    port: 3000
    hooks:
      preBuild:
        - "npm run generate-types"      # Run before building
      postBuild:
        - "docker scan {{IMAGE}}"        # Security scan after build
      preDeploy:
        - "curl https://api.slack.com/..." # Notify team
      postDeploy:
        - "echo 'Deployed successfully!'"
      postStart:
        - "exec: npm run migrate"        # Run migrations inside container
        - "exec: npm run seed"           # Seed database
```

**Hook Types:**

1. **Shell Commands** - Run on the server
   ```yaml
   - "echo 'Starting deployment'"
   - "curl -X POST https://webhook.com/notify"
   ```

2. **Container Commands** - Run inside the container (use `exec:` prefix)
   ```yaml
   - "exec: npm run migrate"
   - "exec: php artisan cache:clear"
   - "exec: python manage.py migrate"
   ```

**Available Lifecycle Phases:**

- `preBuild` - Before building Docker image
- `postBuild` - After building Docker image (use `{{IMAGE}}` placeholder)
- `preDeploy` - Before deploying service
- `postDeploy` - After deploying service
- `postStart` - After service is running (best for migrations)

**Common Use Cases:**

```yaml
# Database migrations
postStart:
  - "exec: npm run migrate"

# Cache warming
postStart:
  - "exec: php artisan cache:warm"

# Deployment notifications
postDeploy:
  - "curl -X POST https://hooks.slack.com/... -d 'Deployed!'"

# Security scanning
postBuild:
  - "docker scan {{IMAGE}}"
```

See the [Plausible example](./examples/18-plausible) for a real-world use case.

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
│ Traefik (HTTPS) │
│ Docker Engine   │
│ Your Containers │
│ State Storage   │
└─────────────────┘
```

### Multi-Server Swarm Mode

```
    Tako CLI
        │
   ┌────┴────┬────────┐
   │         │        │
Manager    Worker1  Worker2
   │         │        │
Registry  Services Services
Traefik   Replicas Replicas
   └─── Overlay Network ───┘
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
- **11-multi-server-swarm** - Multi-server orchestration

### Third-Party Applications
- **17-n8n** - n8n (workflow automation)
- **18-plausible** - Plausible (web analytics)
- **19-umami** - Umami (web analytics)
- **20-ghost** - Ghost (headless CMS)

### Testing & Advanced
- **test-parallel** - Parallel deployment testing
- **test-placement-strategies** - Swarm placement strategies
- **test-secrets** - Secrets management
- **test-swarm** - Docker Swarm testing

Each example includes complete documentation and is ready to deploy.

---

## Development

### Prerequisites

- Go 1.21 or higher
- Git
- Docker (for local testing)
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
│   ├── setup.go          # Server provisioning
│   ├── rollback.go       # Rollback functionality
│   ├── access.go         # Traefik access logs
│   ├── monitor.go        # Service monitoring
│   └── ...               # Other commands
├── pkg/                  # Reusable packages
│   ├── config/           # Configuration management
│   ├── deployer/         # Core deployment engine
│   ├── swarm/            # Docker Swarm orchestration
│   ├── traefik/          # Traefik reverse proxy
│   ├── git/              # Git operations
│   ├── ssh/              # SSH client with pooling
│   ├── provisioner/      # Server setup
│   ├── monitoring/       # Service health monitoring
│   ├── accesslog/        # Access log formatting
│   └── ...               # Other packages
├── internal/             # Internal packages
│   └── state/            # Deployment state management
├── examples/             # Example projects (25 examples)
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

**⚠️ Disclaimer:** This is a personal pet project. While functional, it comes with no guarantees or liability. Use at your own risk.

---

## Acknowledgments

Tako CLI is inspired by the excellent work of:

- [Kamal](https://kamal-deploy.org/) by DHH and 37signals
- [Dokku](https://dokku.com/) by Jeff Lindsay
- [Docker Swarm](https://docs.docker.com/engine/swarm/) by Docker Inc.
- The simplicity of Heroku's developer experience

---

**🐙 Built by [Redentor Valerio](https://github.com/redentordev) ([@redentor_dev](https://twitter.com/redentor_dev)) | Start deploying in minutes, not hours. Your servers, your rules.**
