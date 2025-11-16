# Simple Web Application - Getting Started Example

**The simplest possible Tako deployment** - perfect for learning or deploying your first app!

## What This Example Does

✓ Deploys a Node.js web app to your VPS  
✓ Sets up automatic HTTPS with Let's Encrypt  
✓ Makes your app publicly accessible at `https://simple-web.<YOUR-IP>.sslip.io`  
✓ Zero configuration needed (besides your server IP)

## Prerequisites

- A VPS server (DigitalOcean, Hetzner, AWS EC2, etc.)
- SSH access to the server
- Tako CLI installed

## Quick Start (3 Steps)

### 1. Copy the environment file

```bash
cp .env.example .env
```

### 2. Edit `.env` with your server IP

```bash
# .env
SERVER_HOST=95.216.194.236  # Replace with your actual server IP
LETSENCRYPT_EMAIL=you@example.com  # Your email for SSL certificates
```

### 3. Deploy!

```bash
# First time: provision the server with Docker, Traefik, etc.
tako setup

# Deploy your app
tako deploy
```

That's it! Your app will be live at:
```
https://simple-web.95.216.194.236.sslip.io
```
(Replace `95.216.194.236` with your server IP)

## What Gets Deployed

This example includes:

- **`index.js`** - Simple Express.js server
- **`Dockerfile`** - Builds the Node.js app
- **`tako.yaml`** - 15 lines of configuration

## Understanding tako.yaml

```yaml
project:
  name: simple-web        # Your project name
  version: 1.0.0

servers:
  prod:
    host: ${SERVER_HOST}  # From .env file
    user: root
    sshKey: ~/.ssh/id_ed25519

environments:
  production:
    servers:
      - prod
    services:
      web:
        build: .          # Build from Dockerfile in current directory
        port: 3000        # Port your app runs on inside container
        proxy:            # Makes your app publicly accessible
          domains:
            - simple-web.${SERVER_HOST}.sslip.io
          email: ${LETSENCRYPT_EMAIL}
        env:
          NODE_ENV: production
```

## What Happens During Deployment

Tako automatically:

1. **Builds** your Docker image on the server
2. **Deploys** the container with zero downtime
3. **Configures** Traefik reverse proxy
4. **Obtains** SSL certificate from Let's Encrypt
5. **Routes** traffic to your app

## Useful Commands

```bash
# View deployment status
tako ps

# View logs
tako logs

# View HTTP access logs
tako access

# Rollback to previous version
tako rollback

# Stop the service
tako stop

# Start the service
tako start

# Remove everything
tako destroy
```

## Testing Locally

Before deploying, test locally:

```bash
npm install
npm start
# Visit http://localhost:3000
```

## Customizing

### Use Your Own Domain

Instead of `sslip.io`, use your own domain:

1. Point your domain's A record to your server IP:
   ```
   app.example.com → 95.216.194.236
   ```

2. Update `tako.yaml`:
   ```yaml
   proxy:
     domains:
       - app.example.com
     email: you@example.com
   ```

3. Deploy:
   ```bash
   tako deploy
   ```

### Add Environment Variables

```yaml
services:
  web:
    env:
      NODE_ENV: production
      API_URL: https://api.example.com
      DATABASE_URL: ${DATABASE_URL}  # From .env file
```

### Add a Database

```yaml
services:
  web:
    build: .
    port: 3000
    proxy:
      domains: [app.example.com]
    env:
      DATABASE_URL: postgresql://postgres:5432/myapp

  postgres:
    image: postgres:16-alpine
    volumes:
      - postgres_data:/var/lib/postgresql/data
    env:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
```

## No Dockerfile? No Problem!

If you don't have a Dockerfile, Tako can auto-detect your framework with Nixpacks:

1. Remove the `Dockerfile`
2. Deploy - Tako will auto-detect Node.js and build automatically

Works with:
- Node.js (npm, yarn, pnpm)
- Python (Django, Flask)
- Go
- Ruby (Rails)
- PHP (Laravel)
- And more!

## Troubleshooting

### Can't connect to server?

```bash
# Test SSH connection
ssh root@YOUR_SERVER_IP

# Verify SSH key
ls -la ~/.ssh/id_ed25519
```

### Deployment failed?

```bash
# Check logs
tako logs --verbose

# Verify server status
tako ps
```

### SSL certificate not working?

- Make sure your domain's DNS is pointing to your server IP
- Wait 1-2 minutes for Let's Encrypt to issue certificate
- Check Traefik logs: `ssh root@server "docker logs traefik"`

## Next Steps

Once you've deployed this example, try:

- **[Example 02: Web + Database](../02-web-database)** - Add PostgreSQL
- **[Example 03: Full-Stack App](../03-fullstack)** - Frontend + API + DB
- **[Example 09: Next.js](../09-nextjs-todos)** - Deploy a Next.js app

## Learn More

- [Tako CLI Documentation](../../README.md)
- [Configuration Reference](../../README.md#configuration-examples)
- [Deployment Commands](../../README.md#core-commands)

## What Makes This Simple?

This example uses the absolute minimum configuration:

- **1 server** (your VPS)
- **1 service** (web app)
- **1 environment** (production)
- **15 lines** of config

From here, you can scale to:
- Multiple services (web, API, workers)
- Multiple servers (Swarm orchestration)
- Multiple environments (staging, production)
- Advanced features (secrets, hooks, health checks)

Start simple, scale when needed! 🐙
