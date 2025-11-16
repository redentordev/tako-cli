const express = require('express');
const app = express();
const PORT = process.env.PORT || 3000;

app.get('/', (req, res) => {
  res.send(`
    <!DOCTYPE html>
    <html>
      <head>
        <title>Simple Web Example</title>
        <style>
          body {
            font-family: Arial, sans-serif;
            max-width: 800px;
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
          .version { font-size: 12px; opacity: 0.8; }
          p { line-height: 1.6; }
        </style>
      </head>
      <body>
        <div class="container">
          <h1>Hello from Simple Web!</h1>
          <p>This is a basic Node.js web server deployed with Tako CLI.</p>
          <p><strong>Features demonstrated:</strong></p>
          <ul>
            <li>Single service deployment</li>
            <li>Public domain with proxy configuration</li>
            <li>Automatic HTTPS with Let's Encrypt</li>
          </ul>
        </div>
      </body>
    </html>
  `);
});

app.get('/health', (req, res) => {
  res.json({ status: 'healthy', timestamp: new Date().toISOString() });
});

app.listen(PORT, () => {
  console.log(`Server running on port ${PORT}`);
  console.log(`Environment: ${process.env.NODE_ENV}`);
});
