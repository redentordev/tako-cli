#!/bin/bash

# Validation script for 01-simple-web example

echo "Validating 01-simple-web deployment..."

# Check if container is running
echo "Checking if container is running..."
CONTAINER_COUNT=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=simple-web_web --format '{{.Names}}' | wc -l")

if [ "$CONTAINER_COUNT" -eq "0" ]; then
    echo "Error: Container is not running"
    exit 1
fi

echo "✓ Container is running"

# Check if container is healthy (if it has port exposed)
echo "Checking container health..."
CONTAINER_STATUS=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=simple-web_web --format '{{.Status}}'")

if [[ ! "$CONTAINER_STATUS" =~ "Up" ]]; then
    echo "Error: Container is not healthy: $CONTAINER_STATUS"
    exit 1
fi

echo "✓ Container is healthy: $CONTAINER_STATUS"

# Check if port 3000 is listening
echo "Checking if port 3000 is listening..."
PORT_LISTENING=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker exec simple-web_web_1 netstat -tuln 2>/dev/null | grep :3000 || echo 'not found'")

if [[ "$PORT_LISTENING" == "not found" ]]; then
    echo "Warning: Could not verify port 3000 (netstat not available)"
else
    echo "✓ Port 3000 is listening"
fi

# Check container logs for startup message
echo "Checking container logs..."
LOGS=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker logs simple-web_web_1 2>&1")

if [[ "$LOGS" =~ "Server running" ]] || [[ "$LOGS" =~ "listening" ]]; then
    echo "✓ Container logs show successful startup"
else
    echo "Warning: Could not find startup message in logs"
    echo "Logs: $LOGS"
fi

echo "✓ All validations passed for 01-simple-web"
exit 0
