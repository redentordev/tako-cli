#!/bin/bash
# Laravel Deployment Test Script
# Run this when server is accessible

set -e

echo "🧪 Laravel Deployment Test"
echo "=========================="

# Check .env exists
if [ ! -f .env ]; then
    echo "❌ .env not found. Creating from .env.example..."
    cp .env.example .env
    echo "⚠️  Please edit .env with:"
    echo "   - SERVER_HOST (your server IP)"
    echo "   - LETSENCRYPT_EMAIL"
    echo "   - APP_KEY (generate with: php artisan key:generate --show)"
    exit 1
fi

# Verify required vars
if ! grep -q "SERVER_HOST=.*[0-9]" .env; then
    echo "❌ SERVER_HOST not set in .env"
    exit 1
fi

if grep -q "APP_KEY=base64:generated_key_placeholder" .env; then
    echo "❌ APP_KEY not generated. Run: php artisan key:generate --show"
    exit 1
fi

echo "✅ Configuration OK"
echo ""

# Test SSH connection
echo "🔐 Testing SSH connection..."
SERVER_IP=$(grep SERVER_HOST .env | cut -d= -f2)
if ssh -o ConnectTimeout=5 -o BatchMode=yes root@$SERVER_IP "echo connected" 2>/dev/null; then
    echo "✅ SSH connection successful"
else
    echo "❌ Cannot connect to server via SSH"
    echo "   Server: $SERVER_IP"
    echo "   Check:"
    echo "   1. Server is running"
    echo "   2. SSH key is correct (~/.ssh/id_ed25519)"
    echo "   3. Firewall allows port 22"
    exit 1
fi

echo ""
echo "🚀 Deploying Laravel..."
../../tako deploy --env production --verbose

echo ""
echo "✅ Deployment complete!"
echo ""
echo "📋 Verification steps:"
echo "1. Check status: ../../tako ps --env production"
echo "2. View logs: ../../tako logs --service web --env production"
echo "3. Test URL: https://laravel.${SERVER_IP}.sslip.io"
echo ""
echo "Expected: Laravel welcome page"
