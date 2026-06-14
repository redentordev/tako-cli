# Tako CLI Examples

Comprehensive examples demonstrating all features of the Tako CLI. Each example is fully functional and ready to deploy.

## Overview

We have ready-to-deploy examples covering common use cases:

### Deployment Pattern Templates
| Example | Description | Features |
|---------|-------------|----------|
| [deployment-patterns](./deployment-patterns/) | Copyable production shapes | Prebuilt images, static sites, APIs, volumes, databases, workers, cron-style runners, monorepos, stages, WebSockets, CI/CD, Python, Go |

### Web Frameworks & Applications
| Example | Description | Features |
|---------|-------------|----------|
| [01-simple-web](./01-simple-web/) | Basic Node.js web | Single service, public domain, HTTPS |
| [02-web-database](./02-web-database/) | Web + PostgreSQL | Database connection, persistence, service discovery |
| [03-fullstack](./03-fullstack/) | Complete stack | Web, API, PostgreSQL, Redis, internal services |
| [04-monorepo](./04-monorepo/) | Monorepo structure | Multiple services, separate builds, subdirectories |
| [09-nextjs-todos](./09-nextjs-todos/) | Next.js + SQLite | Full-stack Next.js, TypeScript, SQLite |
| [12-hono](./12-hono/) | Hono framework | Ultra-fast Edge framework |
| [13-sveltekit](./13-sveltekit/) | SvelteKit | Full-stack Svelte framework |
| [15-astro](./15-astro/) | Astro | Content-driven framework |
| [16-php](./16-php/) | Vanilla PHP 8.3 | Pure PHP application |
| [17-laravel](./17-laravel/) | Laravel | PHP framework |
| [18-rails](./18-rails/) | Ruby on Rails | Rails application |

### Scaling & Infrastructure
| Example | Description | Features |
|---------|-------------|----------|
| [05-workers](./05-workers/) | Background workers | Job processing, Redis queue, worker replicas |
| [06-scaling](./06-scaling/) | Load balancing | Multiple replicas, round-robin, scaling |
| [07-backend-api](./07-backend-api/) | RESTful API | Export services for other projects |
| [08-frontend-consumer](./08-frontend-consumer/) | Frontend consumer | Import and use services from other projects |

### Third-Party Applications
| Example | Description | Features |
|---------|-------------|----------|
| [17-n8n](./17-n8n/) | n8n | Workflow automation platform |
| [18-plausible](./18-plausible/) | Plausible Analytics | Privacy-friendly web analytics |
| [19-umami](./19-umami/) | Umami Analytics | Simple, fast web analytics |
| [20-ghost](./20-ghost/) | Ghost CMS | Headless CMS for content |

### Testing & Advanced Examples
| Example | Description | Features |
|---------|-------------|----------|
| [test-parallel](./test-parallel/) | Parallel deployment | Test concurrent service deployment |
| [test-placement-strategies](./test-placement-strategies/) | Placement strategies | takod node placement testing |
| [test-secrets](./test-secrets/) | Secrets management | Secure environment variables |

## Quick Start

Each example follows this structure:

```
example-name/
├── tako.yaml            # Deployment configuration
├── Dockerfile(s)         # Container setup
├── package.json          # Dependencies
├── source files          # Application code
└── README.md             # Detailed documentation
```

### Deploy Any Example

1. **Set your server host:**
   ```bash
   export SERVER_HOST=your.server.ip
   ```

2. **Navigate to example:**
   ```bash
   cd examples/01-simple-web
   ```

3. **Update domain in tako.yaml:**
   ```yaml
   proxy:
     domain: your-domain.com
   ```

4. **Deploy:**
   ```bash
   tako deploy -e production
   ```

## Example Progression

The examples are ordered by complexity - start with Example 1 and work your way up:

### 1. Simple Web (Start Here!)
**Level:** Beginner
**Time:** 5 minutes

Learn the basics:
- Single service deployment
- Public domain configuration
- Automatic HTTPS with Let's Encrypt

Perfect first example to understand Tako CLI fundamentals.

### 2. Web + Database
**Level:** Beginner
**Time:** 10 minutes

Add persistence:
- PostgreSQL database
- Service-to-service communication
- Persistent volumes
- Environment variables

Learn how services talk to each other internally.

### 3. Full-Stack
**Level:** Intermediate
**Time:** 15 minutes

Complete architecture:
- Public web frontend
- Internal API backend
- Database and cache layer
- Multiple replicas
- Health monitoring

See how a real production app is structured.

### 4. Monorepo
**Level:** Intermediate
**Time:** 10 minutes

Organize your code:
- Services in subdirectories
- Separate build contexts
- Centralized configuration
- Independent deployments

Great for teams with multiple services.

### 5. Workers
**Level:** Intermediate
**Time:** 15 minutes

Background processing:
- Worker services (no ports)
- Redis job queue
- Multiple worker instances
- Job distribution
- Graceful shutdown

Perfect for async tasks like email, image processing, etc.

### 6. Scaling
**Level:** Intermediate
**Time:** 10 minutes

Handle traffic:
- Multiple service replicas
- Load balancing strategies
- Zero downtime deployments
- High availability

Learn to scale horizontally.

### 7. Backend API (Cross-Project Provider)
**Level:** Advanced
**Time:** 10 minutes

Export services:
- Make services available to other projects
- Multiple replicas with load balancing
- REST API endpoints
- Health checks

Learn to create reusable backend services.

### 8. Frontend Consumer (Cross-Project Import)
**Level:** Advanced
**Time:** 10 minutes

Import services:
- Consume services from other projects
- Automatic network bridging
- DNS-based service discovery
- Seamless cross-project communication

Learn to build microservices architecture across projects.

## Features Matrix

| Feature | 01 | 02 | 03 | 04 | 05 | 06 | 07 | 08 |
|---------|----|----|----|----|----|----|----|----|
| Public Web | ✓ | ✓ | ✓ | ✓ | | ✓ | | |
| Internal API | | | ✓ | ✓ | | | ✓ | |
| Database | | ✓ | ✓ | | | | | |
| Redis | | | ✓ | | ✓ | | | |
| Workers | | | | | ✓ | | | |
| Replicas | | | ✓ | ✓ | ✓ | ✓ | ✓ | |
| Load Balancing | | | | | | ✓ | ✓ | |
| Persistence | | ✓ | ✓ | | ✓ | | | |
| Monorepo | | | | ✓ | | | | |
| Cross-Project | | | | | | | ✓ | ✓ |

## Configuration Patterns

### Public Service (Accessible via Domain)
```yaml
services:
  web:
    build: .
    port: 3000
    proxy:
      domain: example.com
      email: admin@example.com
```

### Internal Service (Not Exposed)
```yaml
services:
  api:
    build: ./api
    port: 4000
    # No proxy = internal only
```

### Worker Service (No Port)
```yaml
services:
  worker:
    build: ./worker
    command: npm run worker
    # No port = background worker
```

### Database/Cache (Persistent)
```yaml
services:
  postgres:
    image: postgres:15
    persistent: true
    volumes:
      - /var/lib/postgresql/data
```

### Scaled Service (Multiple Replicas)
```yaml
services:
  api:
    build: .
    port: 4000
    replicas: 3
    loadBalancer:
      strategy: round_robin
```

### Cross-Project Service Export
```yaml
services:
  api:
    build: .
    port: 4000
    export: true  # Make available to other projects
```

### Cross-Project Service Import
```yaml
services:
  web:
    build: .
    port: 3000
    imports:
      - backend-api.api  # Import from another project
    env:
      API_URL: http://backend-api_api:4000  # DNS works!
```

## Common Use Cases

### Static Website
Use: **01-simple-web**
Modify: Remove Express, serve static files

### REST API
Use: **03-fullstack** (API service)
Modify: Remove web service, add database

### Full-Stack App
Use: **03-fullstack**
Modify: Adjust replicas based on load

### Microservices
Use: **04-monorepo**
Modify: Add more services in subdirectories

### Job Processing System
Use: **05-workers**
Modify: Add dashboard service to monitor jobs

### High-Traffic Website
Use: **06-scaling**
Modify: Increase replicas, add caching

### Microservices Architecture
Use: **07-backend-api + 08-frontend-consumer**
Modify: Create multiple backend services, import as needed

## Testing Locally

All examples can be tested locally before deployment:

```bash
# Navigate to example
cd examples/01-simple-web

# Install dependencies
npm install

# Set environment variables
export DATABASE_URL=postgresql://localhost/mydb
export REDIS_URL=redis://localhost:6379

# Run the application
npm start

# Visit http://localhost:3000
```

For services requiring databases, use your normal local Postgres/Redis setup
while developing. After deployment, validate runtime state through Tako:

```bash
tako ps
tako logs --service web --tail 50
```

## Environment Variables

All examples use `${SERVER_HOST}` for the server configuration:

```yaml
servers:
  prod:
    host: ${SERVER_HOST}
    user: root
```

Set it before deploying:
```bash
export SERVER_HOST=46.62.254.8
```

You can also set it in a `.env` file:
```bash
SERVER_HOST=46.62.254.8
```

## Customizing Examples

### Change Port
```yaml
services:
  web:
    port: 8080  # Change from 3000 to 8080
```

### Add Environment Variable
```yaml
services:
  web:
    env:
      MY_VARIABLE: my-value
      API_KEY: ${API_KEY}  # From environment
```

### Add Volume
```yaml
services:
  web:
    volumes:
      - /app/uploads
      - /app/logs
```

### Add Replicas
```yaml
services:
  web:
    replicas: 5  # Scale to 5 instances
```

### Run Startup Tasks
```yaml
services:
  web:
    command: sh -c "npm run migrate && npm start"
```

## Troubleshooting

### Service Not Starting
```bash
# Check logs
tako logs --service web

# Check takod runtime state
tako ps

# Check node and project health
tako doctor
```

### Domain Not Resolving
1. Verify DNS points to your server IP
2. Wait for DNS propagation (up to 48 hours)
3. Check access logs with `tako access`

### Database Connection Failed
1. Check service is running: `tako ps`
2. Verify connection string in env vars
3. Check recent service logs: `tako logs --service postgres`

### Can't Access Internal Service
- Internal services are not exposed to internet (by design)
- Access only from other services: `http://service-name:port`
- Don't add `proxy` config to internal services

## Production Checklist

Before deploying to production:

- [ ] Update all domains in `tako.yaml`
- [ ] Set strong database passwords
- [ ] Configure email for SSL certificates
- [ ] Set `NODE_ENV=production`
- [ ] Enable persistent volumes for databases
- [ ] Configure backup schedules
- [ ] Test health check endpoints
- [ ] Set appropriate replica counts
- [ ] Review resource limits
- [ ] Set up monitoring and logging

## Next Steps

1. **Start with 01-simple-web** to learn the basics
2. **Progress through examples** in order
3. **Mix and match** patterns for your needs
4. **Read the main documentation** for advanced features
5. **Deploy to production** with confidence!

## Getting Help

- Read individual example READMEs for details
- Check the main Tako CLI documentation
- Review configuration reference
- Open issues on GitHub

## Contributing

Have a great example to add? We'd love to see:
- React/Vue/Angular SPAs
- GraphQL APIs
- Django applications
- Message queues beyond Redis
- Object storage patterns
- Multi-region application shapes

Submit a pull request with your example!

## License

All examples are provided as-is for learning and reference purposes.
