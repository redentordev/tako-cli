# Example 08: Frontend Consumer (Cross-Project Link)

This example demonstrates a frontend service that consumes the backend-api service from example 07.

## Features

- **Shared Backend**: example 07 exposes its primary API port with `share: true`
- **Service Link Env**: `API_URL` is resolved by Tako from the backend service link
- **Private Discovery**: the resolved URL uses Tako's private discovery path, not public DNS

## Prerequisites

**Deploy backend-api first!** This project links to it:

```bash
cd examples/07-backend-api
../../bin/tako deploy
```

## Services

### web
- **Port**: 3000
- **Link**: `API_URL` -> `backend-api / production / api`
- **Endpoints**:
  - `GET /health` - Health check
  - `GET /users` - Fetches users from backend-api
  - `GET /products` - Fetches products from backend-api

## How It Works

1. **Share Declaration**: example 07 sets `share: true` on the backend API.
2. **Env Link**: this project sets `API_URL` with a link to the backend app, stage, and service.
3. **Endpoint Resolution**: Tako resolves the shared backend endpoint during deploy and writes a normal env var into the container.

## Usage

Deploy after backend-api is running:

```bash
cd examples/08-frontend-consumer
../../bin/tako deploy -v
```

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

The service link is explicit metadata. It does not grant access to every service
in the backend project:

```
frontend
  API_URL -> backend-api / production / api
```

Only shared backend ports are intended to be resolvable. Private services such
as databases remain unshared.

## Security

- Only **shared** services are accessible
- backend-api's database remains private
- Explicit links provide clear dependency tracking
- No accidental cross-project access
