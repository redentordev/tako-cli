# Example 08: Frontend Consumer (Cross-Project Imports)

This example demonstrates a frontend service that **imports** and consumes the backend-api service from example 07.

## Features

- **Cross-Project Imports**: Imports `backend-api.api` service
- **Automatic DNS Resolution**: Access backend via `backend-api-production-api:4000`
- **Network Bridging**: Automatically connected to the backend API export network
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
- **Environment**: `API_URL=http://backend-api-production-api:4000`
- **Endpoints**:
  - `GET /health` - Health check
  - `GET /users` - Fetches users from backend-api
  - `GET /products` - Fetches products from backend-api

## How It Works

1. **Import Declaration**: `imports: [backend-api.api]` in tako.yaml
2. **Network Bridging**: Container is connected to backend-api's service-scoped API export network
3. **DNS Resolution**: Docker DNS resolves `backend-api-production-api` inside that export network
4. **Load Balancing**: Automatic round-robin across all api replicas

## Usage

Deploy after backend-api is running:

```bash
cd examples/08-frontend-consumer
../../bin/tako deploy -v
```

Important: exported services are accessible via their readable alias:
`{project}-{environment}-{service}`. For this example, use
`http://backend-api-production-api:4000`.

## Testing

Once deployed, test cross-project communication:

```bash
# Test frontend health
curl -fsS https://frontend.yourdomain.com/health

# Test cross-project API call (frontend -> backend)
curl -fsS https://frontend.yourdomain.com/users

# Check runtime state and logs
tako ps web
tako logs --service web --tail 50
```

## Network Topology

After deployment, the frontend container is connected to TWO networks:

```
frontend_web_1:
  - tako_frontend (own project network)
  - backend-api API export network
```

This allows DNS resolution of:
- `backend-api-production-api` - load balanced across exported API replicas
- other backend-api services - not reachable unless explicitly exported and imported

## Security

- Only **exported** services are accessible
- backend-api's private services remain private
- Explicit imports provide clear dependency tracking
- No accidental cross-project access
