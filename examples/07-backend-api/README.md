# Example 07: Backend API (Cross-Project Provider)

This example demonstrates a backend API service that **exports** itself for use by other projects.

## Features

- **Exported Service**: API service is marked with `export: true`
- **Load Balanced**: 2 replicas with health checks
- **Private Database**: PostgreSQL database NOT exported (stays private)
- **Multi-Project**: Can be consumed by other projects via imports

## Services

### api
- **Port**: 4000
- **Replicas**: 2
- **Export**: true (available to other projects)
- **Endpoints**:
  - `GET /health` - Health check
  - `GET /api/users` - User list
  - `GET /api/products` - Product list

### database
- **Image**: postgres:15
- **Persistent**: true
- **Export**: false (private to this project)

## DNS Resolution

Other projects can access this API via:
- `backend-api_api` - Round-robin across both replicas
- `backend-api_api_1` - Specific replica 1
- `backend-api_api_2` - Specific replica 2

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
# SSH into server
ssh root@your-server

# Test health check
docker exec backend-api_api_1 wget -qO- http://localhost:4000/health

# Test users endpoint
docker exec backend-api_api_1 wget -qO- http://localhost:4000/api/users
```
