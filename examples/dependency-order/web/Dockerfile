FROM node:18-alpine

WORKDIR /app

# Create a simple web server
RUN echo 'const http = require("http"); \
const server = http.createServer((req, res) => { \
  if (req.url === "/") { \
    res.writeHead(200, {"Content-Type": "text/html"}); \
    res.end("<html><body><h1>Web Service</h1><p>Connected to API</p></body></html>"); \
  } else if (req.url === "/health") { \
    res.writeHead(200, {"Content-Type": "application/json"}); \
    res.end(JSON.stringify({status: "healthy", service: "web"})); \
  } else { \
    res.writeHead(404); \
    res.end("Not Found"); \
  } \
}); \
const PORT = process.env.PORT || 3001; \
server.listen(PORT, () => console.log(`Web listening on port ${PORT}`));' > index.js

EXPOSE 3001

CMD ["node", "index.js"]
