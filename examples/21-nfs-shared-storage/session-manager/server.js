const http = require("http");
const fs = require("fs");
const path = require("path");

const port = Number(process.env.PORT || 4000);
const sharedRepoPath = process.env.SHARED_REPO_PATH || "/app/shared-repo";
const sessionsDir = process.env.SESSIONS_DIR || "/app/sessions";

fs.mkdirSync(sessionsDir, { recursive: true });

function listSafe(dir) {
  try {
    return fs.readdirSync(dir).slice(0, 20);
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
    service: "session-manager",
    sharedRepoPath,
    sessionsDir,
    sharedEntries: listSafe(sharedRepoPath),
    sessions: listSafe(sessionsDir)
  }, null, 2));
});

server.listen(port, () => {
  console.log(`session-manager listening on ${port}`);
});
