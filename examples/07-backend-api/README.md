# Example 07: Backend API (Cross-Project Provider)

This example demonstrates a backend API service that **exports** itself for use by other projects.

## Features

- **Exported Service**: API service explicitly exports the named `web` port
- **Load Balanced**: 2 replicas with health checks
- **Private Database**: PostgreSQL database NOT exported (stays private)
- **Multi-Project**: Can be consumed by other projects via imports

## Services

### api
- **Port**: 4000
- **Replicas**: 2
- **Export**: `web:4000` (available to other projects)
- **Endpoints**:
  - `GET /health` - Health check
  - `GET /api/users` - User list
  - `GET /api/products` - Product list

### database
- **Image**: postgres:15
- **Persistent**: true
- **Export**: false (private to this project)

## Export

Other projects can declare an import for:

```yaml
imports:
  backend_api:
    project: backend-api
    environment: production
    service: api
    port: web
```

## Usage

Deploy this project first before deploying consumers:

```bash
cd examples/07-backend-api
../../bin/tako deploy
```

Then deploy a consumer project (see example 08-frontend-consumer).

## Testing

Once deployed, test the API:

```bash
# Test health check
curl -fsS https://api.yourdomain.com/health

# Test users endpoint
curl -fsS https://api.yourdomain.com/api/users

# Check runtime state and logs
tako ps api
tako logs --service api --tail 50
```
