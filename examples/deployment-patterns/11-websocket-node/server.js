import http from "node:http";
import { WebSocketServer } from "ws";

const port = Number(process.env.PORT || 3000);

const server = http.createServer((req, res) => {
  if (req.url === "/health") {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ ok: true, service: "realtime" }));
    return;
  }

  res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
  res.end(`<!doctype html>
<html>
  <head><title>Tako WebSocket Pattern</title></head>
  <body>
    <h1>Realtime service</h1>
    <p>Open a WebSocket connection to <code>/socket</code>.</p>
  </body>
</html>`);
});

const sockets = new WebSocketServer({ server, path: "/socket" });

sockets.on("connection", (socket) => {
  socket.send(JSON.stringify({ type: "hello", at: new Date().toISOString() }));
  socket.on("message", (message) => {
    socket.send(JSON.stringify({ type: "echo", message: message.toString() }));
  });
});

server.listen(port, "0.0.0.0", () => {
  console.log(`realtime service listening on ${port}`);
});
