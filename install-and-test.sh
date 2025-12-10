#!/bin/bash
# Tako Security Test - Install dependencies and run
# Usage: sudo ./install-and-test.sh <target_ip> <domain>
# Example: sudo ./install-and-test.sh 77.42.21.99 hono.77.42.21.99.sslip.io

TARGET="${1:-77.42.21.99}"
DOMAIN="${2:-hono.77.42.21.99.sslip.io}"

if [ "$EUID" -ne 0 ]; then
    echo "Run with sudo: sudo $0 $TARGET $DOMAIN"
    exit 1
fi

echo "Installing dependencies..."
apt-get update -qq
apt-get install -y nmap hydra sslscan netcat-openbsd curl openssl jq

echo ""
echo "Running security test against $TARGET ($DOMAIN)..."
echo ""

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
bash "$SCRIPT_DIR/scripts/security-test.sh" "$TARGET" "$DOMAIN"
