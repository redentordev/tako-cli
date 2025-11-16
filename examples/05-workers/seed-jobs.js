#!/usr/bin/env node

// This script seeds the Redis queue with sample jobs
// Useful for testing the worker system

const redis = require('redis');

const REDIS_URL = process.env.REDIS_URL || 'redis://localhost:6379';

async function seedJobs() {
  const client = redis.createClient({ url: REDIS_URL });
  await client.connect();

  console.log('Seeding jobs...');

  const jobs = [
    { id: 1, type: 'email', data: { recipient: 'user1@example.com', subject: 'Welcome!' } },
    { id: 2, type: 'image', data: { filename: 'photo1.jpg', size: '2MB' } },
    { id: 3, type: 'email', data: { recipient: 'user2@example.com', subject: 'Newsletter' } },
    { id: 4, type: 'report', data: { reportId: 'R-001', type: 'monthly' } },
    { id: 5, type: 'image', data: { filename: 'photo2.jpg', size: '1.5MB' } },
    { id: 6, type: 'email', data: { recipient: 'user3@example.com', subject: 'Password Reset' } },
    { id: 7, type: 'report', data: { reportId: 'R-002', type: 'weekly' } },
    { id: 8, type: 'image', data: { filename: 'photo3.jpg', size: '3MB' } },
    { id: 9, type: 'email', data: { recipient: 'user4@example.com', subject: 'Confirmation' } },
    { id: 10, type: 'report', data: { reportId: 'R-003', type: 'daily' } },
  ];

  for (const job of jobs) {
    const jobData = JSON.stringify({
      ...job,
      createdAt: new Date().toISOString()
    });

    await client.lPush('jobs', jobData);
    console.log(`Added job ${job.id}: ${job.type}`);
  }

  console.log(`\nSeeded ${jobs.length} jobs to the queue!`);
  console.log('Workers will process them automatically.');

  // Show stats
  const queueLength = await client.lLen('jobs');
  const completedLength = await client.lLen('completed');
  const workerStats = await client.hGetAll('worker:stats');

  console.log('\nCurrent Stats:');
  console.log(`  Jobs in queue: ${queueLength}`);
  console.log(`  Jobs completed: ${completedLength}`);
  console.log('  Worker stats:', workerStats);

  await client.quit();
}

seedJobs().catch(err => {
  console.error('Error seeding jobs:', err);
  process.exit(1);
});
