const express = require('express');
const axios = require('axios');

const app = express();
const PORT = process.env.PORT || 3000;
const API_URL = process.env.API_URL || 'http://api:4000';

app.get('/', async (req, res) => {
  try {
    const apiResponse = await axios.get(`${API_URL}/info`);
    const apiInfo = apiResponse.data;

    res.send(`
      <!DOCTYPE html>
      <html>
        <head>
          <title>Monorepo Example</title>
          <style>
            body {
              font-family: Arial, sans-serif;
              max-width: 800px;
              margin: 50px auto;
              padding: 20px;
              background: linear-gradient(135deg, #fa709a 0%, #fee140 100%);
              color: #333;
            }
            .container {
              background: white;
              padding: 40px;
              border-radius: 10px;
              box-shadow: 0 10px 40px rgba(0,0,0,0.1);
            }
            h1 { margin-top: 0; color: #fa709a; }
            .service-info {
              display: grid;
              grid-template-columns: 1fr 1fr;
              gap: 20px;
              margin: 30px 0;
            }
            .info-card {
              background: #f8f9fa;
              padding: 20px;
              border-radius: 8px;
              border-left: 4px solid #fa709a;
            }
            .info-card h3 {
              margin-top: 0;
              color: #fa709a;
            }
            .info-card p {
              margin: 5px 0;
              font-family: monospace;
              font-size: 14px;
            }
            code {
              background: #f8f9fa;
              padding: 2px 6px;
              border-radius: 3px;
              font-family: monospace;
            }
          </style>
        </head>
        <body>
          <div class="container">
            <h1>Monorepo Example</h1>
            <p>Services organized in subdirectories with separate build contexts.</p>

            <div class="service-info">
              <div class="info-card">
                <h3>Web Service</h3>
                <p><strong>Location:</strong> /web</p>
                <p><strong>Build:</strong> ./web</p>
                <p><strong>Port:</strong> 3000</p>
                <p><strong>Type:</strong> Public</p>
              </div>

              <div class="info-card">
                <h3>API Service</h3>
                <p><strong>Location:</strong> /api</p>
                <p><strong>Build:</strong> ./api</p>
                <p><strong>Port:</strong> 4000</p>
                <p><strong>Type:</strong> Internal</p>
                <p><strong>Replicas:</strong> 2</p>
              </div>
            </div>

            <h2>API Response</h2>
            <div class="info-card">
              <p><strong>Service:</strong> ${apiInfo.service}</p>
              <p><strong>Version:</strong> ${apiInfo.version}</p>
              <p><strong>Hostname:</strong> ${apiInfo.hostname}</p>
              <p><strong>Uptime:</strong> ${apiInfo.uptime}s</p>
            </div>

            <h2>Monorepo Structure</h2>
            <pre style="background: #f8f9fa; padding: 15px; border-radius: 5px; overflow-x: auto;">
04-monorepo/
├── tako.yaml             # Root configuration
├── web/                  # Web service
│   ├── Dockerfile
│   ├── package.json
│   └── server.js
├── api/                  # API service
│   ├── Dockerfile
│   ├── package.json
│   └── server.js
└── README.md
            </pre>

            <p style="margin-top: 30px;"><strong>Features demonstrated:</strong></p>
            <ul>
              <li>Multiple services in subdirectories</li>
              <li>Separate build contexts for each service</li>
              <li>Independent Dockerfiles per service</li>
              <li>Centralized configuration in root tako.yaml</li>
              <li>Service-to-service communication</li>
            </ul>
          </div>
        </body>
      </html>
    `);
  } catch (err) {
    console.error('Error calling API:', err.message);
    res.status(500).send(`Error: ${err.message}`);
  }
});

app.listen(PORT, () => {
  console.log(`Web service running on port ${PORT}`);
  console.log(`API URL: ${API_URL}`);
});
