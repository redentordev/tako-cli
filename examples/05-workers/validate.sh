#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

echo "Validating 05-workers deployment through Tako..."

command -v tako >/dev/null 2>&1 || {
  echo "Error: tako CLI is not installed or not in PATH"
  exit 1
}

ps_output="$(tako ps)"
printf '%s\n' "$ps_output"

if ! grep -E '^worker[[:space:]]+3/3[[:space:]].*running' <<<"$ps_output" >/dev/null; then
  echo "Error: worker service is not running at 3/3 replicas"
  exit 1
fi

if ! grep -E '^redis[[:space:]]+1/1[[:space:]].*running' <<<"$ps_output" >/dev/null; then
  echo "Error: redis service is not running at 1/1 replicas"
  exit 1
fi

echo "Checking recent worker logs through takod..."
tako logs --service worker --tail 50 >/dev/null

echo "All validations passed for 05-workers"
