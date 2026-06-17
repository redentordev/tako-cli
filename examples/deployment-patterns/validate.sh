#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PATTERNS_DIR="$ROOT/examples/deployment-patterns"
TMP_DIR="$(mktemp -d)"

cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

TEST_SSH_KEY="$TMP_DIR/id_ed25519"
printf '%s\n' "test-key" > "$TEST_SSH_KEY"
chmod 600 "$TEST_SSH_KEY"

export SERVER_HOST="${SERVER_HOST:-203.0.113.10}"
export TAKO_SERVER_HOST="${TAKO_SERVER_HOST:-$SERVER_HOST}"
export SSH_KEY="${SSH_KEY:-$TEST_SSH_KEY}"
export TAKO_SSH_KEY="${TAKO_SSH_KEY:-$SSH_KEY}"
export LETSENCRYPT_EMAIL="${LETSENCRYPT_EMAIL:-ops@example.com}"
export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-example-postgres-password}"

echo "Validating deployment pattern templates..."
go build -o "$TMP_DIR/tako" "$ROOT"

for config_path in "$PATTERNS_DIR"/*/tako.yaml; do
  pattern_dir="$(dirname "$config_path")"
  pattern_name="$(basename "$pattern_dir")"
  echo "  - $pattern_name"
  (
    cd "$pattern_dir"
    TAKO_SKIP_UPDATE_CHECK=1 "$TMP_DIR/tako" --config tako.yaml --env production validate --quiet
  )
done

echo "Running deployment pattern assertions..."
(
  cd "$ROOT"
  go test ./examples
)

echo "Deployment pattern templates validated."
