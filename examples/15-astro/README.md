# Astro Example

This example demonstrates deploying an [Astro](https://astro.build) application with Tako CLI.

Astro is a web framework for building content-driven websites with zero JavaScript by default.

## Features

- 🚀 Fast by default
- 🎯 Zero JavaScript by default
- 📦 Component Islands architecture
- 🔄 Server-side rendering (SSR)
- 🏝️ Static site generation (SSG)
- 🎨 Bring your own framework (React, Vue, Svelte, etc.)

## Local Development

```bash
npm install
npm run dev
```

Visit http://localhost:4321

## Deploy with Tako CLI

```bash
# Deploy
tako deploy

# View logs
tako logs

# Check status
tako ps
```

## Component Islands

Astro's Islands architecture allows you to use your favorite UI framework (React, Vue, Svelte, etc.) for interactive components while keeping the rest of your site static.

## Project Structure

```
src/
├── pages/
│   ├── index.astro        # Home page
│   └── api/
│       └── hello.ts       # API endpoint
└── components/            # Reusable components (optional)
```

## Learn More

- [Astro Documentation](https://docs.astro.build)
- [Astro Islands](https://docs.astro.build/en/concepts/islands/)
- [Tako CLI Documentation](https://github.com/redentordev/tako-cli)
