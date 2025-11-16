# Umami Analytics - Simple & Fast Analytics

This example demonstrates deploying **Umami Analytics**, a simple, fast, privacy-focused alternative to Google Analytics.

## What This Demonstrates

- ✅ Lightweight analytics platform
- ✅ PostgreSQL database integration
- ✅ Service dependencies
- ✅ Privacy-focused analytics
- ✅ Simple two-service architecture

## About Umami

Umami is a simple, easy-to-use, self-hosted web analytics solution. It's privacy-focused, GDPR compliant, and doesn't use cookies.

- **Official Image:** `ghcr.io/umami-software/umami`
- **Documentation:** https://umami.is/docs
- **GitHub:** https://github.com/umami-software/umami

## Prerequisites

- Server with Tako CLI installed
- 1GB RAM minimum
- Domain or IP address for HTTPS access

## Configuration

### 1. Copy Environment File

```bash
cp .env.example .env
```

### 2. Generate App Secret

Generate a secure app secret:

```bash
# On Linux/Mac
openssl rand -hex 32

# On Windows (PowerShell)
$bytes = New-Object byte[] 32; (New-Object Security.Cryptography.RNGCryptoServiceProvider).GetBytes($bytes); [BitConverter]::ToString($bytes).Replace("-","").ToLower()
```

### 3. Update Variables

Edit `.env`:

```bash
# Your server IP
SERVER_HOST=95.216.194.236

# Email for SSL certificate
LETSENCRYPT_EMAIL=your-email@example.com

# Secure password
POSTGRES_PASSWORD=your_secure_password_here

# Paste the generated app secret
APP_SECRET=paste_64_character_hex_string_here
```

## Deployment

```bash
# Deploy to production
tako deploy

# Check status
tako ps
```

## Access Umami

After deployment, access Umami at:

```
https://umami.<your-server-ip>.sslip.io
```

Example: `https://umami.95.216.194.236.sslip.io`

### Default Login

First login credentials:
- **Username:** `admin`
- **Password:** `umami`

**IMPORTANT:** Change the password immediately after first login!

## Architecture

Two services:

### 1. PostgreSQL (postgres)
- Stores analytics data and configuration
- Persistent volume: `postgres_data`
- Port: 5432

### 2. Umami (umami)
- Web interface and analytics API
- Depends on PostgreSQL
- Public-facing with HTTPS
- Port: 3000

## Data Persistence

The `postgres_data` volume stores all analytics data and configuration.

## Environment Variables

Key Umami environment variables:

- `DATABASE_URL` - PostgreSQL connection string
- `DATABASE_TYPE` - Database type (postgresql)
- `APP_SECRET` - Secret for encrypting sessions (REQUIRED)
- `HOSTNAME` - Bind address

See [Umami environment variables](https://umami.is/docs/environment-variables) for more options.

## Tracking Your Website

After setup, add this script to your website:

```html
<script defer src="https://umami.<your-server-ip>.sslip.io/script.js" data-website-id="your-website-id"></script>
```

Get your `website-id` from Umami dashboard after adding your site.

## Managing the Deployment

```bash
# View logs
tako logs umami
tako logs postgres

# Stop services
tako stop

# Start services
tako start

# Remove deployment
tako remove
```

## Custom Domain

To use a custom domain:

1. Point your domain's DNS A record to your server IP
2. Update `tako.yaml`:
```yaml
proxy:
  domains:
    - analytics.yourdomain.com
```

## Backup

```bash
# SSH into server
ssh root@your-server-ip

# Backup PostgreSQL database
docker exec -t umami_production_postgres_0 pg_dump -U umami umami > umami-backup-$(date +%Y%m%d).sql
```

## Restore

```bash
# Restore PostgreSQL database
cat umami-backup-20251114.sql | docker exec -i umami_production_postgres_0 psql -U umami -d umami
```

## Scaling

For high traffic, scale Umami horizontally:

```yaml
umami:
  replicas: 3  # Multiple instances
```

PostgreSQL remains single instance.

## Troubleshooting

### Can't login with default credentials

Check Umami logs:
```bash
tako logs umami
```

Verify database connection:
```bash
tako logs postgres
```

### Analytics not tracking

1. Verify tracking script is added to your website
2. Check browser console for errors
3. Verify website is added in Umami dashboard

## Resource Usage

Minimal resource requirements:

- **Umami**: ~100MB RAM per instance
- **PostgreSQL**: ~100-200MB RAM

Minimum recommended: **1GB RAM total**

## Security

1. **Change Default Password** - Immediately after first login
2. **Use Strong App Secret** - Generate random 64-character hex string
3. **Secure Database Password** - Use strong, unique password
4. **Regular Backups** - Schedule automated database backups

## Production Recommendations

1. **Version Pinning**
   ```yaml
   image: ghcr.io/umami-software/umami:postgresql-v2.10.0
   ```

2. **Custom Domain** - Use your own domain

3. **Regular Backups** - Automate database backups

4. **Monitoring** - Use Tako's monitoring features

## Umami vs Plausible

**Umami Advantages:**
- Lighter weight (smaller resource footprint)
- Simpler setup (only PostgreSQL required)
- Faster deployment

**Plausible Advantages:**
- More features (funnels, custom properties)
- Better performance at scale (uses ClickHouse)
- More frequent updates

Choose Umami for simplicity, Plausible for features.

## Learn More

- [Umami Documentation](https://umami.is/docs)
- [Running on Docker](https://umami.is/docs/running-on-docker)
- [Environment Variables](https://umami.is/docs/environment-variables)
- [API Reference](https://umami.is/docs/api)

## What's Next?

Try other analytics or monitoring platforms:
- Plausible (`plausible/analytics`) - Feature-rich analytics
- Matomo (`matomo:latest`) - Comprehensive analytics suite
- Grafana (`grafana/grafana`) - Metrics visualization
