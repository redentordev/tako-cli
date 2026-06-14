#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

echo "Validating 06-scaling deployment through Tako..."

command -v tako >/dev/null 2>&1 || {
  echo "Error: tako CLI is not installed or not in PATH"
  exit 1
}

ps_output="$(tako ps)"
printf '%s\n' "$ps_output"

if ! grep -E '^web[[:space:]]+3/3[[:space:]].*running' <<<"$ps_output" >/dev/null; then
  echo "Error: web service is not running at 3/3 replicas"
  exit 1
fi

echo "Checking recent web logs and stats through takod..."
tako logs --service web --tail 50 >/dev/null
tako metrics --server prod >/dev/null

if [ -n "${TAKO_VALIDATE_URL:-}" ]; then
  echo "Checking public endpoint: $TAKO_VALIDATE_URL"
  curl -fsS "$TAKO_VALIDATE_URL/health" >/dev/null
fi

echo "All validations passed for 06-scaling"
