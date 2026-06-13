const http = require("http");
const fs = require("fs");
const path = require("path");

const port = Number(process.env.PORT || 3000);
const repoPath = process.env.REPO_PATH || "/app/repo";

function listFiles(dir, prefix = "") {
  try {
    return fs.readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
      const rel = path.join(prefix, entry.name);
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        return listFiles(full, rel);
      }
      return rel;
    }).slice(0, 50);
  } catch {
    return [];
  }
}

const server = http.createServer((req, res) => {
  if (req.url === "/health") {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ ok: true }));
    return;
  }

  res.writeHead(200, { "content-type": "application/json" });
  res.end(JSON.stringify({
    service: "content-server",
    repoPath,
    files: listFiles(repoPath)
  }, null, 2));
});

server.listen(port, () => {
  console.log(`content-server listening on ${port}`);
});
