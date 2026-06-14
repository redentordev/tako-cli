import http from "node:http";

const port = Number(process.env.PORT || 3000);
const stage = process.env.APP_STAGE || "local";

const server = http.createServer((req, res) => {
  if (req.url === "/health") {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ ok: true, stage }));
    return;
  }

  res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
  res.end(`<!doctype html>
<html>
  <head><title>Tako Stage Pattern</title></head>
  <body>
    <h1>${stage}</h1>
    <p>The same app can run multiple isolated stages on one node.</p>
  </body>
</html>`);
});

server.listen(port, "0.0.0.0", () => {
  console.log(`${stage} stage listening on ${port}`);
});
