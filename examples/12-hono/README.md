# Hono Example

This example demonstrates deploying a [Hono](https://hono.dev) application with Tako CLI.

Hono is an ultra-fast web framework built for the Edge. It's lightweight, fast, and works on any JavaScript runtime.

## Features

- 🔥 Blazing fast performance
- 🪶 Zero dependencies core
- 🌍 Edge-ready (works on Node, Deno, Bun, Cloudflare Workers)
- 🦕 TypeScript first
- ⚡ Simple and intuitive API

## Local Development

```bash
npm install
npm run dev
```

Visit http://localhost:3000

## Deploy with Tako CLI

```bash
# Initialize (if not already done)
tako init

# Deploy
tako deploy

# View logs
tako logs

# Check status
tako ps
```

## Endpoints

- `GET /` - Welcome page
- `GET /api/hello` - JSON API endpoint
- `GET /api/status` - Health check endpoint

## Learn More

- [Hono Documentation](https://hono.dev)
- [Tako CLI Documentation](https://github.com/redentordev/tako-cli)
