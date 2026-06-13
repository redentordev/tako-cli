#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

echo "Validating 02-web-database deployment through Tako..."

command -v tako >/dev/null 2>&1 || {
  echo "Error: tako CLI is not installed or not in PATH"
  exit 1
}

ps_output="$(tako ps)"
printf '%s\n' "$ps_output"

check_service() {
  local service="$1"
  if ! grep -E "^${service}[[:space:]]+1/1[[:space:]].*running" <<<"$ps_output" >/dev/null; then
    echo "Error: ${service} service is not running at 1/1 replicas"
    exit 1
  fi
}

check_service web
check_service postgres

echo "Checking recent web logs through takod..."
tako logs --service web --tail 20 >/dev/null

if [ -n "${TAKO_VALIDATE_URL:-}" ]; then
  echo "Checking public endpoint: $TAKO_VALIDATE_URL"
  curl -fsS "$TAKO_VALIDATE_URL" >/dev/null
fi

echo "All validations passed for 02-web-database"
