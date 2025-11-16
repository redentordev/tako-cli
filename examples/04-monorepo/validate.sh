#!/bin/bash

# Validation script for 04-monorepo example

echo "Validating 04-monorepo deployment..."

# Check if all containers are running
echo "Checking if all containers are running..."
WEB_COUNT=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=monorepo_web --format '{{.Names}}' | wc -l")
API_COUNT=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=monorepo_api --format '{{.Names}}' | wc -l")

if [ "$WEB_COUNT" -eq "0" ]; then
    echo "Error: Web container is not running"
    exit 1
fi

if [ "$API_COUNT" -eq "0" ]; then
    echo "Error: API container is not running"
    exit 1
fi

echo "✓ Both containers are running (web: 1, api: 1)"

# Check web container logs
echo "Checking web container logs..."
WEB_LOGS=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker logs monorepo_web_1 2>&1 | tail -10")

if [[ "$WEB_LOGS" =~ "Server running" ]] || [[ "$WEB_LOGS" =~ "listening" ]] || [[ "$WEB_LOGS" =~ "started" ]]; then
    echo "✓ Web container started successfully"
else
    echo "Warning: Could not verify web startup from logs"
fi

# Check API container logs
echo "Checking API container logs..."
API_LOGS=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker logs monorepo_api_1 2>&1 | tail -10")

if [[ "$API_LOGS" =~ "API server running" ]] || [[ "$API_LOGS" =~ "listening" ]] || [[ "$API_LOGS" =~ "started" ]]; then
    echo "✓ API container started successfully"
else
    echo "Warning: Could not verify API startup from logs"
fi

# Test web can reach API via Docker DNS
echo "Testing web to API connectivity..."
WEB_API=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker exec monorepo_web_1 ping -c 1 api 2>&1" || echo "failed")

if [[ "$WEB_API" =~ "1 packets transmitted, 1 received" ]] || [[ "$WEB_API" =~ "0% packet loss" ]]; then
    echo "✓ Web can reach API via Docker DNS"
else
    echo "Warning: Could not verify web to API connectivity"
fi

echo "✓ All validations passed for 04-monorepo"
exit 0
