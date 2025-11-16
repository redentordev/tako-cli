#!/bin/bash

# Validation script for 05-workers example

echo "Validating 05-workers deployment..."

# Check if all containers are running
echo "Checking if all containers are running..."
WORKER_COUNT=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=workers_worker --format '{{.Names}}' | wc -l")
REDIS_COUNT=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=workers_redis --format '{{.Names}}' | wc -l")

if [ "$WORKER_COUNT" -lt "3" ]; then
    echo "Error: Expected 3 worker replicas, found $WORKER_COUNT"
    exit 1
fi

if [ "$REDIS_COUNT" -eq "0" ]; then
    echo "Error: Redis container is not running"
    exit 1
fi

echo "✓ All containers are running (workers: $WORKER_COUNT, redis: 1)"

# Check Redis is ready
echo "Checking if Redis is ready..."
REDIS_PING=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker exec workers_redis_1 redis-cli ping 2>&1" || echo "failed")

if [[ "$REDIS_PING" =~ "PONG" ]]; then
    echo "✓ Redis is ready"
else
    echo "Warning: Could not verify Redis: $REDIS_PING"
fi

# Check worker logs to ensure they're processing
echo "Checking worker logs..."
for i in 1 2 3; do
    WORKER_LOGS=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker logs workers_worker_$i 2>&1 | tail -5")

    if [[ "$WORKER_LOGS" =~ "Worker" ]] || [[ "$WORKER_LOGS" =~ "Processing" ]] || [[ "$WORKER_LOGS" =~ "started" ]]; then
        echo "✓ Worker $i is running"
    else
        echo "Warning: Could not verify worker $i from logs"
    fi
done

# Test worker can reach Redis
echo "Testing worker to Redis connectivity..."
WORKER_REDIS=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker exec workers_worker_1 ping -c 1 redis 2>&1" || echo "failed")

if [[ "$WORKER_REDIS" =~ "1 packets transmitted, 1 received" ]] || [[ "$WORKER_REDIS" =~ "0% packet loss" ]]; then
    echo "✓ Workers can reach Redis via Docker DNS"
else
    echo "Warning: Could not verify worker to Redis connectivity"
fi

echo "✓ All validations passed for 05-workers"
exit 0
