const express = require("express");
const fs = require("fs");
const path = require("path");

const app = express();
const dataDir = process.env.DATA_DIR || "/app/data";
const counterPath = path.join(dataDir, "counter.txt");

app.get("/health", (_req, res) => res.json({ ok: true }));

app.get("/", (_req, res) => {
  fs.mkdirSync(dataDir, { recursive: true });
  const current = Number(fs.existsSync(counterPath) ? fs.readFileSync(counterPath, "utf8") : "0");
  const next = current + 1;
  fs.writeFileSync(counterPath, String(next));
  res.json({ visits: next, dataDir });
});

app.listen(3000, "0.0.0.0");
