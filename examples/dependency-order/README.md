# Dependency Order

This example shows how Tako orders a multi-service deploy with `dependsOn`:

- Services deploy **sequentially**, one at a time, in dependency order
- A service starts rolling out only after every service it depends on has finished
- Within a single service, the rollout to each node runs concurrently
- A failed service stops the deploy before any of its dependents are touched

## Architecture

This example deploys a multi-service application with the following dependency structure:

```
No dependencies:
  - postgres (database)
  - redis (cache)

Depends on postgres and redis:
  - api
  - worker

Depends on api:
  - web
```

## Deploy Order

Tako resolves `dependsOn` into a single, deterministic order. Services that
become ready at the same time are ordered by name:

```
postgres → redis → api → worker → web
```

1. **postgres** and **redis** have no dependencies, so they go first
2. **api** and **worker** follow once both of their dependencies are deployed
3. **web** deploys last, after **api**

Each service finishes its rollout, including health checks, before the next
service starts. If a service fails, the remaining services are not deployed.

## Configuration

Ordering comes entirely from `dependsOn` on each service:

```yaml
environments:
  production:
    services:
      api:
        build: ./api
        dependsOn:
          - postgres
          - redis
      web:
        build: ./web
        dependsOn:
          - api
```

There is no separate switch for ordering or concurrency. Rollout behavior for a
single service (for example rolling or blue/green) is set with
`services.<name>.deploy.strategy`.

## Setup

1. Copy `.env.example` to `.env` and set `SERVER_HOST` and `POSTGRES_PASSWORD`.

2. Check the server configuration in `tako.yaml`:
   ```yaml
   servers:
     production:
       host: ${SERVER_HOST}
       user: root
       sshKey: ~/.ssh/id_ed25519
   ```

3. Each build-backed service (`api`, `web`, `worker`) has a minimal Dockerfile
   in its own directory, so the example deploys without a real application.

## Validate and Deploy

```bash
# Check the config without contacting servers
tako validate

# Deploy all services in dependency order
tako deploy -v
```

The verbose output lists each service as it starts and finishes, in the order
shown above.

## Key Features Demonstrated

1. **Dependency ordering**: `dependsOn` decides which services deploy first
2. **Deterministic order**: ties are broken by service name, so every deploy uses the same order
3. **Fail fast**: a failed service stops the deploy before its dependents change
4. **Per-node concurrency**: on multi-server environments, each service rolls out to its nodes concurrently

## Troubleshooting

### A service starts before its dependency is ready
- Check that the dependency is listed in the service's `dependsOn`
- Add a `healthCheck` to the dependency so its rollout only completes once it is healthy

### Circular dependency errors
- `tako deploy` refuses to start when services depend on each other in a loop; remove one `dependsOn` edge of the cycle

### Deployment failures
- Ensure health check endpoints are working
- Check service logs: `tako logs <service>`
- Verify network connectivity between services

## Next Steps

- Add more complex dependency chains
- Test with real applications from examples/02-web-database or examples/05-workers
