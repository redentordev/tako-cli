const redis = require('redis');
const os = require('os');

const REDIS_URL = process.env.REDIS_URL || 'redis://localhost:6379';
const WORKER_ID = process.env.WORKER_ID || os.hostname();

const client = redis.createClient({ url: REDIS_URL });

let jobsProcessed = 0;

async function processJob(job) {
  const { id, type, data, createdAt } = JSON.parse(job);

  console.log(`[${WORKER_ID}] Processing job ${id} (${type})`);

  // Simulate different job types
  switch (type) {
    case 'email':
      await simulateWork(1000);
      console.log(`[${WORKER_ID}] Sent email to ${data.recipient}`);
      break;

    case 'image':
      await simulateWork(2000);
      console.log(`[${WORKER_ID}] Processed image ${data.filename}`);
      break;

    case 'report':
      await simulateWork(3000);
      console.log(`[${WORKER_ID}] Generated report ${data.reportId}`);
      break;

    default:
      console.log(`[${WORKER_ID}] Unknown job type: ${type}`);
  }

  jobsProcessed++;

  // Store completion stats
  await client.hSet('worker:stats', WORKER_ID, jobsProcessed.toString());
  await client.lPush('completed', JSON.stringify({
    ...JSON.parse(job),
    completedAt: new Date().toISOString(),
    completedBy: WORKER_ID
  }));
}

function simulateWork(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}

async function worker() {
  console.log(`Worker ${WORKER_ID} starting...`);
  console.log(`Connected to Redis: ${REDIS_URL}`);

  await client.connect();

  // Set initial stats
  await client.hSet('worker:stats', WORKER_ID, '0');

  console.log(`Worker ${WORKER_ID} ready and waiting for jobs...`);

  // Process jobs from queue
  while (true) {
    try {
      // Block until a job is available (timeout after 5 seconds)
      const result = await client.brPop('jobs', 5);

      if (result) {
        await processJob(result.element);
      } else {
        // No jobs, just log we're still alive
        console.log(`[${WORKER_ID}] Waiting for jobs... (processed: ${jobsProcessed})`);
      }
    } catch (err) {
      console.error(`[${WORKER_ID}] Error processing job:`, err);
      await simulateWork(1000); // Wait before retrying
    }
  }
}

// Handle graceful shutdown
process.on('SIGTERM', async () => {
  console.log(`[${WORKER_ID}] Received SIGTERM, shutting down gracefully...`);
  await client.quit();
  process.exit(0);
});

process.on('SIGINT', async () => {
  console.log(`[${WORKER_ID}] Received SIGINT, shutting down gracefully...`);
  await client.quit();
  process.exit(0);
});

// Start worker
worker().catch(err => {
  console.error('Worker error:', err);
  process.exit(1);
});
