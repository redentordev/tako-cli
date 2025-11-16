# n8n Workflow Automation - Public Docker Image Example

This example demonstrates deploying **n8n** (workflow automation tool) using its official Docker image from Docker Hub, without writing any Dockerfile.

## What This Demonstrates

- ✅ Deploying public Docker images (no custom Dockerfile needed)
- ✅ Using official images from Docker Hub
- ✅ Persistent data storage with named volumes
- ✅ HTTPS with Let's Encrypt
- ✅ Environment configuration for public images

## About n8n

n8n is a workflow automation tool that lets you connect different services and build powerful workflows. It's self-hosted, meaning you have full control over your data.

- **Official Image:** `docker.n8n.io/n8nio/n8n`
- **Documentation:** https://docs.n8n.io/
- **Docker Hub:** https://hub.docker.com/r/n8nio/n8n

## Prerequisites

- Server with Tako CLI installed
- Domain or IP address for HTTPS access

## Configuration

### 1. Copy Environment File

```bash
cp .env.example .env
```

### 2. Update Variables

Edit `.env`:

```bash
# Your server IP address
SERVER_HOST=95.216.194.236

# Email for SSL certificate
LETSENCRYPT_EMAIL=your-email@example.com
```

## Deployment

```bash
# Deploy to production
tako deploy

# Check status
tako ps
```

## Access n8n

After deployment, access n8n at:

```
https://n8n.<your-server-ip>.sslip.io
```

Example: `https://n8n.95.216.194.236.sslip.io`

On first access, you'll be prompted to create an admin account.

## Data Persistence

The `n8n_data` volume stores:
- Workflow definitions
- Credentials
- Execution history
- Settings

Data persists across deployments and restarts.

## Environment Variables

Key n8n environment variables used:

- `N8N_HOST` - The domain where n8n is accessible
- `N8N_PORT` - Internal port (5678)
- `N8N_PROTOCOL` - Use HTTPS
- `WEBHOOK_URL` - Base URL for webhooks
- `GENERIC_TIMEZONE` - Server timezone

See [n8n environment variables documentation](https://docs.n8n.io/hosting/configuration/environment-variables/) for more options.

## Managing the Deployment

```bash
# View logs
tako logs n8n

# Stop service
tako stop

# Start service
tako start

# Remove deployment
tako remove
```

## Scaling

n8n doesn't support horizontal scaling (multiple replicas) due to its workflow execution model. Keep replicas at 1.

## Custom Domain

To use a custom domain instead of sslip.io:

1. Point your domain's DNS A record to your server IP
2. Update `tako.yaml`:
```yaml
proxy:
  domains:
    - n8n.yourdomain.com
  email: admin@yourdomain.com
env:
  N8N_HOST: n8n.yourdomain.com
  WEBHOOK_URL: https://n8n.yourdomain.com/
```

## Backup

To backup your n8n data:

```bash
# SSH into server
ssh root@your-server-ip

# Backup the volume
docker run --rm -v n8n_production_n8n_data:/data -v $(pwd):/backup \
  alpine tar czf /backup/n8n-backup-$(date +%Y%m%d).tar.gz -C /data .
```

## Restore

```bash
# Restore from backup
docker run --rm -v n8n_production_n8n_data:/data -v $(pwd):/backup \
  alpine tar xzf /backup/n8n-backup-20251114.tar.gz -C /data
```

## Key Features of This Example

### Using Public Docker Images

No Dockerfile needed! Just specify the image:

```yaml
services:
  n8n:
    image: docker.n8n.io/n8nio/n8n:latest
```

Tako CLI will:
1. Pull the image from Docker Hub
2. Deploy it with your configuration
3. Set up HTTPS automatically
4. Configure persistent storage

### Named Volumes

```yaml
volumes:
  - n8n_data:/home/node/.n8n
```

Tako automatically creates and manages the volume `{project}_{env}_{volume_name}`.

### Environment Configuration

Configure the public image using environment variables:

```yaml
env:
  N8N_HOST: n8n.${SERVER_HOST}.sslip.io
  N8N_PROTOCOL: https
```

## Troubleshooting

### Service not accessible

Check service status:
```bash
tako ps
```

View logs:
```bash
tako logs n8n
```

### SSL certificate issues

Verify domain resolves to your server:
```bash
nslookup n8n.<your-ip>.sslip.io
```

Check Traefik logs:
```bash
ssh root@your-server-ip
docker service logs traefik
```

### Data not persisting

Verify volume exists:
```bash
ssh root@your-server-ip
docker volume ls | grep n8n
```

## Production Recommendations

1. **Backups** - Schedule regular backups of the n8n_data volume
2. **Updates** - Pin to a specific version instead of `latest`
3. **Custom Domain** - Use your own domain for production
4. **Environment Variables** - Add authentication, email settings, etc.

## Learn More

- [n8n Documentation](https://docs.n8n.io/)
- [n8n Docker Setup](https://docs.n8n.io/hosting/installation/docker/)
- [n8n Environment Variables](https://docs.n8n.io/hosting/configuration/environment-variables/)
- [Tako CLI Documentation](https://github.com/yourusername/tako-cli)

## What's Next?

Try deploying other public Docker images:
- Plausible Analytics (`plausible/analytics`)
- Umami Analytics (`ghcr.io/umami-software/umami`)
- Ghost CMS (`ghost:latest`)
- Grafana (`grafana/grafana`)
- Metabase (`metabase/metabase`)

All of these can be deployed using Tako CLI without writing any Dockerfiles!
