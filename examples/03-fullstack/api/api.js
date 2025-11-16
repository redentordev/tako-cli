const express = require('express');
const { Pool } = require('pg');
const redis = require('redis');

const app = express();
const PORT = process.env.PORT || 4000;

app.use(express.json());

// PostgreSQL connection
const pool = new Pool({
  connectionString: process.env.DATABASE_URL,
});

// Redis connection
const redisClient = redis.createClient({
  url: process.env.REDIS_URL,
});

redisClient.connect().catch(console.error);

// Initialize database
async function initDB() {
  try {
    await pool.query(`
      CREATE TABLE IF NOT EXISTS visits (
        id SERIAL PRIMARY KEY,
        timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP
      )
    `);
    console.log('Database initialized');
  } catch (err) {
    console.error('Database initialization error:', err);
  }
}

initDB();

// Record a visit
app.post('/visits', async (req, res) => {
  try {
    await pool.query('INSERT INTO visits DEFAULT VALUES');

    // Increment API call counter in Redis
    await redisClient.incr('api_calls');

    res.json({ success: true });
  } catch (err) {
    console.error('Error recording visit:', err);
    res.status(500).json({ error: err.message });
  }
});

// Get statistics
app.get('/stats', async (req, res) => {
  try {
    // Try to get from cache first
    const cached = await redisClient.get('stats');

    if (cached) {
      await redisClient.incr('cache_hits');
      return res.json(JSON.parse(cached));
    }

    // Get from database
    const visitResult = await pool.query('SELECT COUNT(*) as count FROM visits');
    const visits = parseInt(visitResult.rows[0].count);

    const cacheHits = parseInt(await redisClient.get('cache_hits') || 0);
    const apiCalls = parseInt(await redisClient.get('api_calls') || 0);

    const stats = {
      visits,
      cacheHits,
      apiCalls,
      timestamp: new Date().toISOString()
    };

    // Cache for 5 seconds
    await redisClient.setEx('stats', 5, JSON.stringify(stats));

    res.json(stats);
  } catch (err) {
    console.error('Error getting stats:', err);
    res.status(500).json({ error: err.message });
  }
});

// Health check
app.get('/health', async (req, res) => {
  try {
    // Check database
    await pool.query('SELECT 1');

    // Check Redis
    await redisClient.ping();

    res.json({
      status: 'healthy',
      database: 'connected',
      redis: 'connected',
      timestamp: new Date().toISOString()
    });
  } catch (err) {
    res.status(500).json({
      status: 'unhealthy',
      error: err.message
    });
  }
});

app.listen(PORT, () => {
  console.log(`API server running on port ${PORT}`);
  console.log(`Database: ${process.env.DATABASE_URL}`);
  console.log(`Redis: ${process.env.REDIS_URL}`);
});
