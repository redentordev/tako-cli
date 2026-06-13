#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

echo "Validating 03-fullstack deployment through Tako..."

command -v tako >/dev/null 2>&1 || {
  echo "Error: tako CLI is not installed or not in PATH"
  exit 1
}

ps_output="$(tako ps)"
printf '%s\n' "$ps_output"

check_service() {
  local service="$1"
  local expected="$2"
  if ! grep -E "^${service}[[:space:]]+${expected}/${expected}[[:space:]].*running" <<<"$ps_output" >/dev/null; then
    echo "Error: ${service} service is not running at ${expected}/${expected} replicas"
    exit 1
  fi
}

check_service web 1
check_service api 2
check_service postgres 1
check_service redis 1

echo "Checking recent web and api logs through takod..."
tako logs --service web --tail 20 >/dev/null
tako logs --service api --tail 20 >/dev/null

if [ -n "${TAKO_VALIDATE_URL:-}" ]; then
  echo "Checking public endpoint: $TAKO_VALIDATE_URL"
  curl -fsS "$TAKO_VALIDATE_URL" >/dev/null
fi

echo "All validations passed for 03-fullstack"
