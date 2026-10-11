FROM node:18-alpine

WORKDIR /app

# Create a simple worker that processes jobs
RUN echo 'console.log("Worker starting..."); \
console.log("Environment:", process.env.NODE_ENV); \
console.log("Connected to:", process.env.DATABASE_URL ? "Database" : "None", process.env.REDIS_URL ? "Redis" : "None"); \
let jobCount = 0; \
setInterval(() => { \
  jobCount++; \
  console.log(`[${new Date().toISOString()}] Processing job #${jobCount}...`); \
}, 10000);' > worker.js

CMD ["node", "worker.js"]
