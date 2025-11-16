const express = require('express');
const os = require('os');

const app = express();
const PORT = process.env.PORT || 4000;
const startTime = Date.now();

app.get('/info', (req, res) => {
  res.json({
    service: 'monorepo-api',
    version: '1.0.0',
    hostname: os.hostname(),
    uptime: Math.floor((Date.now() - startTime) / 1000),
    timestamp: new Date().toISOString()
  });
});

app.get('/health', (req, res) => {
  res.json({
    status: 'healthy',
    service: 'api',
    hostname: os.hostname(),
    timestamp: new Date().toISOString()
  });
});

app.listen(PORT, () => {
  console.log(`API service running on port ${PORT}`);
  console.log(`Hostname: ${os.hostname()}`);
});
