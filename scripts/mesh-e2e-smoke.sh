#!/usr/bin/env bash
#
# Fast, non-remote smoke checks for the mesh E2E harness.

set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"
APP_DIR="$REPO_DIR/examples/01-simple-web"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/tako-mesh-e2e-smoke.XXXXXX")"
TAKO_BIN="$TMP_ROOT/tako"
SMOKE_HOME="$TMP_ROOT/home"

cleanup() {
  rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

fail() {
  echo "error: $*" >&2
  exit 1
}

echo "Building smoke-test tako binary..."
(cd "$REPO_DIR" && go build -o "$TAKO_BIN" .)
mkdir -p "$SMOKE_HOME/.ssh"
touch "$SMOKE_HOME/.ssh/id_ed25519"
chmod 600 "$SMOKE_HOME/.ssh/id_ed25519"

run_guard_check() {
  local phase="$1"
  local output="$TMP_ROOT/$phase.out"
  local log_dir="$TMP_ROOT/$phase-logs"

  echo "Checking $phase guard..."
  set +e
  SERVER_HOST=example.com \
    LETSENCRYPT_EMAIL=ops@example.com \
    HOME="$SMOKE_HOME" \
    TAKO_E2E_LOG_DIR="$log_dir" \
    "$SCRIPT_DIR/mesh-e2e.sh" \
    --app-dir "$APP_DIR" \
    --tako-bin "$TAKO_BIN" \
    --phases "$phase" \
    --yes >"$output" 2>&1
  local status=$?
  set -e

  if [[ $status -eq 0 ]]; then
    cat "$output" >&2
    fail "$phase guard unexpectedly passed for a one-node config"
  fi

  local expected="phase '$phase' requires at least 2 configured environment nodes; validate reported 1"
  if ! grep -Fq "$expected" "$output"; then
    cat "$output" >&2
    fail "$phase guard did not report the expected server-count error"
  fi

  if grep -REq "two-node setup|offline baseline target status|Stopping takod|upgrade servers|state repair|deploy --yes" "$output" "$log_dir" 2>/dev/null; then
    cat "$output" >&2
    fail "$phase guard reached remote or mutating work"
  fi
}

run_guard_check two-node
run_guard_check offline

echo "Mesh E2E smoke checks passed."
