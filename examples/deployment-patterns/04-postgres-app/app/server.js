const express = require("express");
const { Pool } = require("pg");

const app = express();
const pool = new Pool({ connectionString: process.env.DATABASE_URL });

app.get("/health", async (_req, res) => {
  const result = await pool.query("select 1 as ok");
  res.json({ ok: result.rows[0].ok === 1 });
});

app.get("/", (_req, res) => {
  res.json({ service: "postgres-app" });
});

app.listen(3000, "0.0.0.0");
