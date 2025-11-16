# Next.js Todo App Example

A full-stack todo application built with Next.js 15, TypeScript, Tailwind CSS, and SQLite. This example demonstrates how to deploy a complete Next.js application with a database using Tako CLI.

## Features

- Next.js 15 with App Router
- TypeScript for type safety
- Tailwind CSS for styling
- SQLite database with better-sqlite3
- RESTful API routes
- Persistent storage with Docker volumes
- Production-ready Dockerfile

## Project Structure

```
09-nextjs-todos/
├── app/
│   ├── api/todos/           # API routes for todo CRUD operations
│   │   ├── route.ts         # GET all, POST new, DELETE all
│   │   └── [id]/route.ts    # GET, PATCH, DELETE by ID
│   ├── layout.tsx           # Root layout
│   ├── page.tsx             # Main todo UI
│   └── globals.css          # Global styles
├── lib/
│   └── db.ts                # SQLite database setup and queries
├── Dockerfile               # Multi-stage Docker build
├── tako.yaml             # Tako CLI configuration
└── .env.production          # Production environment variables
```

## API Endpoints

- `GET /api/todos` - Get all todos
- `POST /api/todos` - Create a new todo
- `DELETE /api/todos` - Delete all todos
- `GET /api/todos/:id` - Get a specific todo
- `PATCH /api/todos/:id` - Update a todo (title or completed status)
- `DELETE /api/todos/:id` - Delete a specific todo

## Local Development

1. Install dependencies:
   ```bash
   npm install
   ```

2. Run development server:
   ```bash
   npm run dev
   ```

3. Open http://localhost:3000

The SQLite database will be created in `./data/todos.db`.

## Deployment with Tako CLI

1. Set your server IP in `.env` file:
   ```bash
   SERVER_IP=your.server.ip.here
   ```

2. The domain is automatically configured using sslip.io:
   ```yaml
   proxy:
     domains:
       - nextjs-todos.${SERVER_IP}.sslip.io  # Auto-resolves to your server
   ```

   This provides automatic DNS without needing to configure actual DNS records.
   Perfect for testing and development!

3. Deploy to production:
   ```bash
   SERVER_IP=your.server.ip.here ../../bin/tako.exe deploy -v
   ```

The application will be:
- Built as a Docker image
- Deployed to your configured server
- Accessible via `https://nextjs-todos.YOUR.IP.sslip.io` with automatic HTTPS
- Data persisted in a Docker volume

**Multiple Apps**: Deploy multiple apps using different subdomains:
- `nextjs-todos.YOUR.IP.sslip.io` → Todo app
- `api.YOUR.IP.sslip.io` → Backend API
- `app.YOUR.IP.sslip.io` → Frontend app

All apps route through Caddy on ports 80/443, no port numbers needed!

## Database Persistence

The todo data is stored in SQLite and persisted using Docker volumes:
- Volume name: `todos_data`
- Mount path: `/app/data`
- Database file: `/app/data/todos.db`

Data will persist across container restarts and redeployments.

## Configuration

### Environment Variables

Configured in `.env.production`:
- `NODE_ENV=production` - Sets Node environment
- `DATABASE_PATH=/app/data/todos.db` - SQLite database location

### Tako CLI Settings

Key settings in `tako.yaml`:
- **Port**: 3000 (Next.js default)
- **Replicas**: 1 (can be increased for load balancing)
- **Restart Policy**: `unless-stopped`
- **Health Check**: Monitors `/api/todos` endpoint
- **Deployment Strategy**: Rolling updates with zero downtime
- **TLS**: Automatic HTTPS with Let's Encrypt

## Tech Stack

- **Framework**: Next.js 15.1
- **Language**: TypeScript 5
- **Styling**: Tailwind CSS 4
- **Database**: SQLite (better-sqlite3)
- **Runtime**: Node.js 20
- **Deployment**: Docker + Tako CLI
