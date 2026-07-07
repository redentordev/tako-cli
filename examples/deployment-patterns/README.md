# Deployment Pattern Templates

Small, copyable templates for common Tako deployments. Each template is kept
minimal so the deployment shape is easy to see and adapt.

## Patterns

| Template | Use case | Main features |
| --- | --- | --- |
| `00-prebuilt-image` | Run an existing Docker image | `image`, public proxy, no build context |
| `01-static-site` | Static HTML site | Nginx Dockerfile, static assets, HTTPS |
| `02-node-api` | Dynamic Node.js API | Docker build, health check, replicas |
| `03-volume-sqlite` | App-owned local data | Named volume, backup schedule |
| `04-postgres-app` | Web app with database | App service, PostgreSQL, persistence, dependency |
| `05-workers-redis` | API plus background workers | Web/API, worker command, Redis queue |
| `06-cron-runner` | Scheduled job on a cron schedule | `kind: job`, cron `schedule`, run history via `tako jobs runs` |
| `07-python-fastapi` | Python web service | FastAPI/Uvicorn Docker build |
| `08-go-web` | Go web service | Multi-stage Dockerfile, replicas, load balancing |
| `09-monorepo-web-api` | Monorepo with public web and internal API | Separate build contexts, dependency order, internal routing |
| `10-stages-shared-node` | Preview and production on one node | Same project, separate environments, distinct domains |
| `11-websocket-node` | Realtime Node.js service | WebSocket endpoint, replicas, sticky balancing |
| `12-github-actions-deploy` | CI/CD deployment | GitHub Actions, state restore, noninteractive deploy |

## How To Use

1. Copy a template directory into your project.
2. Set `SERVER_HOST`, `SSH_KEY`, and `LETSENCRYPT_EMAIL`.
3. Replace the project name, domain, and service settings.
4. Run `tako setup -e production`, then `tako deploy -e production --yes`.

## Validate The Catalog

Run the local validation command after changing these templates:

```sh
make examples
```

This validates every example config in the repository, checks build contexts,
verifies the CI/CD example, and runs the deployment pattern assertions without
contacting a real server. To validate only this catalog, run
`examples/deployment-patterns/validate.sh`.

Stateful templates use node-local volumes. Pin stateful services when moving
to multiple nodes unless the service itself is built for replication.

When multiple unrelated projects share one node, keep each template's
`project.name`, environment name, proxy domain, and named volumes unique. Tako
uses the project plus environment as the runtime boundary for containers,
networks, proxy files, state, and cleanup. Public proxy upstreams use scoped
container aliases instead of generic names like `web`, so examples can reuse
common service names without cross-project routing conflicts.
