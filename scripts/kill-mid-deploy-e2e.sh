#!/usr/bin/env bash
#
# SIGKILL kill-mid-deploy recovery proof.
#
# Deploys a scratch app to a real takod node, SIGKILLs the CLI while the
# apply is in flight, then proves the documented recovery story:
#
#   1. `tako history` shows the interrupted deployment as `in_progress`.
#   2. The next deploy is blocked by the still-held remote lease (exit 3).
#   3. `tako state lease release --id <id> --force` clears the lease.
#   4. The next deploy succeeds (exit 0) and history records `success`.
#
# The scratch app uses a throwaway project name and is decommissioned with
# `tako destroy --yes` at the end; existing apps on the node are untouched.
#
# Required environment:
#   TAKO_E2E_HOST        Target node SSH host.
#   TAKO_E2E_SSH_KEY     SSH private key path for the node.
#   TAKO_E2E_CONFIRM=run Required; this script mutates the node.
# Optional:
#   TAKO_E2E_SSH_USER    SSH user. Defaults to root.
#   TAKO_E2E_TAKO_BIN    Existing tako binary. Defaults to a local build.
#   TAKO_E2E_LOG_DIR     Directory for logs. Defaults to a temp dir.
#   TAKO_E2E_KILL_DELAY  Seconds to wait after the first service starts
#                        deploying before SIGKILL. Defaults to 5.

set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

HOST="${TAKO_E2E_HOST:-}"
SSH_USER="${TAKO_E2E_SSH_USER:-root}"
SSH_KEY="${TAKO_E2E_SSH_KEY:-}"
CONFIRM="${TAKO_E2E_CONFIRM:-}"
TAKO_BIN="${TAKO_E2E_TAKO_BIN:-}"
LOG_DIR="${TAKO_E2E_LOG_DIR:-}"
KILL_DELAY="${TAKO_E2E_KILL_DELAY:-5}"

PROJECT_NAME="tako-kill-proof"
STEP=0
APP_DIR=""
DEPLOY_PID=""

log()  { printf '\n[%s] %s\n' "$(date -u '+%H:%M:%S')" "$*"; }
step() { STEP=$((STEP + 1)); log "step $STEP: $*"; }
fail() { log "FAIL: $*"; exit 1; }

json_get() {
  # json_get <file> <python expression over doc>
  python3 -c 'import json,sys; doc=json.load(open(sys.argv[1])); print(eval(sys.argv[2]))' "$1" "$2"
}

cleanup() {
  if [[ -n "$DEPLOY_PID" ]] && kill -0 "$DEPLOY_PID" 2>/dev/null; then
    kill -9 -- "-$DEPLOY_PID" 2>/dev/null || kill -9 "$DEPLOY_PID" 2>/dev/null || true
  fi
  if [[ -n "$APP_DIR" && -d "$APP_DIR" ]]; then
    log "cleanup: destroying scratch app $PROJECT_NAME"
    (cd "$APP_DIR" && run_tako destroy --yes >"$LOG_DIR/destroy.log" 2>&1) ||
      log "cleanup: destroy failed; inspect $LOG_DIR/destroy.log and remove $PROJECT_NAME manually"
  fi
}
trap cleanup EXIT

run_tako() {
  TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 "$TAKO_BIN" "$@"
}

[[ -n "$HOST" ]] || fail "TAKO_E2E_HOST is required"
[[ -n "$SSH_KEY" ]] || fail "TAKO_E2E_SSH_KEY is required"
[[ "$CONFIRM" == "run" ]] || fail "set TAKO_E2E_CONFIRM=run to allow this mutating proof"
command -v python3 >/dev/null || fail "python3 is required"

if [[ -z "$LOG_DIR" ]]; then
  LOG_DIR="$(mktemp -d "${TMPDIR:-/tmp}/tako-kill-proof-logs.XXXXXX")"
fi
mkdir -p "$LOG_DIR"
log "logs: $LOG_DIR"

step "build tako binary"
if [[ -z "$TAKO_BIN" ]]; then
  TAKO_BIN="$LOG_DIR/tako"
  (cd "$REPO_DIR" && go build -o "$TAKO_BIN" .)
fi
"$TAKO_BIN" --version >/dev/null || fail "tako binary is not runnable"

step "generate scratch app ($PROJECT_NAME)"
APP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/tako-kill-proof-app.XXXXXX")"
NONCE="$(date -u '+%Y%m%dT%H%M%SZ')"
printf '%s\n' "$NONCE" >"$APP_DIR/nonce.txt"
cat >"$APP_DIR/Dockerfile" <<'EOF'
FROM busybox:1.36
COPY nonce.txt /nonce.txt
RUN sleep 35
CMD ["sh", "-c", "while true; do sleep 3600; done"]
EOF
cat >"$APP_DIR/tako.yaml" <<EOF
project:
  name: $PROJECT_NAME
  version: 1.0.0

servers:
  node1:
    host: $HOST
    user: $SSH_USER
    sshKey: $SSH_KEY

environments:
  production:
    servers: [node1]
    services:
      worker:
        build: .
EOF
git -C "$APP_DIR" init -q
git -C "$APP_DIR" add .
git -C "$APP_DIR" -c user.name=tako-e2e -c user.email=tako-e2e@localhost \
  commit -qm "kill-mid-deploy proof $NONCE"

step "resolve server SSH fields via internal e2e-server-ssh harness"
(cd "$APP_DIR" && run_tako internal e2e-server-ssh --server node1) | tee "$LOG_DIR/server-ssh.txt"
RESOLVED_HOST="$(head -1 "$LOG_DIR/server-ssh.txt")"
[[ "$RESOLVED_HOST" == "$HOST" ]] || fail "resolved host $RESOLVED_HOST != $HOST"

step "start deploy and SIGKILL it mid-apply"
# The deploy runs in its own session/process group so the SIGKILL takes out
# the CLI and its ssh children together, the way a crashed operator machine
# would. Killing only the CLI leaves ssh children holding the local
# .tako/.lock flock, which is a different (weaker) failure than this proof
# is about.
(cd "$APP_DIR" && exec python3 -c '
import os, sys
try:
    os.setsid()
except OSError:
    pass
os.execvp(sys.argv[1], sys.argv[1:])
' env TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 "$TAKO_BIN" deploy --events ndjson \
  >"$LOG_DIR/deploy-killed.ndjson" 2>"$LOG_DIR/deploy-killed.log") &
DEPLOY_PID=$!
DEADLINE=$((SECONDS + 300))
while ! grep -q '"type":"deploy.service.started"' "$LOG_DIR/deploy-killed.ndjson" 2>/dev/null; do
  kill -0 "$DEPLOY_PID" 2>/dev/null || fail "deploy exited before the apply phase; see $LOG_DIR/deploy-killed.log"
  [[ $SECONDS -lt $DEADLINE ]] || fail "timed out waiting for deploy.service.started"
  sleep 1
done
sleep "$KILL_DELAY"
kill -0 "$DEPLOY_PID" 2>/dev/null || fail "deploy finished before SIGKILL; widen the build window"
kill -9 -- "-$DEPLOY_PID" 2>/dev/null || kill -9 "$DEPLOY_PID"
wait "$DEPLOY_PID" 2>/dev/null || true
DEPLOY_PID=""
log "deploy SIGKILLed mid-apply"

step "history shows the interrupted deployment as in_progress"
(cd "$APP_DIR" && run_tako history --output json -n 3 \
  >"$LOG_DIR/history-after-kill.json" 2>>"$LOG_DIR/history.log")
LATEST_STATUS="$(json_get "$LOG_DIR/history-after-kill.json" 'doc["deployments"][0]["status"]')"
[[ "$LATEST_STATUS" == "in_progress" ]] || fail "latest history status = $LATEST_STATUS, want in_progress"
log "latest deployment status: in_progress"

step "next deploy is blocked by the held lease (exit 3)"
set +e
(cd "$APP_DIR" && run_tako deploy --output json \
  >"$LOG_DIR/deploy-locked.json" 2>"$LOG_DIR/deploy-locked.log")
LOCKED_EXIT=$?
set -e
[[ "$LOCKED_EXIT" -eq 3 ]] || fail "deploy under held lease exited $LOCKED_EXIT, want 3 (locked)"
log "locked deploy exit code: 3"

step "inspect and force-release the abandoned lease"
(cd "$APP_DIR" && run_tako state lease --output json \
  >"$LOG_DIR/lease.json" 2>>"$LOG_DIR/lease.log")
LEASE_ID="$(json_get "$LOG_DIR/lease.json" '[n["lease"]["id"] for n in doc["nodes"] if n.get("lease")][0]')"
[[ -n "$LEASE_ID" ]] || fail "no lease found after killed deploy"
log "abandoned lease: $LEASE_ID"
(cd "$APP_DIR" && run_tako state lease release --id "$LEASE_ID" --force --output json \
  >"$LOG_DIR/lease-release.json" 2>>"$LOG_DIR/lease.log")

step "deploy succeeds after lease release"
(cd "$APP_DIR" && run_tako deploy --output json \
  >"$LOG_DIR/deploy-recovered.json" 2>"$LOG_DIR/deploy-recovered.log")
RECOVERED_STATUS="$(json_get "$LOG_DIR/deploy-recovered.json" 'doc["status"]')"
[[ "$RECOVERED_STATUS" == "success" ]] || fail "recovered deploy status = $RECOVERED_STATUS, want success"
(cd "$APP_DIR" && run_tako history --output json -n 3 \
  >"$LOG_DIR/history-after-recovery.json" 2>>"$LOG_DIR/history.log")
FINAL_STATUS="$(json_get "$LOG_DIR/history-after-recovery.json" 'doc["deployments"][0]["status"]')"
[[ "$FINAL_STATUS" == "success" ]] || fail "post-recovery history status = $FINAL_STATUS, want success"

log "PASS: SIGKILL mid-deploy leaves in_progress history + held lease; force-release recovers"
