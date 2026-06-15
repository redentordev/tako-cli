# Example 08: Frontend Consumer (Cross-Project Imports)

This example demonstrates a frontend service that **imports** and consumes the backend-api service from example 07.

## Features

- **Cross-Project Import Declaration**: Imports `backend-api` through a named `backend_api` alias
- **Explicit API URL**: `BACKEND_API_URL` is used until generated import upstream wiring is enabled
- **Service Discovery Input**: The import declaration gives Tako the project, stage, service, and named port to resolve

## Prerequisites

**Deploy backend-api first!** This project imports from it:

```bash
cd examples/07-backend-api
../../bin/tako deploy
```

## Services

### web
- **Port**: 3000
- **Imports**: `backend_api` -> `backend-api/production/api:web`
- **Environment**: `API_URL=${BACKEND_API_URL}`
- **Endpoints**:
  - `GET /health` - Health check
  - `GET /users` - Fetches users from backend-api
  - `GET /products` - Fetches products from backend-api

## How It Works

1. **Export Declaration**: example 07 exports `api:web`.
2. **Import Declaration**: this project declares a top-level `backend_api` import.
3. **Endpoint Resolution**: use `tako discovery` against the backend project or a generated config artifact to get healthy private endpoints.
4. **Application Wiring**: set `BACKEND_API_URL` to the resolved backend URL for this example.

## Usage

Deploy after backend-api is running:

```bash
cd examples/08-frontend-consumer
../../bin/tako deploy -v
```

Set `BACKEND_API_URL` before deploy, for example `http://10.210.0.10:4000`
after resolving the backend API endpoint.

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

The import declaration is explicit metadata. It does not grant access to every
service in the backend project:

```
frontend
  imports backend_api -> backend-api / production / api:web
```

Only explicitly exported backend ports are intended to be resolvable. Private
services such as databases remain unexported.

## Security

- Only **exported** services are accessible
- backend-api's database remains private
- Explicit imports provide clear dependency tracking
- No accidental cross-project access
