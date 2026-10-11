FROM node:18-alpine

WORKDIR /app

# Create a simple API server with health check
RUN echo 'const http = require("http"); \
const server = http.createServer((req, res) => { \
  if (req.url === "/health") { \
    res.writeHead(200, {"Content-Type": "application/json"}); \
    res.end(JSON.stringify({status: "healthy", service: "api", time: new Date().toISOString()})); \
  } else { \
    res.writeHead(200, {"Content-Type": "application/json"}); \
    res.end(JSON.stringify({message: "API Service", version: "1.0.0"})); \
  } \
}); \
const PORT = process.env.PORT || 3000; \
server.listen(PORT, () => console.log(`API listening on port ${PORT}`));' > index.js

EXPOSE 3000

CMD ["node", "index.js"]
