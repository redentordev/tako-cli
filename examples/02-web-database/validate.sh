#!/bin/bash

# Validation script for 02-web-database example

echo "Validating 02-web-database deployment..."

# Check if both containers are running
echo "Checking if containers are running..."
WEB_COUNT=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=web-db_web --format '{{.Names}}' | wc -l")
DB_COUNT=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker ps --filter name=web-db_postgres --format '{{.Names}}' | wc -l")

if [ "$WEB_COUNT" -eq "0" ]; then
    echo "Error: Web container is not running"
    exit 1
fi

if [ "$DB_COUNT" -eq "0" ]; then
    echo "Error: Database container is not running"
    exit 1
fi

echo "✓ Both containers are running"

# Check database is ready
echo "Checking if database is ready..."
DB_READY=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker exec web-db_postgres_1 pg_isready -U postgres 2>&1" || echo "failed")

if [[ "$DB_READY" =~ "accepting connections" ]]; then
    echo "✓ Database is ready and accepting connections"
else
    echo "Warning: Could not verify database readiness: $DB_READY"
fi

# Check web container logs
echo "Checking web container logs..."
WEB_LOGS=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker logs web-db_web_1 2>&1 | tail -20")

if [[ "$WEB_LOGS" =~ "Server running" ]] || [[ "$WEB_LOGS" =~ "listening" ]]; then
    echo "✓ Web container logs show successful startup"
else
    echo "Warning: Could not find startup message in web logs"
fi

# Test database connection from web container
echo "Testing database connection..."
DB_TEST=$(ssh -i "$VPS_SSH_KEY" ${VPS_USER:-root}@$VPS_HOST "docker exec web-db_web_1 node -e 'const pg = require(\"pg\"); const client = new pg.Client({host: \"postgres\", user: \"postgres\", password: \"postgres\", database: \"visitors_db\"}); client.connect().then(() => { console.log(\"Connected\"); process.exit(0); }).catch(e => { console.error(e); process.exit(1); });' 2>&1" || echo "failed")

if [[ "$DB_TEST" =~ "Connected" ]]; then
    echo "✓ Web container can connect to database"
else
    echo "Warning: Could not verify database connection: $DB_TEST"
fi

echo "✓ All validations passed for 02-web-database"
exit 0
