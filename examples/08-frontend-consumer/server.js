const express = require('express');
const fetch = require('node-fetch');
const app = express();
const port = 3000;

// Get API URL from environment variable (set in tako.yaml)
const API_URL = process.env.API_URL || 'http://backend-api_api:4000';

app.use(express.json());

// Health check endpoint
app.get('/health', (req, res) => {
  res.json({ status: 'healthy', service: 'frontend' });
});

// Proxy to backend API - users
app.get('/users', async (req, res) => {
  try {
    console.log(`Fetching users from: ${API_URL}/api/users`);
    const response = await fetch(`${API_URL}/api/users`);
    const data = await response.json();

    res.json({
      source: 'frontend',
      api_url: API_URL,
      data: data
    });
  } catch (error) {
    console.error('Error fetching users:', error);
    res.status(500).json({
      error: 'Failed to fetch users from backend',
      message: error.message
    });
  }
});

// Proxy to backend API - products
app.get('/products', async (req, res) => {
  try {
    console.log(`Fetching products from: ${API_URL}/api/products`);
    const response = await fetch(`${API_URL}/api/products`);
    const data = await response.json();

    res.json({
      source: 'frontend',
      api_url: API_URL,
      data: data
    });
  } catch (error) {
    console.error('Error fetching products:', error);
    res.status(500).json({
      error: 'Failed to fetch products from backend',
      message: error.message
    });
  }
});

// Root endpoint
app.get('/', (req, res) => {
  res.json({
    service: 'frontend',
    version: '1.0.0',
    backend_api: API_URL,
    endpoints: ['/users', '/products', '/health'],
    message: 'This frontend consumes backend-api service via cross-project imports'
  });
});

app.listen(port, '0.0.0.0', () => {
  console.log(`Frontend listening on port ${port}`);
  console.log(`Backend API URL: ${API_URL}`);
});
