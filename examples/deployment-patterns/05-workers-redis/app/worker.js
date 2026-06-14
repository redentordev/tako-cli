const intervalMs = Number(process.env.WORKER_INTERVAL_MS || 5000);

setInterval(() => {
  console.log("worker heartbeat", new Date().toISOString(), process.env.REDIS_URL);
}, intervalMs);
