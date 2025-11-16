# Example 4: Monorepo

This example demonstrates how to organize multiple services in a monorepo structure with separate subdirectories and build contexts.

## Features Demonstrated

- **Monorepo Structure**: Multiple services in one repository
- **Separate Build Contexts**: Each service has its own Dockerfile
- **Independent Dependencies**: Each service has its own package.json
- **Centralized Config**: Single tako.yaml in root
- **Service Communication**: Web calls API internally

## Repository Structure

```
04-monorepo/
├── tako.yaml          # Configuration for all services
├── web/                  # Web service (public)
│   ├── Dockerfile
│   ├── package.json
│   └── server.js
├── api/                  # API service (internal)
│   ├── Dockerfile
│   ├── package.json
│   └── server.js
└── README.md
```

## Configuration Highlights

```yaml
services:
  web:
    build: ./web          # Build context is web/ subdirectory
    port: 3000
    proxy:
      domains: [monorepo.example.com]

  api:
    build: ./api          # Build context is api/ subdirectory
    port: 4000
    replicas: 2
```

## How Build Contexts Work

When you specify `build: ./web`, the CLI:
1. Changes to the `web/` subdirectory
2. Looks for `Dockerfile` in that directory
3. Uses files in `web/` as build context
4. Builds the image with only those files

This means:
- Each service is isolated
- Each has its own dependencies
- Changes to one don't rebuild others
- Smaller build contexts = faster builds

## How to Deploy

1. Set server host:
   ```bash
   export SERVER_HOST=your.server.ip
   ```

2. Update domain in `tako.yaml`

3. Deploy from root directory:
   ```bash
   cd 04-monorepo
   start deploy prod
   ```

The CLI will:
- Build web service from `./web`
- Build API service from `./api`
- Deploy both services
- Configure networking

## Testing Locally

**Terminal 1 - API:**
```bash
cd api
npm install
npm start
# Running on port 4000
```

**Terminal 2 - Web:**
```bash
cd web
npm install
export API_URL=http://localhost:4000
npm start
# Running on port 3000
```

Visit `http://localhost:3000`

## Why Use Monorepo?

**Benefits:**
- **Single Repository**: All services in one place
- **Shared Commits**: Coordinate changes across services
- **Easier Development**: Clone once, see everything
- **Atomic Updates**: Deploy related changes together

**When to Use:**
- Tightly coupled services
- Shared development team
- Coordinated releases
- Small to medium projects

**When NOT to Use:**
- Independent teams
- Different release cycles
- Very large services
- Need strict separation

## Adding More Services

To add a new service:

1. Create new subdirectory:
   ```bash
   mkdir worker
   ```

2. Add Dockerfile and code to `worker/`

3. Update `tako.yaml`:
   ```yaml
   services:
     worker:
       build: ./worker
       command: npm run worker
   ```

4. Deploy:
   ```bash
   start deploy prod
   ```

## Service Communication

The web service calls the API using the service name:

```javascript
const API_URL = 'http://api:4000';
const response = await axios.get(`${API_URL}/info`);
```

Docker's internal DNS resolves `api` to the correct container(s), even with multiple replicas.

## Load Balancing

With `replicas: 2` for the API service, Docker automatically load balances requests across both instances. Each request may hit a different replica, which you can see in the hostname field.

## Development Workflow

1. Make changes in `web/` or `api/`
2. Test locally in that subdirectory
3. Deploy from root: `start deploy prod`
4. CLI builds only changed services (smart detection)
5. Services update independently

## Alternative Structure

You could also use:

```
monorepo/
├── tako.yaml
├── packages/
│   ├── web/
│   └── api/
└── shared/
    └── utils/
```

Just update the build paths:
```yaml
services:
  web:
    build: ./packages/web
```
