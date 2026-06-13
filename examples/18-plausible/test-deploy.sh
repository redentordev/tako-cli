#!/bin/bash
# Quick test deployment script for Plausible
set -e

echo "🔍 Checking prerequisites..."

# Check if .env exists
if [ ! -f .env ]; then
    echo "❌ .env file not found!"
    echo "Creating .env from .env.example..."
    cp .env.example .env
    echo "⚠️  Please edit .env with your actual values!"
    exit 1
fi

# Check if SERVER_HOST is set
if ! grep -q "SERVER_HOST=.*[0-9]" .env; then
    echo "❌ SERVER_HOST not configured in .env"
    echo "Please set your server IP address"
    exit 1
fi

# Check if passwords are still default
if grep -q "change_me" .env; then
    echo "❌ Default passwords found in .env"
    echo "Please generate secure passwords:"
    echo "  openssl rand -base64 32"
    exit 1
fi

echo "✅ Configuration looks good!"
echo ""
echo "🚀 Deploying Plausible Analytics..."
echo ""

# Deploy
cd "$(dirname "$0")"
../../tako deploy --env production --verbose

echo ""
echo "✅ Deployment complete!"
echo ""
echo "📋 Next steps:"
echo "  1. Check status: ../../tako ps --env production"
echo "  2. View logs: ../../tako logs --service plausible --env production"
echo "  3. Wait 2-3 minutes for databases to initialize"
echo ""
SERVER_IP=$(grep SERVER_HOST .env | cut -d= -f2)
echo "🌐 Access Plausible at: https://plausible.${SERVER_IP}.sslip.io"
