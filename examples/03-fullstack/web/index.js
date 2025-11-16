const express = require('express');
const axios = require('axios');

const app = express();
const PORT = process.env.PORT || 3000;
const API_URL = process.env.API_URL || 'http://api:4000';

app.get('/', async (req, res) => {
  try {
    // Call internal API
    const statsResponse = await axios.get(`${API_URL}/stats`);
    const stats = statsResponse.data;

    // Call API to record visit
    await axios.post(`${API_URL}/visits`);

    res.send(`
      <!DOCTYPE html>
      <html>
        <head>
          <title>Full-Stack Example</title>
          <style>
            body {
              font-family: Arial, sans-serif;
              max-width: 900px;
              margin: 50px auto;
              padding: 20px;
              background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
              color: white;
            }
            .container {
              background: rgba(255, 255, 255, 0.1);
              padding: 40px;
              border-radius: 10px;
              backdrop-filter: blur(10px);
            }
            h1 { margin-top: 0; }
            .stats {
              display: grid;
              grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
              gap: 20px;
              margin: 30px 0;
            }
            .stat-card {
              background: rgba(255, 255, 255, 0.1);
              padding: 20px;
              border-radius: 8px;
              text-align: center;
            }
            .stat-value {
              font-size: 36px;
              font-weight: bold;
              margin: 10px 0;
            }
            .stat-label {
              font-size: 14px;
              opacity: 0.8;
            }
            .architecture {
              margin-top: 30px;
              background: rgba(0, 0, 0, 0.2);
              padding: 20px;
              border-radius: 8px;
              font-family: monospace;
              font-size: 14px;
            }
          </style>
        </head>
        <body>
          <div class="container">
            <h1>Full-Stack Application</h1>
            <p>Complete multi-service architecture with internal API communication.</p>

            <div class="stats">
              <div class="stat-card">
                <div class="stat-label">Total Visits</div>
                <div class="stat-value">${stats.visits || 0}</div>
              </div>
              <div class="stat-card">
                <div class="stat-label">Cache Hits</div>
                <div class="stat-value">${stats.cacheHits || 0}</div>
              </div>
              <div class="stat-card">
                <div class="stat-label">API Calls</div>
                <div class="stat-value">${stats.apiCalls || 0}</div>
              </div>
            </div>

            <h2>Architecture</h2>
            <div class="architecture">
              Internet<br>
              &nbsp;&nbsp;↓<br>
              <strong>WEB</strong> (this page) → port 3000 → PUBLIC<br>
              &nbsp;&nbsp;↓<br>
              <strong>API</strong> (internal) → port 4000 → INTERNAL ONLY<br>
              &nbsp;&nbsp;↓&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;↓<br>
              <strong>POSTGRES</strong>&nbsp;&nbsp;&nbsp;<strong>REDIS</strong><br>
              (persistent)&nbsp;&nbsp;(cache)
            </div>

            <p style="margin-top: 30px;"><strong>Features demonstrated:</strong></p>
            <ul>
              <li>Public web service (accessible via domain)</li>
              <li>Internal API service (not exposed to internet)</li>
              <li>Service-to-service communication (web → api)</li>
              <li>PostgreSQL for persistent data</li>
              <li>Redis for caching</li>
              <li>API replicas for load balancing</li>
            </ul>
          </div>
        </body>
      </html>
    `);
  } catch (err) {
    console.error('Error calling API:', err.message);
    res.status(500).send(`
      <!DOCTYPE html>
      <html>
        <head><title>Error</title></head>
        <body>
          <h1>Error</h1>
          <p>Could not connect to API: ${err.message}</p>
          <p>API URL: ${API_URL}</p>
        </body>
      </html>
    `);
  }
});

app.get('/health', async (req, res) => {
  try {
    const apiHealth = await axios.get(`${API_URL}/health`);
    res.json({
      status: 'healthy',
      web: 'ok',
      api: apiHealth.data,
      timestamp: new Date().toISOString()
    });
  } catch (err) {
    res.status(500).json({
      status: 'unhealthy',
      web: 'ok',
      api: 'error',
      error: err.message
    });
  }
});

app.listen(PORT, () => {
  console.log(`Web server running on port ${PORT}`);
  console.log(`API URL: ${API_URL}`);
});
