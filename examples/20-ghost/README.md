# Ghost CMS - Professional Publishing Platform

This example demonstrates deploying **Ghost**, a powerful content management system and publishing platform with built-in SEO, newsletter, and membership tools.

## What This Demonstrates

- ✅ Production-ready CMS deployment
- ✅ MySQL database integration
- ✅ Persistent content storage
- ✅ Professional publishing platform
- ✅ Built-in membership and newsletter features

## About Ghost

Ghost is a powerful app for new-media creators to publish, share, and grow a business around their content. It's used by Apple, NASA, Sky News, Cloudflare, and thousands of other creators.

- **Official Image:** `ghost`
- **Documentation:** https://ghost.org/docs/
- **Docker Hub:** https://hub.docker.com/_/ghost

## Prerequisites

- Server with Tako CLI installed
- 1GB RAM minimum (2GB+ recommended)
- Domain or IP address for HTTPS access

## Configuration

### 1. Copy Environment File

```bash
cp .env.example .env
```

### 2. Update Variables

Edit `.env`:

```bash
# Your server IP
SERVER_HOST=95.216.194.236

# Email for SSL certificate
LETSENCRYPT_EMAIL=your-email@example.com

# Secure passwords
MYSQL_ROOT_PASSWORD=secure_root_password_here
MYSQL_PASSWORD=secure_ghost_password_here
```

## Deployment

```bash
# Deploy to production
tako deploy

# Check status
tako ps
```

The deployment will:
1. Start MySQL database
2. Start Ghost CMS
3. Initialize Ghost database
4. Configure HTTPS with Let's Encrypt

## Access Ghost

After deployment, access Ghost at:

```
https://ghost.<your-server-ip>.sslip.io
```

Example: `https://ghost.95.216.194.236.sslip.io`

### First-Time Setup

1. Navigate to your Ghost URL
2. Go to `/ghost` to access admin panel:
   ```
   https://ghost.<your-server-ip>.sslip.io/ghost
   ```
3. Create your admin account
4. Configure your site

## Architecture

Two services:

### 1. MySQL (mysql)
- Stores posts, users, settings, and all Ghost data
- Persistent volume: `mysql_data`
- Port: 3306

### 2. Ghost (ghost)
- Publishing platform and admin interface
- Persistent volume: `ghost_content` (themes, images, files)
- Public-facing with HTTPS
- Port: 2368

## Data Persistence

Two volumes store all data:

- `mysql_data` - Database (posts, users, settings)
- `ghost_content` - Uploaded images, themes, files

All data persists across deployments and restarts.

## Environment Variables

Key Ghost environment variables:

- `database__client` - Database type (mysql)
- `database__connection__*` - Database connection details
- `url` - The public URL of your Ghost site (REQUIRED)
- `NODE_ENV` - Production mode

Ghost uses double underscores (`__`) for nested configuration.

See [Ghost configuration docs](https://ghost.org/docs/config/) for all options.

## Managing the Deployment

```bash
# View logs
tako logs ghost
tako logs mysql

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
    - blog.yourdomain.com
env:
  url: https://blog.yourdomain.com
```

**Important:** The `url` environment variable must match your domain exactly!

## Email Configuration (Newsletters)

To enable newsletters, add email settings:

```yaml
env:
  # Mailgun example
  mail__transport: SMTP
  mail__options__service: Mailgun
  mail__options__host: smtp.mailgun.org
  mail__options__port: "587"
  mail__options__auth__user: postmaster@mg.yourdomain.com
  mail__options__auth__pass: your-mailgun-password
  mail__from: noreply@yourdomain.com
```

Ghost supports:
- Mailgun
- SendGrid
- Amazon SES
- Gmail (for testing only)

See [Ghost email docs](https://ghost.org/docs/config/#mail) for more options.

## Themes

### Installing Themes

1. SSH into server
2. Upload theme to Ghost content volume:
   ```bash
   ssh root@your-server-ip
   docker cp mytheme.zip ghost_production_ghost_0:/var/lib/ghost/content/themes/
   ```
3. Extract and activate in Ghost admin panel

### Official Themes

- Download from https://ghost.org/themes/
- Upload via Ghost admin panel: Settings → Design → Change theme

## Backup

```bash
# SSH into server
ssh root@your-server-ip

# Backup MySQL database
docker exec -t ghost_production_mysql_0 mysqldump -u ghost -p ghost > ghost-db-$(date +%Y%m%d).sql

# Backup Ghost content (images, themes)
docker run --rm -v ghost_production_ghost_content:/data -v $(pwd):/backup \
  alpine tar czf /backup/ghost-content-$(date +%Y%m%d).tar.gz -C /data .
```

## Restore

```bash
# Restore MySQL database
cat ghost-db-20251114.sql | docker exec -i ghost_production_mysql_0 mysql -u ghost -p ghost

# Restore Ghost content
docker run --rm -v ghost_production_ghost_content:/data -v $(pwd):/backup \
  alpine tar xzf /backup/ghost-content-20251114.tar.gz -C /data
```

## Scaling

Ghost can run multiple instances for high availability:

```yaml
ghost:
  replicas: 2  # Multiple instances
```

However, you'll need:
1. Shared storage for `ghost_content` (e.g., NFS)
2. Load balancing (handled by Traefik automatically)

For most use cases, a single instance is sufficient.

## Troubleshooting

### Ghost shows incorrect URL

Ensure `url` environment variable matches your domain:
```yaml
env:
  url: https://your-exact-domain.com
```

Restart Ghost after changing:
```bash
tako stop && tako start
```

### Database connection errors

Check MySQL logs:
```bash
tako logs mysql
```

Verify MySQL is running:
```bash
ssh root@your-server-ip
docker service ls
```

### Admin panel not accessible

1. Ensure Ghost is running: `tako ps`
2. Try accessing: `https://your-domain/ghost`
3. Check Ghost logs: `tako logs ghost`

## Resource Usage

Typical resource usage:

- **Ghost**: ~150-300MB RAM per instance
- **MySQL**: ~100-400MB RAM

Minimum recommended: **1GB RAM** (2GB+ for better performance)

## Security

1. **Change Default Passwords** - Use strong MySQL passwords
2. **Regular Backups** - Schedule automated backups
3. **Update Regularly** - Keep Ghost and MySQL updated
4. **Use Custom Domain** - For production sites
5. **Enable 2FA** - In Ghost admin panel

## Features

Ghost includes:

- ✅ Modern editor with Markdown support
- ✅ Built-in SEO tools
- ✅ Newsletter system
- ✅ Membership & subscriptions
- ✅ Custom themes
- ✅ Content API
- ✅ Integrations (Zapier, Slack, etc.)
- ✅ Analytics
- ✅ Multiple authors

## Production Recommendations

1. **Version Pinning**
   ```yaml
   image: ghost:5.75.0-alpine
   ```

2. **Custom Domain** - Use your own domain

3. **Email Setup** - Configure SMTP for newsletters

4. **Regular Backups** - Automate database and content backups

5. **CDN** - Use Cloudflare for static assets

6. **Monitoring** - Use Tako's monitoring features

## Membership & Subscriptions

Ghost has built-in support for:
- Free members
- Paid subscriptions
- Multiple membership tiers
- Stripe integration for payments

Configure in Ghost admin: Settings → Membership

## Ghost vs WordPress

**Ghost Advantages:**
- Much faster and lighter
- Modern, distraction-free editor
- Built-in newsletters and memberships
- Better security (Node.js vs PHP)
- SEO-optimized by default

**WordPress Advantages:**
- Larger plugin ecosystem
- More themes available
- More familiar to users
- Better for complex sites

Choose Ghost for blogging/publishing, WordPress for complex sites.

## Learn More

- [Ghost Documentation](https://ghost.org/docs/)
- [Ghost Themes](https://ghost.org/themes/)
- [Ghost Configuration](https://ghost.org/docs/config/)
- [Content API](https://ghost.org/docs/content-api/)
- [Ghost Newsletter](https://ghost.org/help/newsletters/)

## What's Next?

Try other CMS platforms:
- WordPress (`wordpress:latest`) - Most popular CMS
- Strapi (`strapi/strapi:latest`) - Headless CMS
- Directus (`directus/directus:latest`) - Data platform

Or explore other public Docker images with Tako CLI!
