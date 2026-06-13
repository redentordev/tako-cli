# SolidStart Example

This example demonstrates deploying a [SolidStart](https://start.solidjs.com) application with Tako CLI.

SolidStart is a meta-framework built on SolidJS, featuring fine-grained reactivity without a Virtual DOM.

## Features

- ⚡ Blazing fast performance
- 🎯 Fine-grained reactivity
- 🔄 Server-side rendering (SSR)
- 📦 No Virtual DOM overhead
- 🛣️ File-based routing
- 🌐 API routes
- 🦕 TypeScript support

## Local Development

```bash
npm install
npm run dev
```

Visit http://localhost:3000

## Deploy with Tako CLI

```bash
# Deploy
tako deploy

# View logs
tako logs --service web

# Check status
tako ps
```

## Why SolidJS?

SolidJS offers true reactivity without the overhead of a Virtual DOM. Updates are surgical and precise, leading to exceptional performance.

## Project Structure

```
src/
├── routes/
│   ├── index.tsx          # Home page
│   └── api/
│       └── hello.ts       # API endpoint
├── app.tsx                # Root component
├── entry-server.tsx       # Server entry
└── entry-client.tsx       # Client entry
```

## Learn More

- [SolidStart Documentation](https://start.solidjs.com)
- [SolidJS Documentation](https://solidjs.com)
- [Tako CLI Documentation](https://github.com/redentordev/tako-cli)
