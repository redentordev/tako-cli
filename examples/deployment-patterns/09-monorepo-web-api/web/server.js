import http from "node:http";

const port = Number(process.env.PORT || 3000);
const apiURL = process.env.API_URL || "http://api:4000";

const server = http.createServer(async (req, res) => {
  if (req.url === "/health") {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ ok: true, service: "web" }));
    return;
  }

  if (req.url === "/api-status") {
    try {
      const response = await fetch(`${apiURL}/health`);
      res.writeHead(response.ok ? 200 : 502, { "content-type": "application/json" });
      res.end(await response.text());
    } catch (error) {
      res.writeHead(502, { "content-type": "application/json" });
      res.end(JSON.stringify({ ok: false, error: error.message }));
    }
    return;
  }

  res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
  res.end(`<!doctype html>
<html>
  <head><title>Tako Monorepo Pattern</title></head>
  <body>
    <h1>Monorepo web service</h1>
    <p>Public web service calling an internal API at <code>${apiURL}</code>.</p>
    <p><a href="/api-status">Check API status</a></p>
  </body>
</html>`);
});

server.listen(port, "0.0.0.0", () => {
  console.log(`web listening on ${port}`);
});
