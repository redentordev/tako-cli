import http from "node:http";

const port = Number(process.env.PORT || 4000);

const server = http.createServer((req, res) => {
  if (req.url === "/health") {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ ok: true, service: "api" }));
    return;
  }

  res.writeHead(200, { "content-type": "application/json" });
  res.end(JSON.stringify({ service: "api", time: new Date().toISOString() }));
});

server.listen(port, "0.0.0.0", () => {
  console.log(`api listening on ${port}`);
});
