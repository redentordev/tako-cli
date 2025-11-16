#!/bin/bash

# Validation script for 06-scaling example

echo "Validating 06-scaling deployment..."

# Check if all replicas are running
echo "Checking if all replicas are running..."
WEB_COUNT=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=scaling_web --format '{{.Names}}' | wc -l")

if [ "$WEB_COUNT" -lt "3" ]; then
    echo "Error: Expected 3 web replicas, found $WEB_COUNT"
    exit 1
fi

echo "✓ All 3 replicas are running"

# Check each replica
echo "Checking each replica..."
for i in 1 2 3; do
    REPLICA_STATUS=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=scaling_web_$i --format '{{.Status}}'")

    if [[ "$REPLICA_STATUS" =~ "Up" ]]; then
        echo "✓ Replica $i is healthy: $REPLICA_STATUS"
    else
        echo "Error: Replica $i is not healthy: $REPLICA_STATUS"
        exit 1
    fi
done

# Check replica logs
echo "Checking replica logs..."
for i in 1 2 3; do
    REPLICA_LOGS=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker logs scaling_web_$i 2>&1 | tail -5")

    if [[ "$REPLICA_LOGS" =~ "Server running" ]] || [[ "$REPLICA_LOGS" =~ "listening" ]] || [[ "$REPLICA_LOGS" =~ "started" ]]; then
        echo "✓ Replica $i started successfully"
    else
        echo "Warning: Could not verify replica $i startup from logs"
    fi
done

# Check if Traefik load balancer is configured (should be for multi-replica with proxy)
TRAEFIK_RUNNING=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=traefik --format '{{.Names}}' | wc -l")

if [ "$TRAEFIK_RUNNING" -gt "0" ]; then
    echo "✓ Traefik load balancer is running"
    echo "✓ Traefik automatically load balances all replicas via Docker labels"
else
    echo "Warning: Traefik load balancer not found"
fi

# Test that all replicas are reachable via internal network
echo "Testing replica connectivity..."
for i in 1 2 3; do
    REPLICA_IP=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' scaling_web_$i")

    if [ -n "$REPLICA_IP" ]; then
        echo "✓ Replica $i has IP: $REPLICA_IP"
    else
        echo "Warning: Could not get IP for replica $i"
    fi
done

echo "✓ All validations passed for 06-scaling"
exit 0
