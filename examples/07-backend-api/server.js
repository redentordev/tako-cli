const express = require('express');
const app = express();
const port = 4000;

app.use(express.json());

// Health check endpoint
app.get('/health', (req, res) => {
  res.json({ status: 'healthy', service: 'backend-api' });
});

// API endpoints
app.get('/api/users', (req, res) => {
  res.json({
    users: [
      { id: 1, name: 'Alice', email: 'alice@example.com' },
      { id: 2, name: 'Bob', email: 'bob@example.com' },
      { id: 3, name: 'Charlie', email: 'charlie@example.com' }
    ]
  });
});

app.get('/api/products', (req, res) => {
  res.json({
    products: [
      { id: 1, name: 'Laptop', price: 999 },
      { id: 2, name: 'Phone', price: 599 },
      { id: 3, name: 'Tablet', price: 399 }
    ]
  });
});

// Root endpoint
app.get('/', (req, res) => {
  res.json({
    service: 'backend-api',
    version: '1.0.0',
    endpoints: ['/api/users', '/api/products', '/health']
  });
});

app.listen(port, '0.0.0.0', () => {
  console.log(`Backend API listening on port ${port}`);
});
