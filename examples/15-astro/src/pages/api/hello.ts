export async function GET() {
  return new Response(
    JSON.stringify({
      message: "Hello from Astro!",
      framework: "Astro",
      deployed_with: "Tako CLI",
      timestamp: new Date().toISOString(),
      features: [
        "Zero JavaScript by default",
        "Component Islands",
        "Server-side rendering",
        "Static site generation"
      ]
    }),
    {
      status: 200,
      headers: {
        "Content-Type": "application/json"
      }
    }
  );
}
