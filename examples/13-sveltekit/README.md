# SvelteKit Example

This example demonstrates deploying a [SvelteKit](https://kit.svelte.dev) application with Tako CLI.

SvelteKit is a framework for building web applications with Svelte, featuring server-side rendering, routing, and more.

## Features

- ⚡ Lightning-fast development
- 🎯 Type-safe by default
- 🔄 Server-side rendering (SSR)
- 📦 Optimized production builds
- 🛣️ File-based routing
- 🌐 API routes

## Local Development

```bash
npm install
npm run dev
```

Visit http://localhost:5173

## Deploy with Tako CLI

```bash
# Deploy
tako deploy

# View logs
tako logs

# Check status
tako ps
```

## Project Structure

```
src/
├── routes/
│   ├── +page.svelte        # Home page
│   └── api/
│       └── hello/
│           └── +server.js  # API endpoint
└── app.html                # HTML template
```

## Learn More

- [SvelteKit Documentation](https://kit.svelte.dev/docs)
- [Svelte Documentation](https://svelte.dev)
- [Tako CLI Documentation](https://github.com/redentordev/tako-cli)
