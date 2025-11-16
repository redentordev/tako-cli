import { createSignal } from "solid-js";
import { Title } from "@solidjs/meta";

export default function Home() {
  const [count, setCount] = createSignal(0);

  return (
    <main>
      <Title>SolidStart on Tako CLI</Title>
      <div class="container">
        <h1>⚡ SolidStart on Tako CLI</h1>
        <p>The Shape of Frameworks to Come</p>

        <div class="badges">
          <span class="badge">⚡ Blazing Fast</span>
          <span class="badge">🎯 Fine-grained</span>
          <span class="badge">🔄 SSR</span>
          <span class="badge">📦 No VDOM</span>
        </div>

        <div class="counter">
          <h2>Interactive Counter</h2>
          <p class="count">{count()}</p>
          <button onClick={() => setCount(count() + 1)}>Increment</button>
          <button onClick={() => setCount(count() - 1)}>Decrement</button>
          <button onClick={() => setCount(0)}>Reset</button>
        </div>

        <div class="features">
          <h2>Features</h2>
          <ul>
            <li>Fine-grained reactivity (no Virtual DOM)</li>
            <li>Server-side rendering (SSR)</li>
            <li>File-based routing</li>
            <li>API routes</li>
            <li>TypeScript support</li>
            <li>Truly reactive - no re-renders</li>
          </ul>
        </div>

        <div class="links">
          <a href="/api/hello">API Example</a>
          <a href="https://start.solidjs.com" target="_blank">SolidStart Docs</a>
        </div>
      </div>
    </main>
  );
}
