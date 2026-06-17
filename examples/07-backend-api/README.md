# Example 07: Backend API (Cross-Project Provider)

This example demonstrates a backend API service that **exports** itself for use by other projects.

## Features

- **Exported Service**: API service is marked with `export: true`
- **Load Balanced**: 2 replicas with health checks
- **Service-Scoped Export Network**: Only the API service is attached to its export network
- **Multi-Project**: Can be consumed by other projects via explicit imports

## Services

### api
- **Port**: 4000
- **Replicas**: 2
- **Export**: true (available to other projects)
- **Endpoints**:
  - `GET /health` - Health check
  - `GET /api/users` - User list
  - `GET /api/products` - Product list

## DNS Resolution

Other projects can access this API via:
- `backend-api-production-api` - readable export alias for the production API service

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
