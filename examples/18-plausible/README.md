# Plausible Analytics - Public Docker Image Example

This example demonstrates deploying **Plausible Analytics** using its official Docker image, complete with PostgreSQL and ClickHouse databases.

## ⚠️ IMPORTANT: Before You Start

**Prerequisites Checklist:**
- [ ] Server with at least 2GB RAM
- [ ] Ports 80 and 443 open in firewall
- [ ] SSH access to server
- [ ] Generated all required secrets (see step 2 below)

**Common Issues:**
- **Can't access URL?** → Check [Troubleshooting](#troubleshooting) section below
- **502/503 errors?** → Databases may not be ready yet (wait 1-2 minutes)
- **Migration errors?** → Check logs: `tako logs plausible`

## What This Demonstrates

- ✅ Deploying multi-service applications with public images
- ✅ Database dependencies (PostgreSQL + ClickHouse)
- ✅ Service dependencies with `depends_on`
- ✅ Multiple persistent volumes
- ✅ Environment-based configuration
- ✅ **Lifecycle hooks** for automated database migrations
- ✅ Production-ready analytics platform

## About Plausible

Plausible is a lightweight, open-source, and privacy-friendly alternative to Google Analytics. No cookies, fully compliant with GDPR, CCPA, and PECR.

- **Official Image:** `plausible/analytics`
- **Documentation:** https://plausible.io/docs
- **GitHub:** https://github.com/plausible/analytics

## Prerequisites

- Server with Tako CLI installed
- At least 2GB RAM recommended
- Domain or IP address for HTTPS access

## Configuration

### 1. Copy Environment File

```bash
cp .env.example .env
```

### 2. Generate Secret Key

Generate a secure secret key:

```bash
# On Linux/Mac
openssl rand -base64 64

# On Windows (PowerShell)
$bytes = New-Object byte[] 48; (New-Object Security.Cryptography.RNGCryptoServiceProvider).GetBytes($bytes); [Convert]::ToBase64String($bytes)
```

### 3. Update Variables

Edit `.env`:

```bash
# Your server IP
SERVER_HOST=95.216.194.236

# Email for SSL certificate
LETSENCRYPT_EMAIL=your-email@example.com

# Secure passwords
POSTGRES_PASSWORD=your_secure_postgres_password_here
CLICKHOUSE_PASSWORD=your_secure_clickhouse_password_here

# Paste the generated secret key
SECRET_KEY_BASE=paste_64_character_base64_string_here

# Email for notifications
MAILER_EMAIL=analytics@yourdomain.com
```

## Deployment

### Step-by-Step Deployment

```bash
# 1. Navigate to the Plausible example
cd examples/18-plausible

# 2. Copy environment file
cp .env.example .env

# 3. Generate secrets (run these commands)
echo "POSTGRES_PASSWORD=$(openssl rand -base64 32)"
echo "CLICKHOUSE_PASSWORD=$(openssl rand -base64 32)"
echo "SECRET_KEY_BASE=$(openssl rand -base64 64)"

# 4. Edit .env and paste the generated values
nano .env  # or use your preferred editor

# 5. Provision server (first time only)
tako setup --env production

# 6. Deploy all services
tako deploy --env production

# 7. Check deployment status
tako ps --env production

# 8. View logs (wait for migrations to complete)
tako logs plausible --env production
```

The deployment will:
1. Build and start PostgreSQL database
2. Build and start ClickHouse database  
3. Build and start Plausible application
4. **Run database migrations automatically** (via post-start hook)
5. Configure HTTPS with Let's Encrypt (1-2 minutes for certificate)

### First Deployment Timeline

```
0:00 - PostgreSQL starts
0:10 - ClickHouse starts
0:20 - Plausible starts
0:30 - Migrations run (via postStart hook)
0:45 - SSL certificate issued
1:00 - ✓ Plausible is ready!
```

**Expected:** Wait 1-2 minutes after deployment before accessing the URL.

## Lifecycle Hooks

This example demonstrates Tako's **lifecycle hooks** feature. Plausible requires database migrations to run after deployment. Tako automates this with the `postStart` hook:

```yaml
plausible:
  hooks:
    postStart:
      # Runs database migrations inside the container after it starts
      - "exec: /entrypoint.sh db migrate"
```

### How Hooks Work

Tako supports five lifecycle hooks:

1. **preBuild** - Runs before building Docker image
2. **postBuild** - Runs after building Docker image
3. **preDeploy** - Runs before deploying service
4. **postDeploy** - Runs after deploying service
5. **postStart** - Runs after service is running

### Hook Types

- **Shell commands** - Run on the server: `"echo 'Deploying...!'"`
- **Container commands** - Run inside the container: `"exec: /entrypoint.sh db migrate"`

### Example Use Cases

**Database Migrations:**
```yaml
hooks:
  postStart:
    - "exec: npm run migrate"
    - "exec: npm run seed"
```

**Cache Warming:**
```yaml
hooks:
  postStart:
    - "exec: php artisan cache:warm"
```

**Build-time Tasks:**
```yaml
hooks:
  preBuild:
    - "npm run generate-types"
  postBuild:
    - "docker scan {{IMAGE}}"
```

**Deployment Notifications:**
```yaml
hooks:
  postDeploy:
    - "curl -X POST https://hooks.slack.com/... -d 'Deployed!'"
```

## Access Plausible

After deployment, access Plausible at:

```
https://plausible.<your-server-ip>.sslip.io
```

Example: `https://plausible.95.216.194.236.sslip.io`

### First-Time Setup

1. Navigate to your Plausible URL
2. Click "Register" to create your account
3. After registration, you can disable new signups by setting:
   ```yaml
   env:
     DISABLE_REGISTRATION: "true"
   ```
4. Add your website and get the tracking script

## Architecture

This deployment includes three services:

### 1. PostgreSQL (postgres)
- Stores user accounts, site configurations
- Persistent volume: `postgres_data`
- Port: 5432

### 2. ClickHouse (clickhouse)
- Stores analytics events and aggregations
- High-performance columnar database
- Persistent volumes: `clickhouse_data`, `clickhouse_logs`
- Port: 8123

### 3. Plausible (plausible)
- Web interface and analytics API
- Depends on both databases
- Public-facing with HTTPS
- Port: 8000

## Data Persistence

Three volumes store all data:

- `postgres_data` - User accounts, sites, settings
- `clickhouse_data` - Analytics events and stats
- `clickhouse_logs` - ClickHouse logs

All data persists across deployments and restarts.

## Environment Variables

Key Plausible environment variables:

- `BASE_URL` - The public URL of your Plausible instance
- `SECRET_KEY_BASE` - Secret for encrypting sessions (REQUIRED)
- `DATABASE_URL` - PostgreSQL connection string
- `CLICKHOUSE_DATABASE_URL` - ClickHouse connection string
- `DISABLE_REGISTRATION` - Set to "true" after creating your account

See [Plausible self-hosting docs](https://plausible.io/docs/self-hosting-configuration) for all options.

## Managing the Deployment

```bash
# View logs for all services
tako logs

# View logs for specific service
tako logs plausible
tako logs postgres
tako logs clickhouse

# Stop all services
tako stop

# Start all services
tako start

# Remove deployment
tako remove
```

## Tracking Your Website

After setup, add this script to your website:

```html
<script defer data-domain="yourdomain.com" src="https://plausible.<your-server-ip>.sslip.io/js/script.js"></script>
```

Replace `yourdomain.com` with your actual domain.

## Custom Domain

To use a custom domain:

1. Point your domain's DNS A record to your server IP
2. Update `tako.yaml`:
```yaml
proxy:
  domains:
    - analytics.yourdomain.com
env:
  BASE_URL: https://analytics.yourdomain.com
```

## Backup

### Database Backup

```bash
# SSH into server
ssh root@your-server-ip

# Backup PostgreSQL
docker exec -t plausible_production_postgres_0 pg_dump -U plausible plausible_db > plausible-$(date +%Y%m%d).sql

# Backup ClickHouse
docker run --rm -v plausible_production_clickhouse_data:/data -v $(pwd):/backup \
  alpine tar czf /backup/clickhouse-backup-$(date +%Y%m%d).tar.gz -C /data .
```

## Restore

```bash
# Restore PostgreSQL
cat plausible-20251114.sql | docker exec -i plausible_production_postgres_0 psql -U plausible -d plausible_db

# Restore ClickHouse
docker run --rm -v plausible_production_clickhouse_data:/data -v $(pwd):/backup \
  alpine tar xzf /backup/clickhouse-backup-20251114.tar.gz -C /data
```

## Email Configuration (Optional)

To enable email notifications, add SMTP settings:

```yaml
env:
  MAILER_EMAIL: plausible@yourdomain.com
  SMTP_HOST_ADDR: smtp.gmail.com
  SMTP_HOST_PORT: "587"
  SMTP_USER_NAME: your-email@gmail.com
  SMTP_USER_PWD: your-app-password
  SMTP_HOST_SSL_ENABLED: "true"
```

## Scaling Considerations

- **Plausible**: Can run multiple replicas behind Traefik
- **PostgreSQL**: Single instance (use managed PostgreSQL for HA)
- **ClickHouse**: Single instance (use ClickHouse cluster for HA)

For high traffic:
```yaml
plausible:
  replicas: 3  # Scale horizontally
```

## Troubleshooting

### Services not starting

Check logs:
```bash
tako logs postgres
tako logs clickhouse
tako logs plausible
```

### Database connection errors

Verify databases are running:
```bash
ssh root@your-server-ip
docker service ls
```

### Can't access Plausible

Check Traefik configuration:
```bash
ssh root@your-server-ip
docker service logs traefik
```

## Security

1. **Change Default Passwords** - Use strong, unique passwords
2. **Disable Registration** - After creating your account, set `DISABLE_REGISTRATION: "true"`
3. **Use Custom Domain** - For production, use your own domain
4. **Regular Backups** - Schedule automated backups of databases
5. **Keep Updated** - Pin specific versions and update regularly

## Production Recommendations

1. **Version Pinning** - Use specific versions instead of `latest`
   ```yaml
   image: plausible/analytics:v2.0.0
   ```

2. **Custom Domain** - Use your own domain with proper DNS

3. **Email Setup** - Configure SMTP for password resets and reports

4. **Disable Registration** - After setup, prevent new signups

5. **Regular Backups** - Automate database backups

6. **Monitoring** - Use Tako's monitoring features

## Resource Usage

Typical resource usage:

- **Plausible**: ~200MB RAM per instance
- **PostgreSQL**: ~100-500MB RAM
- **ClickHouse**: ~500MB-2GB RAM

Minimum recommended: **2GB RAM total**

## Learn More

- [Plausible Documentation](https://plausible.io/docs)
- [Self-Hosting Guide](https://plausible.io/docs/self-hosting)
- [Configuration Options](https://plausible.io/docs/self-hosting-configuration)
- [Plausible vs Google Analytics](https://plausible.io/vs-google-analytics)

## What's Next?

Try deploying other analytics platforms:
- Umami Analytics (`ghcr.io/umami-software/umami`)
- Matomo (`matomo:latest`)

Or other public Docker images:
- Ghost CMS (`ghost:latest`)
- Grafana (`grafana/grafana`)
- n8n Automation (`docker.n8n.io/n8nio/n8n`)
