# Example 5: Background Workers

This example demonstrates background job processing using Redis as a queue with multiple worker instances.

## Features Demonstrated

- **Worker Services**: Services with no port (background processing)
- **Multiple Replicas**: 3 worker instances processing jobs in parallel
- **Redis Queue**: Job queue with blocking pop (BRPOP)
- **Job Distribution**: Work automatically distributed across workers
- **Graceful Shutdown**: Workers handle SIGTERM properly
- **Job Types**: Different job types with different processing times

## Architecture

```
                    ┌─────────────┐
                    │    Redis    │
                    │   (queue)   │
                    └─────┬───────┘
                          │
         ┌────────────────┼────────────────┐
         │                │                │
    ┌────▼────┐      ┌────▼────┐      ┌────▼────┐
    │ Worker 1│      │ Worker 2│      │ Worker 3│
    └─────────┘      └─────────┘      └─────────┘
```

Jobs are pushed to Redis list, workers compete to process them using BRPOP.

## Files

- `tako.yaml` - Worker service (no port) + Redis
- `Dockerfile` - Node.js worker container
- `worker.js` - Background job processor
- `seed-jobs.js` - Utility to add test jobs
- `package.json` - Dependencies

## Configuration Highlights

```yaml
services:
  worker:
    build: .
    command: node worker.js     # Custom command
    replicas: 3                 # 3 worker instances
    # NO port = background worker

  redis:
    persistent: true            # Queue persists
```

Key points:
- Workers have NO `port` configuration
- Workers don't expose any HTTP endpoints
- Multiple replicas process jobs in parallel
- Redis list used as job queue

## How It Works

1. **Job Creation**: Jobs are pushed to Redis list `jobs`
2. **Worker Polling**: Workers use `BRPOP` to wait for jobs
3. **Processing**: First available worker picks up the job
4. **Completion**: Job moved to `completed` list with stats
5. **Repeat**: Worker immediately waits for next job

## Job Types

The system supports three job types:

- **email** (1s): Send email notifications
- **image** (2s): Process image files
- **report** (3s): Generate reports

## How to Deploy

1. Set server host:
   ```bash
   export SERVER_HOST=your.server.ip
   ```

2. Deploy:
   ```bash
   start deploy prod
   ```

3. Workers start immediately and wait for jobs

## Adding Jobs

**Method 1: Using seed script**
```bash
# Copy seed script to server
scp seed-jobs.js root@your-server:/tmp/

# Run on server
ssh root@your-server
docker run --rm --network tako-workers \
  -v /tmp/seed-jobs.js:/app/seed-jobs.js \
  -e REDIS_URL=redis://redis:6379 \
  node:18-alpine \
  sh -c "cd /app && npm install redis && node seed-jobs.js"
```

**Method 2: Using Redis CLI**
```bash
# Connect to Redis
docker exec -it workers-redis redis-cli

# Add a job
LPUSH jobs '{"id":1,"type":"email","data":{"recipient":"test@example.com"},"createdAt":"2024-01-01T12:00:00.000Z"}'
```

**Method 3: From your app**
```javascript
const redis = require('redis');
const client = redis.createClient({ url: 'redis://redis:6379' });

await client.lPush('jobs', JSON.stringify({
  id: Date.now(),
  type: 'email',
  data: { recipient: 'user@example.com' },
  createdAt: new Date().toISOString()
}));
```

## Monitoring Workers

**View logs:**
```bash
# All workers
docker logs workers-worker-1
docker logs workers-worker-2
docker logs workers-worker-3

# Follow logs
docker logs -f workers-worker-1
```

**Check stats:**
```bash
docker exec workers-redis redis-cli HGETALL worker:stats
# Shows jobs processed by each worker
```

**Check queue:**
```bash
docker exec workers-redis redis-cli LLEN jobs
# Number of pending jobs

docker exec workers-redis redis-cli LLEN completed
# Number of completed jobs
```

## Testing Locally

**Terminal 1 - Redis:**
```bash
docker run -d --name redis -p 6379:6379 redis:7-alpine
```

**Terminal 2, 3, 4 - Workers:**
```bash
npm install
export REDIS_URL=redis://localhost:6379
export WORKER_ID=worker-1  # Change for each terminal
npm start
```

**Terminal 5 - Seed Jobs:**
```bash
export REDIS_URL=redis://localhost:6379
npm run seed
```

Watch as the three workers compete to process jobs!

## Worker Features

**Blocking Pop:**
```javascript
const result = await client.brPop('jobs', 5);
```
- Waits up to 5 seconds for a job
- No busy polling
- Efficient resource usage

**Graceful Shutdown:**
```javascript
process.on('SIGTERM', async () => {
  await client.quit();
  process.exit(0);
});
```
- Finishes current job
- Closes Redis connection
- Exits cleanly

**Stats Tracking:**
```javascript
await client.hSet('worker:stats', WORKER_ID, jobsProcessed);
```
- Each worker tracks jobs processed
- Stored in Redis hash
- Survives worker restarts

## Scaling Workers

Need more processing power? Just increase replicas:

```yaml
services:
  worker:
    replicas: 10  # 10 workers instead of 3
```

Redeploy and you'll have 10 workers processing jobs in parallel!

## Use Cases

Background workers are perfect for:

- **Email sending**: Async email delivery
- **Image processing**: Thumbnails, resizing, optimization
- **Report generation**: PDF creation, data exports
- **Video encoding**: Format conversion, compression
- **Data synchronization**: API syncing, webhooks
- **Scheduled tasks**: Cleanup, backups, maintenance

## Job Persistence

With `persistent: true` on Redis:
- Jobs survive worker crashes
- Jobs survive redeployments
- Jobs are never lost
- Workers can restart anytime

## Monitoring in Production

Add a simple web dashboard:

```yaml
services:
  dashboard:
    build: ./dashboard
    port: 3000
    proxy:
      domains: [workers.example.com]
    env:
      REDIS_URL: redis://redis:6379
```

The dashboard can show:
- Jobs in queue
- Jobs completed
- Worker statistics
- Processing rate
- Error logs
