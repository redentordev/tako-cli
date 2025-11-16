#!/bin/bash
# Plausible Deployment Diagnostics
# Run this script to diagnose access issues

echo "=================================================="
echo "Plausible Analytics - Deployment Diagnostics"
echo "=================================================="
echo ""

# Check environment variables
echo "1. Checking environment variables..."
if [ -f .env ]; then
    echo "   ✓ .env file exists"
    if grep -q "SERVER_HOST=" .env; then
        SERVER_HOST=$(grep "SERVER_HOST=" .env | cut -d'=' -f2)
        echo "   ✓ SERVER_HOST = $SERVER_HOST"
        PLAUSIBLE_URL="https://plausible.${SERVER_HOST}.sslip.io"
        echo "   → Your Plausible URL: $PLAUSIBLE_URL"
    else
        echo "   ✗ SERVER_HOST not found in .env"
    fi
else
    echo "   ✗ .env file not found - copy .env.example to .env"
    exit 1
fi
echo ""

# Check if deployed
echo "2. Checking deployment status..."
if command -v tako &> /dev/null; then
    echo "   Running: tako ps --env production"
    tako ps --env production
else
    echo "   ⚠ Tako CLI not found in PATH"
fi
echo ""

# Check DNS resolution
echo "3. Testing DNS resolution..."
if command -v nslookup &> /dev/null; then
    echo "   Resolving: plausible.${SERVER_HOST}.sslip.io"
    nslookup "plausible.${SERVER_HOST}.sslip.io" | grep -A 2 "Name:"
else
    echo "   ⚠ nslookup not available"
fi
echo ""

# Check if server is reachable
echo "4. Testing server connectivity..."
if command -v nc &> /dev/null; then
    echo "   Testing SSH (port 22)..."
    nc -zv -w3 "$SERVER_HOST" 22 2>&1 | grep -i "succeeded\|open"
    
    echo "   Testing HTTP (port 80)..."
    nc -zv -w3 "$SERVER_HOST" 80 2>&1 | grep -i "succeeded\|open"
    
    echo "   Testing HTTPS (port 443)..."
    nc -zv -w3 "$SERVER_HOST" 443 2>&1 | grep -i "succeeded\|open"
elif command -v telnet &> /dev/null; then
    echo "   Using telnet to test ports..."
    timeout 3 telnet "$SERVER_HOST" 22 2>&1 | grep -i "connected"
else
    echo "   Trying ping..."
    ping -c 3 "$SERVER_HOST" 2>&1 | grep "bytes from"
fi
echo ""

# Try to access the URL
echo "5. Testing HTTP(S) access..."
if command -v curl &> /dev/null; then
    echo "   Testing: $PLAUSIBLE_URL"
    HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 "$PLAUSIBLE_URL")
    echo "   HTTP Status Code: $HTTP_CODE"
    
    if [ "$HTTP_CODE" = "200" ] || [ "$HTTP_CODE" = "301" ] || [ "$HTTP_CODE" = "302" ]; then
        echo "   ✓ Server is responding!"
        echo ""
        echo "   Try accessing: $PLAUSIBLE_URL"
    elif [ "$HTTP_CODE" = "000" ]; then
        echo "   ✗ Cannot connect to server (connection timeout or refused)"
    elif [ "$HTTP_CODE" = "404" ]; then
        echo "   ✗ 404 Not Found - Service might not be deployed or Traefik not configured"
    elif [ "$HTTP_CODE" = "502" ] || [ "$HTTP_CODE" = "503" ]; then
        echo "   ✗ $HTTP_CODE - Service is down or not healthy"
    else
        echo "   ⚠ Unexpected status code: $HTTP_CODE"
    fi
else
    echo "   ⚠ curl not available"
fi
echo ""

echo "=================================================="
echo "Common Issues & Solutions:"
echo "=================================================="
echo ""
echo "1. Connection timeout/refused:"
echo "   → Check firewall: sudo ufw allow 80,443/tcp"
echo "   → Verify services are running: tako ps"
echo "   → Check server is accessible: ping $SERVER_HOST"
echo ""
echo "2. 404 Not Found:"
echo "   → Deploy first: tako deploy --env production"
echo "   → Check Traefik logs: ssh root@$SERVER_HOST 'docker logs traefik'"
echo ""
echo "3. 502/503 Bad Gateway:"
echo "   → Service not healthy: tako logs plausible"
echo "   → Check dependencies: tako logs postgres"
echo "   → Check migrations ran: tako logs plausible | grep migrate"
echo ""
echo "4. SSL Certificate issues:"
echo "   → Wait 1-2 minutes after first deployment"
echo "   → Check Traefik logs for Let's Encrypt errors"
echo ""
echo "Need more help? Run these commands on your server:"
echo "  ssh root@$SERVER_HOST 'docker ps'"
echo "  ssh root@$SERVER_HOST 'docker logs traefik'"
echo "  ssh root@$SERVER_HOST 'docker logs plausible_production_plausible_1'"
echo "=================================================="
