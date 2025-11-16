const express = require('express');
const os = require('os');

const app = express();
const PORT = process.env.PORT || 3000;
const HOSTNAME = os.hostname();
const START_TIME = Date.now();

// Track request count for this instance
let requestCount = 0;

app.get('/', (req, res) => {
  requestCount++;

  const uptime = Math.floor((Date.now() - START_TIME) / 1000);
  const memoryUsage = process.memoryUsage();

  res.send(`
    <!DOCTYPE html>
    <html>
      <head>
        <title>Scaling Example</title>
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
          .instance-info {
            background: rgba(255, 255, 255, 0.2);
            padding: 30px;
            border-radius: 8px;
            margin: 30px 0;
            font-family: monospace;
            font-size: 18px;
            text-align: center;
          }
          .instance-id {
            font-size: 36px;
            font-weight: bold;
            color: #fee140;
            margin: 20px 0;
          }
          .stats {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
            gap: 15px;
            margin: 20px 0;
            font-size: 14px;
          }
          .stat {
            background: rgba(0, 0, 0, 0.2);
            padding: 15px;
            border-radius: 5px;
          }
          .stat-label {
            opacity: 0.8;
            font-size: 12px;
          }
          .stat-value {
            font-size: 24px;
            font-weight: bold;
            margin-top: 5px;
          }
          .refresh-btn {
            background: #fee140;
            color: #333;
            border: none;
            padding: 15px 30px;
            font-size: 16px;
            border-radius: 5px;
            cursor: pointer;
            margin: 20px 0;
          }
          .refresh-btn:hover {
            background: #ffd700;
          }
          .architecture {
            background: rgba(0, 0, 0, 0.2);
            padding: 20px;
            border-radius: 8px;
            font-family: monospace;
            font-size: 14px;
            margin: 20px 0;
          }
        </style>
        <script>
          function refreshPage() {
            location.reload();
          }

          // Auto-refresh every 3 seconds
          setTimeout(refreshPage, 3000);
        </script>
      </head>
      <body>
        <div class="container">
          <h1>Load Balancing & Scaling</h1>
          <p>This page demonstrates load balancing across multiple instances.</p>

          <div class="instance-info">
            <div>You are connected to:</div>
            <div class="instance-id">${HOSTNAME}</div>
            <button class="refresh-btn" onclick="refreshPage()">Refresh to See Load Balancing</button>
            <p style="font-size: 14px; opacity: 0.8; margin-top: 10px;">
              (Auto-refreshes every 3 seconds)
            </p>
          </div>

          <div class="stats">
            <div class="stat">
              <div class="stat-label">Requests Handled</div>
              <div class="stat-value">${requestCount}</div>
            </div>
            <div class="stat">
              <div class="stat-label">Uptime</div>
              <div class="stat-value">${uptime}s</div>
            </div>
            <div class="stat">
              <div class="stat-label">Memory (RSS)</div>
              <div class="stat-value">${Math.round(memoryUsage.rss / 1024 / 1024)}MB</div>
            </div>
            <div class="stat">
              <div class="stat-label">Memory (Heap)</div>
              <div class="stat-value">${Math.round(memoryUsage.heapUsed / 1024 / 1024)}MB</div>
            </div>
          </div>

          <h2>Architecture</h2>
          <div class="architecture">
            User Request<br>
            &nbsp;&nbsp;↓<br>
            <strong>Traefik Load Balancer</strong><br>
            &nbsp;&nbsp;↓&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;(round_robin)<br>
            ├─→ <strong>Instance 1</strong> (port 3000)<br>
            ├─→ <strong>Instance 2</strong> (port 3000)<br>
            └─→ <strong>Instance 3</strong> (port 3000)
          </div>

          <p><strong>How it works:</strong></p>
          <ul>
            <li>3 identical instances of the application</li>
            <li>Traefik distributes requests using round-robin</li>
            <li>Each refresh may hit a different instance</li>
            <li>Watch the hostname change as you refresh</li>
            <li>All instances are identical and stateless</li>
          </ul>

          <p><strong>Benefits of scaling:</strong></p>
          <ul>
            <li><strong>High Availability:</strong> If one instance fails, others continue</li>
            <li><strong>Load Distribution:</strong> Traffic spread across instances</li>
            <li><strong>Zero Downtime:</strong> Rolling updates without service interruption</li>
            <li><strong>Performance:</strong> Handle more concurrent requests</li>
          </ul>

          <h2>Configuration</h2>
          <pre style="background: rgba(0,0,0,0.3); padding: 15px; border-radius: 5px; overflow-x: auto;">
services:
  web:
    replicas: 3              # 3 instances
    loadBalancer:
      strategy: round_robin  # Distribution strategy
          </pre>

          <p style="margin-top: 30px; font-size: 14px; opacity: 0.8;">
            <strong>Tip:</strong> Keep refreshing to see the hostname change!<br>
            Each different hostname = different container instance.
          </p>
        </div>
      </body>
    </html>
  `);
});

app.get('/health', (req, res) => {
  res.json({
    status: 'healthy',
    hostname: HOSTNAME,
    uptime: Math.floor((Date.now() - START_TIME) / 1000),
    requests: requestCount,
    timestamp: new Date().toISOString()
  });
});

app.get('/api/instance', (req, res) => {
  res.json({
    hostname: HOSTNAME,
    uptime: Math.floor((Date.now() - START_TIME) / 1000),
    requests: requestCount,
    memory: process.memoryUsage(),
    timestamp: new Date().toISOString()
  });
});

app.listen(PORT, () => {
  console.log(`Server running on port ${PORT}`);
  console.log(`Hostname: ${HOSTNAME}`);
  console.log(`Process ID: ${process.pid}`);
});
