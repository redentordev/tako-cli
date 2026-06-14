const express = require("express");
const app = express();

app.get("/health", (_req, res) => res.json({ ok: true }));
app.get("/", (_req, res) => res.json({ service: "queue-api" }));

app.listen(3000, "0.0.0.0");
