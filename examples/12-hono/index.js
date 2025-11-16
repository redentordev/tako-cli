import { Hono } from 'hono'
import { serve } from '@hono/node-server'

const app = new Hono()

// Middleware for logging
app.use('*', async (c, next) => {
  console.log(`${c.req.method} ${c.req.url}`)
  await next()
})

// Routes
app.get('/', (c) => {
  return c.html(`
    <!DOCTYPE html>
    <html lang="en">
    <head>
      <meta charset="UTF-8">
      <meta name="viewport" content="width=device-width, initial-scale=1.0">
      <title>Hono on Tako CLI</title>
      <style>
        body {
          font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
          max-width: 800px;
          margin: 50px auto;
          padding: 20px;
          background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
          color: white;
        }
        .container {
          background: rgba(255, 255, 255, 0.1);
          backdrop-filter: blur(10px);
          border-radius: 20px;
          padding: 40px;
          box-shadow: 0 8px 32px rgba(0, 0, 0, 0.1);
        }
        h1 { margin-top: 0; font-size: 2.5em; }
        .badge {
          display: inline-block;
          padding: 8px 16px;
          background: rgba(255, 255, 255, 0.2);
          border-radius: 20px;
          margin: 5px;
          font-size: 0.9em;
        }
        a { color: #ffd700; text-decoration: none; }
        a:hover { text-decoration: underline; }
      </style>
    </head>
    <body>
      <div class="container">
        <h1>🔥 Hono on Tako CLI</h1>
        <p>Ultra-fast web framework for the Edge</p>
        <div>
          <span class="badge">⚡ Fast</span>
          <span class="badge">🪶 Lightweight</span>
          <span class="badge">🌍 Edge-ready</span>
          <span class="badge">🦕 TypeScript</span>
        </div>
        <h2>Features</h2>
        <ul>
          <li>Blazing fast performance</li>
          <li>Zero dependencies core</li>
          <li>Middleware support</li>
          <li>TypeScript first</li>
          <li>Works anywhere (Node, Deno, Bun, Cloudflare Workers)</li>
        </ul>
        <p>
          <a href="/api/hello">Try API endpoint →</a><br>
          <a href="https://hono.dev" target="_blank">Learn more about Hono</a>
        </p>
      </div>
    </body>
    </html>
  `)
})

app.get('/api/hello', (c) => {
  return c.json({
    message: 'Hello from Hono!',
    framework: 'Hono',
    deployed_with: 'Tako CLI',
    timestamp: new Date().toISOString()
  })
})

app.get('/api/status', (c) => {
  return c.json({
    status: 'healthy',
    uptime: process.uptime(),
    memory: process.memoryUsage(),
    version: process.version
  })
})

// 404 handler
app.notFound((c) => {
  return c.text('404 Not Found', 404)
})

const port = parseInt(process.env.PORT || '3000')
console.log(`🔥 Hono server running on http://localhost:${port}`)
serve({
  fetch: app.fetch,
  port
})
