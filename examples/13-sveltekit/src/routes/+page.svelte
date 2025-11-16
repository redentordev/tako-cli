<script>
  let count = 0;

  function increment() {
    count += 1;
  }
</script>

<svelte:head>
  <title>SvelteKit on Tako CLI</title>
</svelte:head>

<div class="container">
  <h1>🔶 SvelteKit on Tako CLI</h1>
  <p>The fastest way to build web apps of all sizes</p>

  <div class="badges">
    <span class="badge">⚡ Fast</span>
    <span class="badge">🎯 Type-safe</span>
    <span class="badge">🔄 SSR</span>
    <span class="badge">📦 Optimized</span>
  </div>

  <div class="counter">
    <h2>Interactive Counter</h2>
    <p class="count">{count}</p>
    <button on:click={increment}>Increment</button>
  </div>

  <div class="features">
    <h2>Features</h2>
    <ul>
      <li>Server-side rendering (SSR)</li>
      <li>File-based routing</li>
      <li>API routes</li>
      <li>Zero-config deployment</li>
      <li>Built-in TypeScript support</li>
      <li>Optimized production builds</li>
    </ul>
  </div>

  <div class="links">
    <a href="/api/hello">API Example</a>
    <a href="https://kit.svelte.dev" target="_blank">SvelteKit Docs</a>
  </div>
</div>

<style>
  :global(body) {
    margin: 0;
    font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
    background: linear-gradient(135deg, #ff3e00 0%, #ff8a00 100%);
    min-height: 100vh;
    display: flex;
    justify-content: center;
    align-items: center;
  }

  .container {
    background: rgba(255, 255, 255, 0.95);
    border-radius: 20px;
    padding: 40px;
    max-width: 600px;
    box-shadow: 0 20px 60px rgba(0, 0, 0, 0.3);
  }

  h1 {
    margin-top: 0;
    color: #ff3e00;
    font-size: 2.5em;
  }

  .badges {
    margin: 20px 0;
  }

  .badge {
    display: inline-block;
    padding: 8px 16px;
    background: #ff3e00;
    color: white;
    border-radius: 20px;
    margin: 5px;
    font-size: 0.9em;
  }

  .counter {
    background: #f0f0f0;
    padding: 30px;
    border-radius: 15px;
    text-align: center;
    margin: 30px 0;
  }

  .count {
    font-size: 4em;
    font-weight: bold;
    color: #ff3e00;
    margin: 20px 0;
  }

  button {
    background: #ff3e00;
    color: white;
    border: none;
    padding: 15px 40px;
    font-size: 1.1em;
    border-radius: 10px;
    cursor: pointer;
    transition: transform 0.2s, box-shadow 0.2s;
  }

  button:hover {
    transform: translateY(-2px);
    box-shadow: 0 5px 20px rgba(255, 62, 0, 0.4);
  }

  button:active {
    transform: translateY(0);
  }

  .features ul {
    list-style: none;
    padding: 0;
  }

  .features li {
    padding: 10px 0;
    border-bottom: 1px solid #eee;
  }

  .features li:before {
    content: "✓ ";
    color: #ff3e00;
    font-weight: bold;
    margin-right: 10px;
  }

  .links {
    margin-top: 30px;
    text-align: center;
  }

  a {
    display: inline-block;
    color: #ff3e00;
    text-decoration: none;
    margin: 0 15px;
    padding: 10px 20px;
    border: 2px solid #ff3e00;
    border-radius: 8px;
    transition: all 0.2s;
  }

  a:hover {
    background: #ff3e00;
    color: white;
  }
</style>
