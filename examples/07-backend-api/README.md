# Example 07: Backend API (Cross-Project Provider)

This example demonstrates a backend API service that **shares** itself for use by other projects.

## Features

- **Shared Service**: API service explicitly shares its primary port
- **Load Balanced**: 2 replicas with health checks
- **Private Database**: PostgreSQL database NOT exported (stays private)
- **Multi-Project**: Can be consumed by other projects via service links

## Services

### api
- **Port**: 4000
- **Replicas**: 2
- **Share**: primary port 4000 is available to linked projects
- **Endpoints**:
  - `GET /health` - Health check
  - `GET /api/users` - User list
  - `GET /api/products` - Product list

### database
- **Image**: postgres:15
- **Persistent**: true
- **Export**: false (private to this project)

## Share

Other projects can link to this shared service:

```yaml
services:
  web:
    env:
      API_URL:
        link:
          app: backend-api
          stage: production
          service: api
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
