const express = require('express');
const app = express();

const PORT = process.env.PORT || 3000;

// Middleware to parse JSON
app.use(express.json());

// Health check endpoint
app.get('/health', (req, res) => {
  res.json({ status: 'healthy', timestamp: new Date().toISOString() });
});

// Main endpoint that displays all environment variables (secrets should be loaded)
app.get('/', (req, res) => {
  const secrets = {
    DATABASE_URL: process.env.DATABASE_URL || 'NOT_SET',
    JWT_SECRET: process.env.JWT_SECRET || 'NOT_SET',
    API_KEY: process.env.API_KEY || 'NOT_SET',
    STRIPE_KEY: process.env.STRIPE_KEY || 'NOT_SET',
  };

  const nonSecrets = {
    NODE_ENV: process.env.NODE_ENV || 'NOT_SET',
    PORT: PORT,
  };

  res.json({
    message: 'Tako Secrets Test API',
    timestamp: new Date().toISOString(),
    environment: nonSecrets,
    secrets: {
      DATABASE_URL: secrets.DATABASE_URL !== 'NOT_SET' ? '✅ Loaded (hidden)' : '❌ NOT_SET',
      JWT_SECRET: secrets.JWT_SECRET !== 'NOT_SET' ? '✅ Loaded (hidden)' : '❌ NOT_SET',
      API_KEY: secrets.API_KEY !== 'NOT_SET' ? '✅ Loaded (hidden)' : '❌ NOT_SET',
      STRIPE_KEY: secrets.STRIPE_KEY !== 'NOT_SET' ? '✅ Loaded (hidden)' : '❌ NOT_SET',
    },
    // This endpoint shows if secrets are actually loaded (for testing only!)
    // In production, NEVER expose actual secret values
    debug_actual_values: process.env.DEBUG === 'true' ? secrets : 'Set DEBUG=true to see values'
  });
});

// Secret values endpoint (for verification during testing)
app.get('/api/verify-secrets', (req, res) => {
  const secretsLoaded = {
    DATABASE_URL: !!process.env.DATABASE_URL,
    JWT_SECRET: !!process.env.JWT_SECRET,
    API_KEY: !!process.env.API_KEY,
    STRIPE_KEY: !!process.env.STRIPE_KEY,
  };

  const allLoaded = Object.values(secretsLoaded).every(v => v);

  res.json({
    status: allLoaded ? 'success' : 'partial',
    message: allLoaded ? 'All secrets loaded successfully!' : 'Some secrets are missing',
    secrets: secretsLoaded,
    count: {
      total: 4,
      loaded: Object.values(secretsLoaded).filter(v => v).length
    }
  });
});

// Endpoint to test DATABASE_URL parsing
app.get('/api/database-info', (req, res) => {
  const dbUrl = process.env.DATABASE_URL;
  
  if (!dbUrl) {
    return res.status(500).json({ error: 'DATABASE_URL not set' });
  }

  try {
    const url = new URL(dbUrl);
    res.json({
      protocol: url.protocol,
      host: url.hostname,
      port: url.port || 'default',
      database: url.pathname.substring(1),
      hasCredentials: !!(url.username && url.password),
      message: 'Database URL parsed successfully (credentials hidden for security)'
    });
  } catch (err) {
    res.json({
      raw: dbUrl,
      message: 'DATABASE_URL is set but not a URL format',
      length: dbUrl.length
    });
  }
});

// Start server
app.listen(PORT, () => {
  console.log(`🚀 Tako Secrets Test API running on port ${PORT}`);
  console.log(`📊 Environment: ${process.env.NODE_ENV || 'development'}`);
  console.log(`🔐 Secrets check:`);
  console.log(`   DATABASE_URL: ${process.env.DATABASE_URL ? '✅' : '❌'}`);
  console.log(`   JWT_SECRET: ${process.env.JWT_SECRET ? '✅' : '❌'}`);
  console.log(`   API_KEY: ${process.env.API_KEY ? '✅' : '❌'}`);
  console.log(`   STRIPE_KEY: ${process.env.STRIPE_KEY ? '✅' : '❌'}`);
  console.log(`\nEndpoints:`);
  console.log(`   GET / - Main info`);
  console.log(`   GET /health - Health check`);
  console.log(`   GET /api/verify-secrets - Verify secrets loaded`);
  console.log(`   GET /api/database-info - Database URL info`);
});
