const express = require("express");

const app = express();
const port = Number(process.env.PORT || 3000);

app.get("/health", (_req, res) => {
  res.json({ ok: true });
});

app.get("/", (_req, res) => {
  res.json({ service: "node-api", version: process.env.APP_VERSION || "dev" });
});

app.listen(port, "0.0.0.0", () => {
  console.log(`node api listening on ${port}`);
});
