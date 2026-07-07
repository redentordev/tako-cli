#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"

cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

VALIDATION_HOME="$TMP_DIR/home"
TEST_SSH_KEY="$VALIDATION_HOME/.ssh/id_ed25519"
mkdir -p "$VALIDATION_HOME/.ssh"
printf '%s\n' "test-key" > "$TEST_SSH_KEY"
chmod 600 "$TEST_SSH_KEY"

export SERVER_HOST="${SERVER_HOST:-203.0.113.10}"
export TAKO_SERVER_HOST="${TAKO_SERVER_HOST:-$SERVER_HOST}"
export SERVER1_HOST="${SERVER1_HOST:-203.0.113.11}"
export SERVER2_HOST="${SERVER2_HOST:-203.0.113.12}"
export SERVER_IP="${SERVER_IP:-$SERVER_HOST}"
export HOSTNAME="${HOSTNAME:-example-worker}"
export SERVER_USER="${SERVER_USER:-root}"
export SSH_KEY="${SSH_KEY:-$TEST_SSH_KEY}"
export TAKO_SSH_KEY="${TAKO_SSH_KEY:-$SSH_KEY}"
export LETSENCRYPT_EMAIL="${LETSENCRYPT_EMAIL:-ops@example.com}"
export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-example-postgres-password}"
export MYSQL_PASSWORD="${MYSQL_PASSWORD:-example-mysql-password}"
export MYSQL_ROOT_PASSWORD="${MYSQL_ROOT_PASSWORD:-example-mysql-root-password}"
export CLICKHOUSE_PASSWORD="${CLICKHOUSE_PASSWORD:-example-clickhouse-password}"
export APP_KEY="${APP_KEY:-base64:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=}"
export APP_SECRET="${APP_SECRET:-example-app-secret}"
export MAILER_EMAIL="${MAILER_EMAIL:-ops@example.com}"
export SECRET_KEY_BASE="${SECRET_KEY_BASE:-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}"
export REGISTRY_USER="${REGISTRY_USER:-example-registry-user}"
export REGISTRY_TOKEN="${REGISTRY_TOKEN:-example-registry-token}"

validate_config() {
  local config_path="$1"
  local rel_path="${config_path#"$ROOT"/}"
  local dir
  local file

  dir="$(dirname "$config_path")"
  file="$(basename "$config_path")"

  echo "  - $rel_path"
  (
    cd "$dir"
    HOME="$VALIDATION_HOME" TAKO_SKIP_UPDATE_CHECK=1 "$TMP_DIR/tako" --config "$file" --env production validate --quiet
  )
}

echo "Building validation binary..."
go build -o "$TMP_DIR/tako" "$ROOT"

echo "Validating top-level example configs..."
find "$ROOT/examples" -mindepth 2 -maxdepth 2 \( -name 'tako.yaml' -o -name 'tako-parallel.yaml' \) -print | sort | while IFS= read -r config_path; do
  validate_config "$config_path"
done

echo "Validating deployment pattern templates..."
find "$ROOT/examples/deployment-patterns" -mindepth 2 -maxdepth 2 -name 'tako.yaml' -print | sort | while IFS= read -r config_path; do
  validate_config "$config_path"
done

echo "Running deployment pattern assertions..."
(
  cd "$ROOT"
  go test ./examples
)

echo "Example configs validated."
