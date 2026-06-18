# Example 6: Scaling & Load Balancing

This example demonstrates horizontal scaling with multiple replicas and automatic load balancing.

## Features Demonstrated

- **Multiple Replicas**: 3 instances of the same service
- **Load Balancing**: Automatic request distribution
- **Round-Robin Strategy**: Sequential distribution across instances
- **Instance Identification**: Each instance shows its unique hostname
- **High Availability**: Service continues if one instance fails
- **Reconciled Updates**: Tako recreates containers to match desired state

## How It Works

```
                    User Requests
                         ↓
                    ┌─────────┐
                    │tako-proxy│ (Load Balancer)
                    └────┬────┘
                         │ round_robin
         ┌───────────────┼───────────────┐
         ↓               ↓               ↓
    ┌─────────┐    ┌─────────┐    ┌─────────┐
    │Instance1│    │Instance2│    │Instance3│
    │ port 3000    │ port 3000    │ port 3000
    └─────────┘    └─────────┘    └─────────┘
```

Each request is routed to the next instance in sequence, distributing load evenly.

## Files

- `tako.yaml` - Service with replicas: 3 and loadBalancer config
- `Dockerfile` - Standard Node.js container
- `index.js` - Shows instance hostname and stats
- `package.json` - Dependencies

## Configuration Highlights

```yaml
services:
  web:
    port: 3000
    replicas: 3              # Run 3 instances
    loadBalancer:
      strategy: round_robin  # Distribution strategy
```

## Load Balancing Strategies

**Round Robin (default):**
- Distributes requests sequentially
- Instance1 → Instance2 → Instance3 → Instance1 → ...
- Simple and fair distribution

**Sticky:**
- Uses a secure HTTP-only cookie for session-affine traffic
- Useful for WebSocket-heavy services or temporary session affinity

## How to Deploy

1. Set server host:
   ```bash
   export SERVER_HOST=your.server.ip
   ```

2. Update domain in `tako.yaml`

3. Deploy:
   ```bash
   tako deploy -e production --yes
   ```

4. Visit your domain and keep refreshing - watch the hostname change!

## Seeing Load Balancing in Action

**Method 1: Browser**
- Visit the domain
- Refresh the page multiple times
- Watch the hostname change
- Each hostname = different container

**Method 2: cURL**
```bash
# Make multiple requests
for i in {1..10}; do
  curl -s https://scaling.example.com/api/instance | jq .hostname
done

# You'll see output like:
# scaling-web-1
# scaling-web-2
# scaling-web-3
# scaling-web-1
# scaling-web-2
# ...
```

**Method 3: Apache Bench**
```bash
# Send 100 requests with 10 concurrent
ab -n 100 -c 10 https://scaling.example.com/

# Check which instances handled requests
curl https://scaling.example.com/api/instance
```

## Scaling Up/Down

**Scale to 5 instances:**
```yaml
services:
  web:
    replicas: 5
```

**Scale to 1 instance:**
```yaml
services:
  web:
    replicas: 1
```

**Deploy changes:**
```bash
tako deploy -e production --yes
```

The system will:
- Add new instances if scaling up
- Gracefully stop instances if scaling down
- Update load balancer configuration
- Reconcile containers to the desired replica count

## Benefits of Horizontal Scaling

**High Availability:**
- If one instance crashes, others continue serving
- No single point of failure
- Automatic health checks

**Performance:**
- Handle more concurrent requests
- Distribute CPU and memory load
- Better response times under load

**Reconciled Deployments:**
- Tako compares desired config against running services
- Replica count changes are applied during deploy
- Health checks keep unhealthy instances out of proxy rotation

**Cost Efficiency:**
- Scale up during peak hours
- Scale down during off-peak
- Pay only for what you need

## Stateless Architecture

For scaling to work properly, instances must be **stateless**:

**Good (Stateless):**
```javascript
// Session in Redis/database
app.get('/cart', async (req, res) => {
  const cart = await redis.get(`cart:${userId}`);
  res.json(cart);
});
```

**Bad (Stateful):**
```javascript
// Session in memory - won't work with load balancing!
const userCarts = {};
app.get('/cart', (req, res) => {
  res.json(userCarts[userId]);
});
```

**Rules for stateless services:**
- Store sessions in Redis/database, not memory
- Don't store user data in local files
- Use external storage (S3, database) for uploads
- Make each request independent

## Testing Locally

You can't easily test load balancing locally, but you can test multiple instances:

**Terminal 1:**
```bash
PORT=3001 HOSTNAME=instance-1 node index.js
```

**Terminal 2:**
```bash
PORT=3002 HOSTNAME=instance-2 node index.js
```

**Terminal 3:**
```bash
PORT=3003 HOSTNAME=instance-3 node index.js
```

For deployed apps, Tako writes the `tako-proxy` route and upstream list from the
replica plan. For local ad hoc testing, call each local port directly or run
your own temporary local proxy; that proxy is outside the Tako deployment path.

## Health Checks

Each instance provides health status:

```bash
curl https://scaling.example.com/health
```

Returns:
```json
{
  "status": "healthy",
  "hostname": "scaling-web-2",
  "uptime": 3600,
  "requests": 1523,
  "timestamp": "2024-01-01T12:00:00.000Z"
}
```

The load balancer uses these to:
- Detect unhealthy instances
- Remove them from rotation
- Add them back when healthy

## Monitoring Instances

**List all instances:**
```bash
tako ps web
```

**View logs from all instances:**
```bash
tako logs --service web --tail 100
```

**Follow logs in real-time:**
```bash
tako logs --service web --follow
```

**Check resource usage:**
```bash
tako stats --service web
```

## When to Scale

**Scale up when:**
- CPU usage consistently > 70%
- Response times increasing
- Error rates increasing
- Expecting traffic spike

**Scale down when:**
- CPU usage consistently < 30%
- Over-provisioned for current load
- Cost optimization needed

**Auto-scaling (future feature):**
```yaml
services:
  web:
    replicas:
      min: 2
      max: 10
      cpu: 70%  # Scale up at 70% CPU
```

## Common Patterns

**1. Web Application:**
```yaml
replicas: 3
loadBalancer:
  strategy: round_robin
```

**2. API Service:**
```yaml
replicas: 5
loadBalancer:
  strategy: round_robin
```

**3. WebSocket Server:**
```yaml
replicas: 2
loadBalancer:
  strategy: sticky  # Session-affine traffic
```

## Troubleshooting

**Problem: Always hitting same instance**
- Check load balancer configuration
- Verify all instances are healthy
- Check if strategy is `sticky` and your client is sending the sticky cookie

**Problem: Uneven load distribution**
- Some instances may be slower
- Check resource usage per instance
- Use `round_robin` for stateless services

**Problem: Sessions not persisting**
- Application is stateful (needs fixing)
- Move sessions to Redis/database
- Use `sticky` as a temporary session-affinity fix
