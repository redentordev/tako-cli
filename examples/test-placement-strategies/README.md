# Placement Strategies Test Example

This example demonstrates Tako's different placement strategies for Docker Swarm deployments.

## Services Included

### 1. web-default
- **Strategy:** None (default)
- **Replicas:** 2
- **Behavior:** Runs on any available nodes
- **URL:** https://default.placement-test.tako.dev

### 2. web-spread
- **Strategy:** `spread`
- **Replicas:** 3
- **Behavior:** Distributes evenly across all nodes
- **URL:** https://spread.placement-test.tako.dev

### 3. monitoring-agent
- **Strategy:** `global` ⭐ NEW
- **Replicas:** Auto (one per node)
- **Behavior:** Runs exactly one instance on EVERY node in the swarm
- **URL:** https://monitor.placement-test.tako.dev

### 4. worker-only
- **Strategy:** Constraint-based
- **Replicas:** 2
- **Constraint:** `node.role==worker`
- **Behavior:** Only runs on worker nodes
- **URL:** https://worker.placement-test.tako.dev

### 5. manager-only
- **Strategy:** Constraint-based
- **Replicas:** 1
- **Constraint:** `node.role==manager`
- **Behavior:** Only runs on manager nodes
- **URL:** https://manager.placement-test.tako.dev

## Setup

1. Copy `.env.example` to `.env`:
   ```bash
   cp .env.example .env
   ```

2. Edit `.env` with your server IP and email:
   ```bash
   SERVER_HOST=95.216.194.236
   LETSENCRYPT_EMAIL=admin@yourdomain.com
   ```

3. Deploy:
   ```bash
   tako deploy
   ```

## Verification

### Check Service Distribution

```bash
# SSH into your server
ssh root@95.216.194.236

# List all services with their placement
docker service ls

# Check where each service is running
docker service ps placement-test_production_web-default
docker service ps placement-test_production_web-spread
docker service ps placement-test_production_monitoring-agent  # Should show on ALL nodes
docker service ps placement-test_production_worker-only
docker service ps placement-test_production_manager-only

# Check global service (should have one task per node)
docker service inspect placement-test_production_monitoring-agent --pretty
```

### Test from Browser

Visit each URL (replace with your actual IPs/domains):
- https://default.placement-test.tako.dev
- https://spread.placement-test.tako.dev
- https://monitor.placement-test.tako.dev
- https://worker.placement-test.tako.dev
- https://manager.placement-test.tako.dev

## Expected Results

### Single-Server Swarm
All services run on the single manager node.

### Two-Server Swarm (1 Manager + 1 Worker)
- `web-default`: Runs on any node (likely distributed)
- `web-spread`: Spreads across both nodes (manager + worker)
- `monitoring-agent`: One on manager, one on worker (2 total)
- `worker-only`: Only on worker node (2 replicas)
- `manager-only`: Only on manager node (1 replica)

### Three-Server Swarm (1 Manager + 2 Workers)
- `web-default`: Distributed across available nodes
- `web-spread`: One on each node (3 replicas spread evenly)
- `monitoring-agent`: One on each node (3 total - auto-scaled!)
- `worker-only`: Distributed across 2 worker nodes
- `manager-only`: Only on manager node

## Key Learnings

### Global Strategy Benefits
- Automatically scales with cluster growth
- Perfect for system-level services
- No manual replica management
- Guaranteed presence on every node

### When to Use Each Strategy

**Default:** Simple apps, don't care about placement
**Spread:** High availability, even load distribution
**Global:** Monitoring, logging, security agents
**Constraints:** Hardware requirements, role-based placement

## Cleanup

```bash
tako destroy
```

## See Also
- [PLACEMENT-STRATEGIES-GUIDE.md](../PLACEMENT-STRATEGIES-GUIDE.md) - Comprehensive guide
- [MULTI-SERVER-DEPLOYMENT-PLAN.md](../../MULTI-SERVER-DEPLOYMENT-PLAN.md) - Multi-server architecture
