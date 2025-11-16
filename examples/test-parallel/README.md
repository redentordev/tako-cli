# Test Parallel Deployment

This example demonstrates Tako's parallel deployment capabilities with:
- Dependency-aware service grouping
- Concurrent builds and deployments
- Docker BuildKit caching
- Performance metrics

## Architecture

This example deploys a multi-service application with the following dependency structure:

```
Level 0 (No dependencies):
  - postgres (database)
  - redis (cache)

Level 1 (Depends on Level 0):
  - api (depends on postgres, redis)
  - worker (depends on postgres, redis)

Level 2 (Depends on Level 1):
  - web (depends on api)
```

## Deployment Strategy

When using parallel deployment:

1. **Level 0** services (postgres, redis) are built and deployed **in parallel**
2. Wait for Level 0 health checks to pass
3. **Level 1** services (api, worker) are built and deployed **in parallel**
4. Wait for Level 1 health checks to pass
5. **Level 2** services (web) are deployed

## Performance Benefits

### Sequential Deployment (traditional):
```
Total Time = postgres + redis + api + worker + web
Estimated: ~10 minutes (2 min per service)
```

### Parallel Deployment (with Tako):
```
Level 0: max(postgres, redis) = ~2 minutes (parallel)
Level 1: max(api, worker) = ~2 minutes (parallel)
Level 2: web = ~2 minutes
Total Time: ~6 minutes (40% faster)
```

## Configuration

The parallel deployment is configured in `tako.yaml`:

```yaml
deployment:
  strategy: parallel
  parallel:
    maxConcurrentBuilds: 4
    maxConcurrentDeploys: 4
    strategy: dependency-aware
  cache:
    enabled: true
    type: local
    retention: 7d
```

## Setup

1. Update server configuration in `tako.yaml`:
   ```yaml
   servers:
     production:
       host: YOUR_SERVER_IP
       user: root
       sshKey: ~/.ssh/id_rsa
   ```

2. Create the application directories:
   ```bash
   mkdir -p api web worker
   ```

3. Create simple Dockerfiles for each service (examples provided below)

## Quick Test

To test the parallel deployment without real applications, you can use simple test services:

```bash
# Create api/Dockerfile
cat > api/Dockerfile << 'EOF'
FROM node:18-alpine
WORKDIR /app
RUN echo 'const http = require("http"); const server = http.createServer((req, res) => { if (req.url === "/health") { res.writeHead(200); res.end("OK"); } else { res.writeHead(200); res.end("API"); } }); server.listen(3000);' > index.js
CMD ["node", "index.js"]
EOF

# Create web/Dockerfile
cat > web/Dockerfile << 'EOF'
FROM node:18-alpine
WORKDIR /app
RUN echo 'const http = require("http"); const server = http.createServer((req, res) => { res.writeHead(200); res.end("Web"); }); server.listen(3001);' > index.js
CMD ["node", "index.js"]
EOF

# Create worker/Dockerfile
cat > worker/Dockerfile << 'EOF'
FROM node:18-alpine
WORKDIR /app
RUN echo 'console.log("Worker started"); setInterval(() => console.log("Processing..."), 5000);' > worker.js
CMD ["node", "worker.js"]
EOF
```

## Deploy

```bash
# Deploy with parallel execution
tako deploy -v

# The output will show:
# - Parallel build groups
# - Concurrent build execution
# - Deployment levels
# - Performance metrics
```

## Expected Output

```
=== Starting Orchestrated Deployment ===
  Services: 5
  Max Parallel Builds: 4
  Max Parallel Deploys: 4

=== Parallel Build Groups ===
  Level 0 (2 services in parallel): [postgres redis]
  Level 1 (2 services in parallel): [api worker]
  Level 2 (1 services in parallel): [web]

→ Stage: build
  → Building level 0 (2 services)...
    → Building postgres...
    ✓ Skipping build for postgres (using postgres:15-alpine)
    → Building redis...
    ✓ Skipping build for redis (using redis:7-alpine)
  
  → Building level 1 (2 services)...
    → Building api...
    ✓ Built api in 45.2s
    → Building worker...
    ✓ Built worker in 38.7s
  
  → Building level 2 (1 services)...
    → Building web...
    ✓ Built web in 42.1s

  ✓ All builds completed in 126.0s

→ Stage: deploy
  → Deploying level 0 (2 services)...
    → Deploying postgres...
    ✓ Deployed postgres
    → Deploying redis...
    ✓ Deployed redis
  
  → Deploying level 1 (2 services)...
    → Deploying api...
    ✓ Deployed api
    → Deploying worker...
    ✓ Deployed worker
  
  → Deploying level 2 (1 services)...
    → Deploying web...
    ✓ Deployed web

  ✓ All services deployed in 87.5s

=== Deployment Summary ===
  Total Duration:      213.5s
  Build Duration:      126.0s
  Deploy Duration:     87.5s
  Services Deployed:   5
  Failures:            0
  Parallel Speedup:    2.1x
```

## Comparing with Sequential Deployment

To compare with sequential deployment, change the strategy:

```yaml
deployment:
  strategy: sequential  # or omit the deployment section entirely
```

Sequential deployment will process services one at a time:
```
postgres → redis → api → worker → web
Estimated: ~350 seconds (vs 213 seconds parallel)
```

## Key Features Demonstrated

1. **Dependency-Aware Grouping**: Services are automatically grouped by dependency level
2. **Parallel Execution**: Multiple services build/deploy simultaneously within each level
3. **Semaphore Control**: Limited concurrency prevents resource exhaustion
4. **Health Check Integration**: Each level waits for health checks before proceeding
5. **Metrics Collection**: Performance statistics are tracked and displayed

## Troubleshooting

### All services building sequentially
- Check that `deployment.strategy: parallel` is set in tako.yaml
- Verify dependencies are properly defined with `dependsOn`

### Build failures
- Increase `maxConcurrentBuilds` if resource constrained
- Check individual service logs
- Verify Dockerfiles exist in build paths

### Deployment failures
- Ensure health check endpoints are working
- Check service logs: `tako logs <service>`
- Verify network connectivity between services

## Next Steps

- Try increasing `maxConcurrentBuilds` for more parallelization
- Add more complex dependency chains
- Measure actual time savings vs sequential deployment
- Test with real applications from examples/02-web-database or examples/05-workers
