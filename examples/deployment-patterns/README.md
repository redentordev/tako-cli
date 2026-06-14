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
| `06-cron-runner` | Scheduled/background task runner | No public port, loop command, restart policy |
| `07-python-fastapi` | Python web service | FastAPI/Uvicorn Docker build |
| `08-go-web` | Go web service | Multi-stage Dockerfile, replicas, load balancing |

## How To Use

1. Copy a template directory into your project.
2. Set `SERVER_HOST`, `SSH_KEY`, and `LETSENCRYPT_EMAIL`.
3. Replace the project name, domain, and service settings.
4. Run `tako setup -e production`, then `tako deploy -e production --yes`.

Stateful templates use node-local volumes. Pin stateful services when moving
to multiple nodes unless the service itself is built for replication.

When multiple unrelated projects share one node, keep each template's
`project.name`, environment name, proxy domain, and named volumes unique. Tako
uses the project plus environment as the runtime boundary for containers,
networks, proxy files, state, and cleanup.
