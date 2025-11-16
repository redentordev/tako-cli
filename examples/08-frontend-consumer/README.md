# Example 08: Frontend Consumer (Cross-Project Imports)

This example demonstrates a frontend service that **imports** and consumes the backend-api service from example 07.

## Features

- **Cross-Project Imports**: Imports `backend-api.api` service
- **Automatic DNS Resolution**: Access backend via `backend-api_api:4000`
- **Network Bridging**: Automatically connected to backend-api's network
- **Service Discovery**: Round-robin load balancing across backend replicas

## Prerequisites

**Deploy backend-api first!** This project imports from it:

```bash
cd examples/07-backend-api
../../bin/tako deploy
```

## Services

### web
- **Port**: 3000
- **Imports**: `backend-api.api`
- **Environment**: `API_URL=http://backend-api_api:4000`
- **Endpoints**:
  - `GET /health` - Health check
  - `GET /users` - Fetches users from backend-api
  - `GET /products` - Fetches products from backend-api

## How It Works

1. **Import Declaration**: `imports: [backend-api.api]` in tako.yaml
2. **Network Bridging**: Container is connected to `tako_backend-api_production` network
3. **DNS Resolution**: Docker DNS resolves service alias `api` to backend replicas within the imported network
4. **Load Balancing**: Automatic round-robin across all api replicas

## Usage

Deploy after backend-api is running:

```bash
cd examples/08-frontend-consumer
../../bin/tako deploy -v
```

Watch for the import connection message:
```
  Connecting to imported services...
  ✓ Connected to backend-api network
  ✓ Connected to backend-api.api (access via backend-api_production_api)
```

**Important**: Exported services are accessible via their global alias: `{project}_{environment}_{service}` (e.g., `http://backend-api_production_api:4000`).

## Testing

Once deployed, test cross-project communication:

```bash
# SSH into server
ssh root@your-server

# Test frontend health
docker exec frontend_web_1 wget -qO- http://localhost:3000/health

# Test cross-project API call (frontend -> backend)
docker exec frontend_web_1 wget -qO- http://localhost:3000/users

# Verify DNS resolution works
docker exec frontend_web_1 wget -qO- http://backend-api_api:4000/api/users
```

## Network Topology

After deployment, the frontend container is connected to TWO networks:

```
frontend_web_1:
  - tako_frontend (own project network)
  - tako_backend-api (imported network)
```

This allows DNS resolution of:
- `api` - Load balanced across all api replicas (use this in your code)
- `api_1` - Specific replica 1
- `api_2` - Specific replica 2
- `database` - ❌ NOT accessible (not exported by backend-api)

## Security

- Only **exported** services are accessible
- backend-api's database remains private
- Explicit imports provide clear dependency tracking
- No accidental cross-project access
