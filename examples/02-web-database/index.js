const express = require('express');
const { Pool } = require('pg');

const app = express();
const PORT = process.env.PORT || 3000;

// PostgreSQL connection
const pool = new Pool({
  connectionString: process.env.DATABASE_URL,
});

// Initialize database
async function initDB() {
  try {
    await pool.query(`
      CREATE TABLE IF NOT EXISTS visitors (
        id SERIAL PRIMARY KEY,
        visit_time TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
        ip_address VARCHAR(50)
      )
    `);
    console.log('Database initialized');
  } catch (err) {
    console.error('Database initialization error:', err);
  }
}

initDB();

app.get('/', async (req, res) => {
  try {
    // Record visit
    const clientIP = req.headers['x-forwarded-for'] || req.connection.remoteAddress;
    await pool.query('INSERT INTO visitors (ip_address) VALUES ($1)', [clientIP]);

    // Get total count
    const result = await pool.query('SELECT COUNT(*) as count FROM visitors');
    const count = result.rows[0].count;

    // Get recent visits
    const recent = await pool.query(
      'SELECT visit_time, ip_address FROM visitors ORDER BY visit_time DESC LIMIT 10'
    );

    res.send(`
      <!DOCTYPE html>
      <html>
        <head>
          <title>Web + Database Example</title>
          <style>
            body {
              font-family: Arial, sans-serif;
              max-width: 800px;
              margin: 50px auto;
              padding: 20px;
              background: linear-gradient(135deg, #f093fb 0%, #f5576c 100%);
              color: white;
            }
            .container {
              background: rgba(255, 255, 255, 0.1);
              padding: 40px;
              border-radius: 10px;
              backdrop-filter: blur(10px);
            }
            h1 { margin-top: 0; }
            .count {
              font-size: 48px;
              font-weight: bold;
              margin: 20px 0;
            }
            table {
              width: 100%;
              border-collapse: collapse;
              margin-top: 20px;
              background: rgba(255, 255, 255, 0.1);
            }
            th, td {
              padding: 10px;
              text-align: left;
              border-bottom: 1px solid rgba(255, 255, 255, 0.2);
            }
            th {
              background: rgba(0, 0, 0, 0.2);
            }
          </style>
        </head>
        <body>
          <div class="container">
            <h1>Web + Database Example</h1>
            <p>This page tracks visitor counts using PostgreSQL.</p>

            <div class="count">
              ${count} total visits
            </div>

            <h2>Recent Visits</h2>
            <table>
              <thead>
                <tr>
                  <th>Time</th>
                  <th>IP Address</th>
                </tr>
              </thead>
              <tbody>
                ${recent.rows.map(row => `
                  <tr>
                    <td>${new Date(row.visit_time).toLocaleString()}</td>
                    <td>${row.ip_address}</td>
                  </tr>
                `).join('')}
              </tbody>
            </table>

            <p style="margin-top: 30px;"><strong>Features demonstrated:</strong></p>
            <ul>
              <li>PostgreSQL database connection</li>
              <li>Persistent data storage</li>
              <li>Service-to-service communication</li>
              <li>Database initialization on startup</li>
            </ul>
          </div>
        </body>
      </html>
    `);
  } catch (err) {
    console.error('Error:', err);
    res.status(500).send('Database error: ' + err.message);
  }
});

app.get('/health', async (req, res) => {
  try {
    await pool.query('SELECT 1');
    res.json({
      status: 'healthy',
      database: 'connected',
      timestamp: new Date().toISOString()
    });
  } catch (err) {
    res.status(500).json({
      status: 'unhealthy',
      database: 'disconnected',
      error: err.message
    });
  }
});

app.listen(PORT, () => {
  console.log(`Server running on port ${PORT}`);
  console.log(`Database: ${process.env.DATABASE_URL}`);
});
