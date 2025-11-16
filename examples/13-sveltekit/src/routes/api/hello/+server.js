import { json } from '@sveltejs/kit';

export function GET() {
  return json({
    message: 'Hello from SvelteKit!',
    framework: 'SvelteKit',
    deployed_with: 'Tako CLI',
    timestamp: new Date().toISOString(),
    features: [
      'Server-side rendering',
      'File-based routing',
      'API routes',
      'TypeScript support'
    ]
  });
}
