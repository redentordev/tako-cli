# Multi-Server Deployment with Docker Swarm

This example demonstrates how to deploy applications across multiple servers using Docker Swarm orchestration.

## Features Demonstrated

- **Automatic Swarm Detection**: Swarm mode is automatically enabled when 2+ servers are configured
- **Environment Isolation**: Different server topologies for dev (1 server), staging (2 servers), production (3 servers)
- **Placement Strategies**: Control where services run across the cluster
- **High Availability**: Multiple replicas with automatic failover
- **Rolling Updates**: Zero-downtime deployments with automatic rollback
- **Cross-Host Networking**: Services communicate seamlessly across servers

## Prerequisites

1. **Multiple Servers**: You need 2-3 Linux servers with:
   - Docker installed
   - SSH access
   - Open ports: 2377 (Swarm), 7946 (overlay network), 4789 (overlay network)

2. **Environment Variables**: Create a `.env` file:
   ```bash
   # Server inventory (replace with your actual server IPs)
   SERVER1_HOST=203.0.113.10
   SERVER2_HOST=203.0.113.11
   SERVER3_HOST=203.0.113.12

   # Application config
   DATABASE_URL=postgresql://user:pass@db:5432/myapp
   ```

## Placement Strategies

### Spread Strategy
```yaml
placement:
  strategy: spread  # Distribute replicas evenly across all nodes
```
**Use case**: Web frontends, stateless APIs - maximize availability

### Pinned Strategy
```yaml
placement:
  strategy: pinned
  servers:
    - server1
    - server2
```
**Use case**: Databases, stateful services - control exact placement

### Any Strategy
```yaml
placement:
  strategy: any  # Swarm decides based on resource availability
```
**Use case**: Background workers, caches - flexible placement

## Deployment Workflow

### 1. Development (Single Server)
```bash
# Deploy to dev environment (no Swarm, single server)
tako deploy --env dev
```

### 2. Staging (2 Servers)
```bash
# First time: CLI will automatically:
# - Initialize Swarm on server1 (manager)
# - Join server2 as worker
# - Setup private registry
# - Create overlay network
# - Deploy services with placement constraints

tako deploy --env staging
```

### 3. Production (3 Servers)
```bash
# Deploy to production cluster
tako deploy --env production

# Check service status
tako ps --env production

# Scale a service
tako scale web --replicas 5 --env production
```

## How It Works

### Automatic Cluster Setup

When you deploy to an environment with 2+ servers, the CLI automatically:

1. **Initializes Swarm** on the first server (manager node)
2. **Joins Workers** to the cluster with proper authentication
3. **Labels Nodes** with environment and server metadata
4. **Creates Registry** for distributing images across nodes
5. **Sets Up Networking** with overlay networks for cross-host communication

### Service Deployment

Services are deployed as Docker Swarm services instead of standalone containers:

```bash
# Traditional single-server
docker run -d my-app

# Swarm mode (automatic)
docker service create --replicas 3 --name my-app my-app
```

### Rolling Updates

Swarm handles rolling updates automatically:
- Update 1 replica at a time
- Wait 10s between updates
- Automatically rollback on failure
- Zero downtime

## Service Communication

Services communicate using Swarm's built-in service discovery:

```yaml
# Web service calls API
env:
  API_URL: http://api:3001  # Uses service name, works across hosts
```

Swarm provides:
- **Service VIP**: Virtual IP for load balancing
- **DNS Resolution**: Service names resolve automatically
- **Load Balancing**: Requests distributed across replicas

## Monitoring

```bash
# List all services
tako ps --env production

# View service details
docker service ps multi-server-demo_production_web

# View logs
docker service logs -f multi-server-demo_production_api

# Inspect Swarm cluster
docker node ls
```

## Rollback

If a deployment fails, Swarm automatically rolls back:

```bash
# Manual rollback to previous version
tako rollback web --env production
```

## Advantages

- **Zero Downtime**: Rolling updates with health checks
- **High Availability**: Automatic failover if a node fails
- **Resource Efficiency**: Automatic load distribution
- **Simple Management**: No external orchestrator needed
- **Built-in Networking**: Overlay networks handle cross-host communication

## Comparison: Single Server vs Swarm

| Aspect | Single Server | Multi-Server Swarm |
|--------|--------------|-------------------|
| Detection | 1 server configured | 2+ servers configured |
| Deployment | Docker containers | Docker services |
| Networking | Bridge network | Overlay network |
| Scaling | Manual restart | `docker service scale` |
| Updates | Container recreation | Rolling updates |
| Failover | Manual intervention | Automatic |
| Load Balancing | External (Caddy/Nginx) | Built-in service VIP |

## Advanced Configuration

### Custom Constraints

```yaml
placement:
  strategy: spread
  constraints:
    - node.role==worker
    - node.labels.disk==ssd
  preferences:
    - spread=node.labels.zone
```

### Resource Limits

```yaml
services:
  api:
    resources:
      limits:
        cpus: '2'
        memory: 2G
      reservations:
        cpus: '1'
        memory: 512M
```

### Update Configuration

```yaml
services:
  web:
    update_config:
      parallelism: 2
      delay: 10s
      failure_action: rollback
      monitor: 30s
```

## Troubleshooting

### Swarm Not Initializing

```bash
# Check if Swarm is active
ssh root@server1 "docker info | grep Swarm"

# View swarm state
cat .tako/swarm_multi-server-demo_production.json

# Force re-initialization
tako swarm reset --env production
```

### Services Not Starting

```bash
# Check service logs
docker service logs <service-name>

# Inspect service
docker service inspect <service-name>

# Check node availability
docker node ls
```

### Network Issues

```bash
# Verify overlay network
docker network ls | grep tako_

# Check firewall rules
# Swarm requires: 2377/tcp, 7946/tcp+udp, 4789/udp
```

## Best Practices

1. **Use odd number of managers** (1, 3, or 5) for quorum
2. **Pin stateful services** to specific nodes
3. **Spread stateless services** for high availability
4. **Use health checks** for automatic recovery
5. **Set resource limits** to prevent resource exhaustion
6. **Monitor node health** regularly
7. **Keep Swarm tokens secure** (stored in `.tako/`)

## Next Steps

- Learn about [placement constraints](../../docs/placement.md)
- Configure [health checks](../../docs/health-checks.md)
- Setup [monitoring](../../docs/monitoring.md)
- Implement [auto-scaling](../../docs/scaling.md)
