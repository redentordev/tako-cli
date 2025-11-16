# Example 3: Full-Stack Application

This example demonstrates a complete full-stack architecture with public web frontend, internal API, database, and cache layer.

## Architecture

```
Internet
   ↓
┌─────────────────┐
│  WEB (public)   │ → Port 3000 → Accessible via domain
└────────┬────────┘
         ↓ HTTP calls
┌─────────────────┐
│  API (internal) │ → Port 4000 → Internal only (2 replicas)
└────┬─────┬──────┘
     ↓     ↓
┌────────┐ ┌──────┐
│POSTGRES│ │REDIS │ → Persistent storage
└────────┘ └──────┘
```

## Features Demonstrated

- **Public Service**: Web frontend accessible via domain
- **Internal Service**: API not exposed to internet
- **Service Discovery**: Web calls API using service name `api`
- **Load Balancing**: API runs with 2 replicas
- **Database**: PostgreSQL for persistent data
- **Caching**: Redis for performance optimization
- **Health Checks**: Multi-layer health monitoring

## Files

```
03-fullstack/
├── tako.yaml          # 4 services configuration
├── web/
│   ├── Dockerfile
│   ├── package.json
│   └── index.js         # Frontend server
├── api/
│   ├── Dockerfile
│   ├── package.json
│   └── api.js           # Backend API
└── README.md
```

## Configuration Highlights

```yaml
services:
  web:
    proxy:
      domains: [fullstack.example.com]  # Public
    env:
      API_URL: http://api:4000          # Internal communication

  api:
    port: 4000
    replicas: 2                          # Load balanced
    # No proxy = internal only

  postgres:
    persistent: true                     # Data persists

  redis:
    persistent: true                     # Cache persists
```

## How It Works

1. **User Request**: Browser → `fullstack.example.com`
2. **Web Layer**: Caddy proxy → Web container
3. **API Call**: Web → `http://api:4000` (internal network)
4. **Load Balance**: Request distributed across 2 API replicas
5. **Data Layer**: API → PostgreSQL (reads/writes) + Redis (cache)

## Data Flow

**First Request:**
```
User → Web → API → Check Redis (miss) → Query PostgreSQL → Cache in Redis → Return
```

**Cached Request:**
```
User → Web → API → Check Redis (hit) → Return cached data
```

## Deployment Ordering

Tako CLI automatically detects and resolves service dependencies to ensure proper startup order:

### Automatic Dependency Detection

The CLI analyzes environment variables to infer dependencies:

```yaml
api:
  env:
    DATABASE_URL: postgresql://postgres:secret123@postgres:5432/appdb
    REDIS_URL: redis://redis:6379
# Automatically infers: api depends on [postgres, redis]

web:
  env:
    API_URL: http://api:4000
# Automatically infers: web depends on [api]
```

**Deployment Order (Auto-detected):**
1. `postgres` (no dependencies)
2. `redis` (no dependencies)
3. `api` (depends on postgres, redis)
4. `web` (depends on api)

### Explicit Dependencies (Optional)

You can also explicitly define dependencies:

```yaml
services:
  web:
    dependsOn:
      - api
  api:
    dependsOn:
      - postgres
      - redis
```

Explicit dependencies override automatic detection.

### Supported Patterns

The dependency resolver detects these patterns in environment variables:
- `SERVICE_URL` (e.g., `API_URL: http://api:4000`)
- `SERVICE_HOST` (e.g., `REDIS_HOST: redis`)
- `://<service>` (e.g., `redis://redis:6379`)
- `@<service>:` (e.g., `user:pass@postgres:5432`)

## How to Deploy

1. Set server host:
   ```bash
   export SERVER_HOST=your.server.ip
   ```

2. Update domain in `tako.yaml`

3. Deploy all services:
   ```bash
   tako deploy --server prod
   ```

4. The CLI will:
   - Analyze service dependencies
   - Deploy services in correct order:
     1. PostgreSQL and Redis (databases/caches first)
     2. API service (waits for databases)
     3. Web service (waits for API)
   - Build web and api images
   - Configure Traefik proxy
   - Set up internal networking
   - Verify health checks

## Testing Locally

**Terminal 1 - PostgreSQL:**
```bash
docker run -d --name postgres \
  -e POSTGRES_DB=appdb \
  -e POSTGRES_USER=postgres \
  -e POSTGRES_PASSWORD=secret123 \
  -p 5432:5432 postgres:15
```

**Terminal 2 - Redis:**
```bash
docker run -d --name redis -p 6379:6379 redis:7-alpine
```

**Terminal 3 - API:**
```bash
cd api
npm install
export DATABASE_URL=postgresql://postgres:secret123@localhost:5432/appdb
export REDIS_URL=redis://localhost:6379
npm start
```

**Terminal 4 - Web:**
```bash
cd web
npm install
export API_URL=http://localhost:4000
npm start
```

Visit `http://localhost:3000`

## API Endpoints

### Internal API (port 4000)

- `POST /visits` - Record a page visit
- `GET /stats` - Get statistics (cached)
- `GET /health` - Health check

### Web (port 3000)

- `GET /` - Main page with statistics
- `GET /health` - Health check (includes API status)

## Security

The API service has NO proxy configuration, making it:
- Not accessible from the internet
- Only accessible to other services in the Docker network
- Secure by default

External requests to `your-server:4000` will fail, but internal requests to `http://api:4000` work perfectly.

## Performance Features

- **Caching**: Stats cached in Redis for 5 seconds
- **Connection Pooling**: PostgreSQL connection pool
- **Load Balancing**: 2 API replicas distribute load
- **Health Monitoring**: Multi-layer health checks

## Monitoring

Check health status:
```bash
curl https://fullstack.example.com/health
```

Returns:
```json
{
  "status": "healthy",
  "web": "ok",
  "api": {
    "status": "healthy",
    "database": "connected",
    "redis": "connected"
  }
}
```
