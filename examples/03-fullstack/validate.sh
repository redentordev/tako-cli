#!/bin/bash

# Validation script for 03-fullstack example

echo "Validating 03-fullstack deployment..."

# Check if all containers are running
echo "Checking if all containers are running..."
WEB_COUNT=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=fullstack_web --format '{{.Names}}' | wc -l")
API_COUNT=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=fullstack_api --format '{{.Names}}' | wc -l")
DB_COUNT=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=fullstack_postgres --format '{{.Names}}' | wc -l")
REDIS_COUNT=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=fullstack_redis --format '{{.Names}}' | wc -l")

if [ "$WEB_COUNT" -eq "0" ]; then
    echo "Error: Web container is not running"
    exit 1
fi

if [ "$API_COUNT" -lt "2" ]; then
    echo "Error: Expected 2 API replicas, found $API_COUNT"
    exit 1
fi

if [ "$DB_COUNT" -eq "0" ]; then
    echo "Error: Database container is not running"
    exit 1
fi

if [ "$REDIS_COUNT" -eq "0" ]; then
    echo "Error: Redis container is not running"
    exit 1
fi

echo "✓ All containers are running (web: 1, api: $API_COUNT, db: 1, redis: 1)"

# Check Redis is ready
echo "Checking if Redis is ready..."
REDIS_PING=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker exec fullstack_redis_1 redis-cli ping 2>&1" || echo "failed")

if [[ "$REDIS_PING" =~ "PONG" ]]; then
    echo "✓ Redis is ready"
else
    echo "Warning: Could not verify Redis: $REDIS_PING"
fi

# Check database is ready
echo "Checking if database is ready..."
DB_READY=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker exec fullstack_postgres_1 pg_isready -U postgres 2>&1" || echo "failed")

if [[ "$DB_READY" =~ "accepting connections" ]]; then
    echo "✓ Database is ready"
else
    echo "Warning: Could not verify database: $DB_READY"
fi

# Check API replicas can reach Redis
echo "Testing API to Redis connection..."
API_REDIS=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker exec fullstack_api_1 ping -c 1 redis 2>&1" || echo "failed")

if [[ "$API_REDIS" =~ "1 packets transmitted, 1 received" ]] || [[ "$API_REDIS" =~ "0% packet loss" ]]; then
    echo "✓ API can reach Redis via Docker DNS"
else
    echo "Warning: Could not verify API to Redis connectivity"
fi

# Check if Traefik reverse proxy is configured (if web has proxy)
TRAEFIK_RUNNING=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=traefik --format '{{.Names}}' | wc -l")

if [ "$TRAEFIK_RUNNING" -gt "0" ]; then
    echo "✓ Traefik reverse proxy is running"
fi

echo "✓ All validations passed for 03-fullstack"
exit 0
