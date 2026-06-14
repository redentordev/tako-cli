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
export APP_SERVER_HOST="${APP_SERVER_HOST:-203.0.113.20}"
export APP_SSH_KEY="${APP_SSH_KEY:-$SSH_KEY}"
export EDGE_SERVER_HOST="${EDGE_SERVER_HOST:-203.0.113.30}"
export EDGE_SSH_KEY="${EDGE_SSH_KEY:-$SSH_KEY}"
export LETSENCRYPT_EMAIL="${LETSENCRYPT_EMAIL:-ops@example.com}"
export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-example-postgres-password}"
export JARDIN_ADMIN_HOST="${JARDIN_ADMIN_HOST:-admin.example.com}"
export JARDIN_SITE_HOST="${JARDIN_SITE_HOST:-sites.example.com}"
export JARDIN_ENV_FILE="${JARDIN_ENV_FILE:-.env.example}"

echo "Validating deployment pattern templates..."
go build -o "$TMP_DIR/test-config" "$ROOT/cmd/test-config"

while IFS= read -r config_path; do
  pattern_dir="$(dirname "$config_path")"
  pattern_name="${pattern_dir#"$PATTERNS_DIR"/}"
  echo "  - $pattern_name"
  (
    cd "$pattern_dir"
    "$TMP_DIR/test-config" tako.yaml >/dev/null
  )
done < <(find "$PATTERNS_DIR" -mindepth 2 -maxdepth 3 -name tako.yaml -print | sort)

echo "Running deployment pattern assertions..."
(
  cd "$ROOT"
  go test ./examples
)

echo "Deployment pattern templates validated."
