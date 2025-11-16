import { json } from "@solidjs/router";

export function GET() {
  return json({
    message: "Hello from SolidStart!",
    framework: "SolidStart",
    deployed_with: "Tako CLI",
    timestamp: new Date().toISOString(),
    features: [
      "Fine-grained reactivity",
      "No Virtual DOM",
      "Server-side rendering",
      "File-based routing"
    ]
  });
}
