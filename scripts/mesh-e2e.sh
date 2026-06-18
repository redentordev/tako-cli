#!/usr/bin/env bash
#
# Run the meshed takod deployment proof checklist against a real app repo.
#
# Safe default:
#   scripts/mesh-e2e.sh
#
# Full mutating run:
#   scripts/mesh-e2e.sh --app-dir /path/to/app --env production --phases standard --yes
#
# Useful environment variables:
#   TAKO_E2E_APP_DIR          App repo containing tako.yaml. Defaults to $PWD.
#   TAKO_E2E_ENVIRONMENT      Environment name. Defaults to production.
#   TAKO_E2E_PHASES           Comma-separated phases. Defaults to preflight.
#   TAKO_E2E_TAKO_BIN         Existing tako binary. Defaults to a local build.
#   TAKO_E2E_TAKOD_BINARY     Linux tako binary to upload for local/non-release agent upgrades.
#   TAKO_E2E_CONFIRM=run      Required for setup/deploy/repair/env phases.
#   TAKO_E2E_CI_HOST_KEY_MODE Host key mode for the ci phase. Defaults to TAKO_HOST_KEY_MODE or tofu.
#   TAKO_E2E_LOG_DIR          Directory for logs. Defaults outside app worktree.
#   TAKO_E2E_KEEP_WORKDIR=1   Keep temporary fresh-clone workdirs.
#   TAKO_E2E_PROTOCOL_URL     Public HTTPS URL for protocol checks.
#   TAKO_E2E_WEBSOCKET_URL    Optional public wss:// URL for WebSocket checks.
#   TAKO_E2E_HTTP3_REQUIRED=1 Fail when curl cannot make HTTP/3 requests.
#   TAKO_E2E_OFFLINE_SERVER   Node name to stop for the offline/rejoin phase.
#   TAKO_E2E_OFFLINE_HOST     SSH host for TAKO_E2E_OFFLINE_SERVER.
#   TAKO_E2E_OFFLINE_USER     SSH user for TAKO_E2E_OFFLINE_SERVER.
#   TAKO_E2E_OFFLINE_PORT     SSH port for TAKO_E2E_OFFLINE_SERVER. Defaults to 22.
#   TAKO_E2E_OFFLINE_SSH_KEY  Optional SSH key for TAKO_E2E_OFFLINE_SERVER.
#   TAKO_E2E_OFFLINE_SSH_OPTS Optional extra ssh options for the offline node.
#   TAKO_E2E_OFFLINE_STOP_CMD Command that makes the node agent unavailable.
#   TAKO_E2E_OFFLINE_START_CMD Command that restores the node agent.
#   TAKO_E2E_OFFLINE_STATUS_CMD Command that succeeds when the node agent is back.

set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

APP_DIR="${TAKO_E2E_APP_DIR:-$PWD}"
ENVIRONMENT="${TAKO_E2E_ENVIRONMENT:-${TAKO_E2E_ENV:-production}}"
PHASES="${TAKO_E2E_PHASES:-preflight}"
TAKO_BIN="${TAKO_E2E_TAKO_BIN:-}"
TAKOD_BINARY="${TAKO_E2E_TAKOD_BINARY:-${TAKO_TAKOD_BINARY:-}}"
CONFIRM="${TAKO_E2E_CONFIRM:-}"
CI_HOST_KEY_MODE="${TAKO_E2E_CI_HOST_KEY_MODE:-${TAKO_HOST_KEY_MODE:-tofu}}"
KEEP_WORKDIR="${TAKO_E2E_KEEP_WORKDIR:-0}"
OFFLINE_SERVER="${TAKO_E2E_OFFLINE_SERVER:-}"
OFFLINE_HOST="${TAKO_E2E_OFFLINE_HOST:-}"
OFFLINE_USER="${TAKO_E2E_OFFLINE_USER:-}"
if [[ -n "${TAKO_E2E_OFFLINE_PORT:-}" ]]; then
  OFFLINE_PORT="$TAKO_E2E_OFFLINE_PORT"
  OFFLINE_PORT_SET=1
else
  OFFLINE_PORT=22
  OFFLINE_PORT_SET=0
fi
OFFLINE_SSH_KEY="${TAKO_E2E_OFFLINE_SSH_KEY:-}"
OFFLINE_SSH_OPTS="${TAKO_E2E_OFFLINE_SSH_OPTS:-}"
OFFLINE_STOP_CMD="${TAKO_E2E_OFFLINE_STOP_CMD:-sudo systemctl stop takod}"
OFFLINE_START_CMD="${TAKO_E2E_OFFLINE_START_CMD:-sudo systemctl start takod}"
OFFLINE_STATUS_CMD="${TAKO_E2E_OFFLINE_STATUS_CMD:-sudo systemctl is-active --quiet takod}"
OFFLINE_RESTORE_NEEDED=0
PROTOCOL_URL="${TAKO_E2E_PROTOCOL_URL:-${TAKO_VALIDATE_URL:-}}"
WEBSOCKET_URL="${TAKO_E2E_WEBSOCKET_URL:-}"
HTTP3_REQUIRED="${TAKO_E2E_HTTP3_REQUIRED:-0}"

LOG_DIR=""
STEP=0
TEMP_DIRS=()
FRESH_CLONE_DIR=""

usage() {
  cat <<'EOF'
Usage:
  scripts/mesh-e2e.sh [options]

Options:
  --app-dir DIR       App repo containing tako.yaml. Defaults to current dir.
  --env NAME, -e NAME Environment to test. Defaults to production.
  --phases LIST       Comma-separated phases to run.
  --tako-bin PATH     Existing tako binary to test.
  --takod-binary PATH Linux tako binary for local/non-release server upgrades.
  --yes               Allow mutating phases.
  --keep-workdir      Keep temporary fresh-clone workdirs.
  --offline-server    Node name to stop for the offline/rejoin phase.
  --offline-host      Override SSH host for the offline node.
  --offline-user      Override SSH user for the offline node.
  --offline-port      Override SSH port for the offline node.
  --offline-ssh-key   Override SSH key for the offline node.
  --protocol-url      Public HTTPS URL for protocol checks.
  --websocket-url     Optional public wss:// URL for WebSocket checks.
  --http3-required    Fail if curl cannot make HTTP/3 requests.
  --help, -h          Show this help.

Phases:
  preflight       validate, doctor, state, lease, and upgrade dry-run checks.
  one-node        setup, upgrade servers, deploy, status, lease, history, ps, drift.
  two-node        setup, upgrade servers, repair, status, lease, deploy, status.
  env             env push, temporary local env removal, env pull --force.
  new-computer    fresh clone, validate, env pull, state pull, status, deploy.
  ci              fresh clone with CI env, validate, upgrade, state pull, deploy.
  repair          state status, state repair, state status.
  invalid-config  prove deploy rejects invalid YAML before remote work.
  protocols       HTTP/1.1, HTTP/2, optional HTTP/3, and optional WebSocket checks.
  offline         stop one takod, prove fail-closed behavior, restart, repair, deploy.
  standard        preflight, invalid-config, one-node, env, new-computer, ci.
  full            standard plus two-node, repair, offline.

Mutating phases require --yes or TAKO_E2E_CONFIRM=run.
The two-node and offline phases require at least two configured environment nodes.
Set TAKO_ENV_PASSPHRASE for env, new-computer, and ci phases.
Set TAKO_E2E_PROTOCOL_URL for protocols phase.
EOF
}

die() {
  echo "error: $*" >&2
  exit 1
}

note() {
  echo "==> $*" >&2
}

slug() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g; s/^-//; s/-$//'
}

cleanup() {
  if declare -F restore_offline_node >/dev/null; then
    restore_offline_node
  fi

  if ((${#TEMP_DIRS[@]} == 0)); then
    return
  fi
  if [[ "$KEEP_WORKDIR" == "1" ]]; then
    for dir in "${TEMP_DIRS[@]}"; do
      echo "kept temp workdir: $dir"
    done
    return
  fi
  for dir in "${TEMP_DIRS[@]}"; do
    rm -rf "$dir"
  done
}
trap cleanup EXIT

while [[ $# -gt 0 ]]; do
  case "$1" in
    --app-dir)
      [[ $# -ge 2 ]] || die "--app-dir requires a value"
      APP_DIR="$2"
      shift 2
      ;;
    --env|-e)
      [[ $# -ge 2 ]] || die "--env requires a value"
      ENVIRONMENT="$2"
      shift 2
      ;;
    --phases)
      [[ $# -ge 2 ]] || die "--phases requires a value"
      PHASES="$2"
      shift 2
      ;;
    --tako-bin)
      [[ $# -ge 2 ]] || die "--tako-bin requires a value"
      TAKO_BIN="$2"
      shift 2
      ;;
    --takod-binary)
      [[ $# -ge 2 ]] || die "--takod-binary requires a value"
      TAKOD_BINARY="$2"
      shift 2
      ;;
    --yes)
      CONFIRM="run"
      shift
      ;;
    --keep-workdir)
      KEEP_WORKDIR="1"
      shift
      ;;
    --offline-server)
      [[ $# -ge 2 ]] || die "--offline-server requires a value"
      OFFLINE_SERVER="$2"
      shift 2
      ;;
    --offline-host)
      [[ $# -ge 2 ]] || die "--offline-host requires a value"
      OFFLINE_HOST="$2"
      shift 2
      ;;
    --offline-user)
      [[ $# -ge 2 ]] || die "--offline-user requires a value"
      OFFLINE_USER="$2"
      shift 2
      ;;
    --offline-port)
      [[ $# -ge 2 ]] || die "--offline-port requires a value"
      OFFLINE_PORT="$2"
      OFFLINE_PORT_SET=1
      shift 2
      ;;
    --offline-ssh-key)
      [[ $# -ge 2 ]] || die "--offline-ssh-key requires a value"
      OFFLINE_SSH_KEY="$2"
      shift 2
      ;;
    --protocol-url)
      [[ $# -ge 2 ]] || die "--protocol-url requires a value"
      PROTOCOL_URL="$2"
      shift 2
      ;;
    --websocket-url)
      [[ $# -ge 2 ]] || die "--websocket-url requires a value"
      WEBSOCKET_URL="$2"
      shift 2
      ;;
    --http3-required)
      HTTP3_REQUIRED="1"
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

APP_DIR="$(cd -- "$APP_DIR" && pwd)"
APP_SLUG="$(basename -- "$APP_DIR" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g; s/^-//; s/-$//')"
[[ -n "$APP_SLUG" ]] || APP_SLUG="app"
LOG_DIR="${TAKO_E2E_LOG_DIR:-${TMPDIR:-/tmp}/tako-e2e-logs/$APP_SLUG/$(date -u +%Y%m%dT%H%M%SZ)}"

require_tool() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

require_app_repo() {
  [[ -f "$APP_DIR/tako.yaml" || -f "$APP_DIR/tako.yml" || -f "$APP_DIR/tako.json" ]] ||
    die "$APP_DIR does not contain tako.yaml, tako.yml, or tako.json"
  (cd "$APP_DIR" && git rev-parse --is-inside-work-tree >/dev/null 2>&1) ||
    die "$APP_DIR must be a git worktree for fresh-clone and deploy checks"
}

require_deploy_ignore_rules() {
  local dir="$1"
  local missing=()

  for path in ".tako" ".env"; do
    if ! (cd "$dir" && git check-ignore -q "$path"); then
      missing+=("$path")
    fi
  done

  if ((${#missing[@]} > 0)); then
    die "$dir must ignore ${missing[*]} before E2E deploy/env phases; add .tako/ and .env to .gitignore"
  fi
}

require_clean_worktree() {
  local dir="$1"
  local status

  status="$(cd "$dir" && git status --porcelain)"
  if [[ -n "$status" ]]; then
    printf '%s\n' "$status" >&2
    die "$dir has uncommitted changes; commit, stash, or discard them before E2E deploy phases"
  fi
}

require_deploy_ready_worktree() {
  local dir="$1"
  require_deploy_ignore_rules "$dir"
  require_clean_worktree "$dir"
}

curl_supports_http3() {
  curl --http3 --version >/dev/null 2>&1
}

require_protocol_url() {
  require_tool curl
  [[ -n "$PROTOCOL_URL" ]] ||
    die "phase 'protocols' requires TAKO_E2E_PROTOCOL_URL or TAKO_VALIDATE_URL"
  case "$PROTOCOL_URL" in
    https://*) ;;
    *) die "phase 'protocols' requires an https:// URL, got: $PROTOCOL_URL" ;;
  esac
  if [[ -n "$WEBSOCKET_URL" ]]; then
    case "$WEBSOCKET_URL" in
      wss://*) ;;
      *) die "phase 'protocols' requires a wss:// WebSocket URL, got: $WEBSOCKET_URL" ;;
    esac
  fi
}

build_or_select_tako() {
  if [[ -n "$TAKO_BIN" ]]; then
    [[ -x "$TAKO_BIN" ]] || die "TAKO binary is not executable: $TAKO_BIN"
    TAKO_BIN="$(cd -- "$(dirname -- "$TAKO_BIN")" && pwd)/$(basename -- "$TAKO_BIN")"
    return
  fi

  require_tool go
  TAKO_BIN="$REPO_DIR/bin/tako-e2e"
  note "Building local tako binary: $TAKO_BIN"
  (cd "$REPO_DIR" && go build -o "$TAKO_BIN" .)
}

normalize_takod_binary() {
  if [[ -z "$TAKOD_BINARY" ]]; then
    return
  fi
  [[ -f "$TAKOD_BINARY" ]] || die "takod binary is not a file: $TAKOD_BINARY"
  TAKOD_BINARY="$(cd -- "$(dirname -- "$TAKOD_BINARY")" && pwd)/$(basename -- "$TAKOD_BINARY")"
}

LAST_LOG_FILE=""

run_cmd_status() {
  local label="$1"
  shift
  STEP=$((STEP + 1))
  local log_file="$LOG_DIR/$(printf '%02d' "$STEP")-$(slug "$label").log"
  LAST_LOG_FILE="$log_file"

  mkdir -p "$LOG_DIR"
  note "$label"
  echo "+ $*" >"$log_file"

  set +e
  "$@" >>"$log_file" 2>&1
  local status=$?
  set -e

  if [[ $status -ne 0 ]]; then
    tail -n 80 "$log_file" >&2 || true
  fi
  return "$status"
}

run_cmd() {
  local label="$1"
  if ! run_cmd_status "$@"; then
    die "$label failed (log: $LAST_LOG_FILE)"
  fi
}

run_tako_in() {
  local dir="$1"
  local label="$2"
  shift 2
  run_cmd "$label" bash -c '
    cd "$1"
    takod_binary="$2"
    shift 2
    if [[ -n "$takod_binary" ]]; then
      export TAKO_TAKOD_BINARY="$takod_binary"
    fi
    TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 "$@"
  ' _ "$dir" "$TAKOD_BINARY" "$TAKO_BIN" --env "$ENVIRONMENT" "$@"
}

run_tako() {
  local label="$1"
  shift
  run_tako_in "$APP_DIR" "$label" "$@"
}

run_tako_status_in() {
  local dir="$1"
  local label="$2"
  shift 2
  run_cmd_status "$label" bash -c '
    cd "$1"
    takod_binary="$2"
    shift 2
    if [[ -n "$takod_binary" ]]; then
      export TAKO_TAKOD_BINARY="$takod_binary"
    fi
    TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 "$@"
  ' _ "$dir" "$TAKOD_BINARY" "$TAKO_BIN" --env "$ENVIRONMENT" "$@"
}

run_tako_status() {
  local label="$1"
  shift
  run_tako_status_in "$APP_DIR" "$label" "$@"
}

run_upgrade_servers_apply_in() {
  local dir="$1"
  local label="$2"
  local args=(upgrade servers)
  if [[ -n "$TAKOD_BINARY" ]]; then
    args+=(--takod-binary "$TAKOD_BINARY")
  fi
  run_tako_in "$dir" "$label" "${args[@]}"
}

run_upgrade_servers_apply() {
  local label="$1"
  run_upgrade_servers_apply_in "$APP_DIR" "$label"
}

run_upgrade_servers_dry_run_in() {
  local dir="$1"
  local label="$2"
  run_tako_in "$dir" "$label" upgrade servers --dry-run
}

run_upgrade_servers_dry_run() {
  local label="$1"
  run_upgrade_servers_dry_run_in "$APP_DIR" "$label"
}

require_min_environment_servers() {
  local phase="$1"
  local minimum="$2"
  local count log_file

  run_tako "validate config for $phase server count" validate
  log_file="$LAST_LOG_FILE"
  count="$(awk -F': ' '/^Servers: / {print $2; exit}' "$log_file")"
  if [[ ! "$count" =~ ^[0-9]+$ ]]; then
    tail -n 80 "$log_file" >&2 || true
    die "phase '$phase' could not determine configured environment server count from validate output"
  fi
  if ((count < minimum)); then
    die "phase '$phase' requires at least $minimum configured environment nodes; validate reported $count"
  fi
}

require_confirm() {
  local phase="$1"
  [[ "$CONFIRM" == "run" ]] ||
    die "phase '$phase' mutates remote/local state; rerun with --yes or TAKO_E2E_CONFIRM=run"
}

require_passphrase() {
  local phase="$1"
  [[ -n "${TAKO_ENV_PASSPHRASE:-}" ]] ||
    die "phase '$phase' requires TAKO_ENV_PASSPHRASE for non-interactive env bundle restore"
}

ssh_offline_node() {
  local remote_cmd="$1"
  local args=("-p" "$OFFLINE_PORT" "-o" "BatchMode=yes")

  if [[ -n "$OFFLINE_SSH_KEY" ]]; then
    args+=("-i" "$OFFLINE_SSH_KEY")
  fi
  if [[ -n "$OFFLINE_SSH_OPTS" ]]; then
    local extra_opts=()
    read -r -a extra_opts <<<"$OFFLINE_SSH_OPTS"
    args+=("${extra_opts[@]}")
  fi

  ssh "${args[@]}" "$OFFLINE_USER@$OFFLINE_HOST" "$remote_cmd"
}

restore_offline_node() {
  if [[ "$OFFLINE_RESTORE_NEEDED" != "1" ]]; then
    return
  fi

  note "Restoring offline node agent: $OFFLINE_SERVER"
  if ! ssh_offline_node "$OFFLINE_START_CMD" >/dev/null 2>&1; then
    echo "warning: failed to restore offline node $OFFLINE_SERVER with: $OFFLINE_START_CMD" >&2
    return
  fi
  OFFLINE_RESTORE_NEEDED=0
}

require_offline_node_control() {
  infer_offline_node_control

  require_tool ssh
  [[ -n "$OFFLINE_SERVER" ]] ||
    die "phase 'offline' requires --offline-server or TAKO_E2E_OFFLINE_SERVER"
  [[ -n "$OFFLINE_HOST" ]] ||
    die "phase 'offline' requires --offline-host or TAKO_E2E_OFFLINE_HOST"
  [[ -n "$OFFLINE_USER" ]] ||
    die "phase 'offline' requires --offline-user or TAKO_E2E_OFFLINE_USER"
}

infer_offline_node_control() {
  if [[ -z "$OFFLINE_SERVER" ]]; then
    return
  fi
  if [[ -n "$OFFLINE_HOST" && -n "$OFFLINE_USER" && "$OFFLINE_PORT_SET" == "1" && -n "$OFFLINE_SSH_KEY" ]]; then
    return
  fi

  local resolved resolved_host resolved_user resolved_port resolved_key
  if ! resolved="$(
    cd "$APP_DIR"
    TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 "$TAKO_BIN" --env "$ENVIRONMENT" internal e2e-server-ssh --server "$OFFLINE_SERVER"
  )"; then
    if [[ -n "${TAKO_E2E_DEBUG:-}" ]]; then
      echo "warning: could not infer offline node SSH settings from tako config" >&2
    fi
    return
  fi

  {
    IFS= read -r resolved_host || true
    IFS= read -r resolved_user || true
    IFS= read -r resolved_port || true
    IFS= read -r resolved_key || true
  } <<<"$resolved"

  [[ -z "$OFFLINE_HOST" && -n "$resolved_host" ]] && OFFLINE_HOST="$resolved_host"
  [[ -z "$OFFLINE_USER" && -n "$resolved_user" ]] && OFFLINE_USER="$resolved_user"
  [[ "$OFFLINE_PORT_SET" == "0" && -n "$resolved_port" ]] && OFFLINE_PORT="$resolved_port"
  [[ -z "$OFFLINE_SSH_KEY" && -n "$resolved_key" ]] && OFFLINE_SSH_KEY="$resolved_key"
  return 0
}

wait_for_offline_status() {
  local wanted="$1"
  local attempt

  for attempt in $(seq 1 30); do
    if ssh_offline_node "$OFFLINE_STATUS_CMD" >/dev/null 2>&1; then
      [[ "$wanted" == "up" ]] && return 0
    else
      [[ "$wanted" == "down" ]] && return 0
    fi
    sleep 2
  done

  return 1
}

fresh_clone() {
  local label="$1"
  local temp_parent
  require_deploy_ready_worktree "$APP_DIR"
  temp_parent="$(mktemp -d "${TMPDIR:-/tmp}/tako-e2e-${label}.XXXXXX")"
  TEMP_DIRS+=("$temp_parent")

  local clone_dir="$temp_parent/app"
  run_cmd "fresh clone for $label" git clone "$APP_DIR" "$clone_dir"
  rm -rf "$clone_dir/.tako"
  rm -f "$clone_dir/.env"
  FRESH_CLONE_DIR="$clone_dir"
}

phase_preflight() {
  run_cmd "tako version" "$TAKO_BIN" --version
  run_tako "validate config" validate
  run_tako "doctor skip remote" doctor --skip-remote
  run_tako "state status" state status
  run_tako "state lease" state lease
  run_upgrade_servers_dry_run "upgrade servers dry-run"
}

phase_one_node() {
  require_confirm "one-node"
  require_deploy_ready_worktree "$APP_DIR"
  run_tako "setup" setup
  run_upgrade_servers_dry_run "upgrade servers dry-run"
  run_upgrade_servers_apply "upgrade servers"
  run_tako "deploy" deploy --yes
  run_tako "state status after deploy" state status
  run_tako "state lease after deploy" state lease
  run_tako "history" history
  run_tako "ps" ps
  run_tako "drift" drift
}

phase_two_node() {
  require_confirm "two-node"
  require_min_environment_servers "two-node" 2
  require_deploy_ready_worktree "$APP_DIR"
  run_tako "two-node setup" setup
  run_upgrade_servers_dry_run "two-node upgrade servers dry-run"
  run_upgrade_servers_apply "two-node upgrade servers"
  run_tako "two-node repair" state repair
  run_tako "two-node status before deploy" state status
  run_tako "two-node lease before deploy" state lease
  run_tako "two-node deploy" deploy --yes
  run_tako "two-node status after deploy" state status
  run_tako "two-node lease after deploy" state lease
}

phase_env() {
  require_confirm "env"
  require_passphrase "env"
  require_deploy_ignore_rules "$APP_DIR"
  run_tako "env push" env push

  local backup=""
  if [[ -f "$APP_DIR/.env" ]]; then
    backup="$LOG_DIR/env.backup"
    cp "$APP_DIR/.env" "$backup"
    rm -f "$APP_DIR/.env"
  fi

  if ! run_tako_status "env pull force" env pull --force; then
    if [[ -n "$backup" ]]; then
      cp "$backup" "$APP_DIR/.env"
    fi
    die "env pull failed; original .env was restored"
  fi

  if [[ -n "$backup" ]]; then
    if [[ ! -f "$APP_DIR/.env" ]]; then
      cp "$backup" "$APP_DIR/.env"
      die "env pull did not restore .env; original .env was restored"
    fi
    if ! cmp -s "$backup" "$APP_DIR/.env"; then
      cp "$APP_DIR/.env" "$LOG_DIR/env.pulled"
      cp "$backup" "$APP_DIR/.env"
      die "env pull restored .env content that differs from the local source; pulled copy saved to $LOG_DIR/env.pulled and original .env restored"
    fi
  fi
}

phase_new_computer() {
  require_confirm "new-computer"
  require_passphrase "new-computer"
  local clone_dir
  fresh_clone "new-computer"
  clone_dir="$FRESH_CLONE_DIR"
  require_deploy_ready_worktree "$clone_dir"
  run_tako_in "$clone_dir" "new-computer validate config" validate
  run_tako_in "$clone_dir" "new-computer env pull" env pull --force
  run_tako_in "$clone_dir" "new-computer state pull" state pull
  run_tako_in "$clone_dir" "new-computer state status" state status
  run_tako_in "$clone_dir" "new-computer state lease" state lease
  run_tako_in "$clone_dir" "new-computer deploy" deploy --yes
}

phase_ci() {
  require_confirm "ci"
  require_passphrase "ci"
  local clone_dir
  fresh_clone "ci"
  clone_dir="$FRESH_CLONE_DIR"
  require_deploy_ready_worktree "$clone_dir"
  run_cmd "ci env and state deploy" bash -c '
    cd "$1"
    ci_host_key_mode="$2"
    takod_binary="$3"
    shift 3
    upgrade_args=()
    if [[ -n "$takod_binary" ]]; then
      export TAKO_TAKOD_BINARY="$takod_binary"
      upgrade_args+=(--takod-binary "$takod_binary")
    fi
    CI=true TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 TAKO_HOST_KEY_MODE="$ci_host_key_mode" "$@" --env "$TAKO_E2E_ENVIRONMENT" validate
    CI=true TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 TAKO_HOST_KEY_MODE="$ci_host_key_mode" "$@" --env "$TAKO_E2E_ENVIRONMENT" upgrade servers --dry-run
    CI=true TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 TAKO_HOST_KEY_MODE="$ci_host_key_mode" "$@" --env "$TAKO_E2E_ENVIRONMENT" upgrade servers "${upgrade_args[@]}"
    CI=true TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 TAKO_HOST_KEY_MODE="$ci_host_key_mode" "$@" --env "$TAKO_E2E_ENVIRONMENT" env pull --force
    CI=true TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 TAKO_HOST_KEY_MODE="$ci_host_key_mode" "$@" --env "$TAKO_E2E_ENVIRONMENT" state pull
    CI=true TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 TAKO_HOST_KEY_MODE="$ci_host_key_mode" "$@" --env "$TAKO_E2E_ENVIRONMENT" state status
    CI=true TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 TAKO_HOST_KEY_MODE="$ci_host_key_mode" "$@" --env "$TAKO_E2E_ENVIRONMENT" state lease
    CI=true TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 TAKO_HOST_KEY_MODE="$ci_host_key_mode" "$@" --env "$TAKO_E2E_ENVIRONMENT" deploy --yes
  ' _ "$clone_dir" "$CI_HOST_KEY_MODE" "$TAKOD_BINARY" "$TAKO_BIN"
}

phase_repair() {
  require_confirm "repair"
  run_tako "repair status before" state status
  run_tako "repair lease before" state lease
  run_tako "state repair" state repair
  run_tako "repair status after" state status
  run_tako "repair lease after" state lease
}

phase_invalid_config() {
  local temp_parent invalid_dir log_file
  temp_parent="$(mktemp -d "${TMPDIR:-/tmp}/tako-e2e-invalid-config.XXXXXX")"
  TEMP_DIRS+=("$temp_parent")
  invalid_dir="$temp_parent/app"
  mkdir -p "$invalid_dir"
  cat >"$invalid_dir/tako.yaml" <<'EOF'
project:
  name: invalid-config-proof
  version: [
EOF

  if run_tako_status_in "$invalid_dir" "invalid YAML deploy should fail preflight" deploy --yes; then
    die "deploy unexpectedly succeeded with invalid YAML"
  fi
  log_file="$LAST_LOG_FILE"

  if ! grep -q "YAML syntax error in tako.yaml" "$log_file"; then
    tail -n 80 "$log_file" >&2 || true
    die "invalid-config phase did not report the deploy config preflight error"
  fi
  if grep -Eq "=== Starting deployment ===|Acquired deployment lock|Acquired remote deploy leases|Starting takod deployment" "$log_file"; then
    tail -n 80 "$log_file" >&2 || true
    die "invalid-config phase reached deployment work instead of stopping at config preflight"
  fi

  note "Invalid YAML failed during deploy config preflight"
}

phase_protocols() {
  require_protocol_url

  run_cmd "protocol HTTP/1.1" curl --http1.1 --fail --silent --show-error --location --max-time 20 --dump-header "$LOG_DIR/protocol-http1.headers" --output "$LOG_DIR/protocol-http1.body" "$PROTOCOL_URL"
  run_cmd "protocol HTTP/2" curl --http2 --fail --silent --show-error --location --max-time 20 --dump-header "$LOG_DIR/protocol-http2.headers" --output "$LOG_DIR/protocol-http2.body" "$PROTOCOL_URL"

  if grep -qi '^alt-svc:.*h3' "$LOG_DIR/protocol-http1.headers" "$LOG_DIR/protocol-http2.headers"; then
    note "HTTP/3 Alt-Svc advertised"
  else
    die "protocol response did not advertise HTTP/3 Alt-Svc"
  fi

  if curl_supports_http3; then
    run_cmd "protocol HTTP/3" curl --http3 --fail --silent --show-error --location --max-time 20 --dump-header "$LOG_DIR/protocol-http3.headers" --output "$LOG_DIR/protocol-http3.body" "$PROTOCOL_URL"
  elif [[ "$HTTP3_REQUIRED" == "1" ]]; then
    die "curl does not support --http3; install an HTTP/3-capable curl or unset TAKO_E2E_HTTP3_REQUIRED"
  else
    note "Skipping HTTP/3 wire request: curl does not support --http3"
  fi

  if [[ -n "$WEBSOCKET_URL" ]]; then
    require_tool node
    run_cmd "protocol WebSocket" node --input-type=module -e '
const url = process.argv[1];
if (typeof WebSocket === "undefined") {
  console.error("Node WebSocket global unavailable; use Node 22+ or omit TAKO_E2E_WEBSOCKET_URL");
  process.exit(1);
}
const ws = new WebSocket(url);
const timeout = setTimeout(() => {
  console.error("websocket timeout");
  process.exit(1);
}, 10000);
ws.addEventListener("message", (event) => {
  console.log(String(event.data));
  clearTimeout(timeout);
  ws.close();
});
ws.addEventListener("error", (event) => {
  console.error(event.error || event.message || "websocket error");
  clearTimeout(timeout);
  process.exit(1);
});
' "$WEBSOCKET_URL"
  else
    note "Skipping WebSocket check: TAKO_E2E_WEBSOCKET_URL is not set"
  fi
}

phase_offline() {
  require_confirm "offline"
  require_min_environment_servers "offline" 2
  require_offline_node_control
  require_deploy_ready_worktree "$APP_DIR"
  run_tako "offline baseline target status" state status --server "$OFFLINE_SERVER"

  note "Stopping takod on $OFFLINE_SERVER"
  ssh_offline_node "$OFFLINE_STOP_CMD" >/dev/null
  OFFLINE_RESTORE_NEEDED=1
  wait_for_offline_status down || die "offline node $OFFLINE_SERVER did not become unavailable"

  run_tako "offline state status" state status
  run_tako "offline state lease" state lease

  if run_tako_status "offline drift should fail closed" drift; then
    die "drift unexpectedly succeeded while $OFFLINE_SERVER was unavailable"
  fi
  note "Drift failed closed while $OFFLINE_SERVER was unavailable"

  if run_tako_status "offline deploy should fail closed" deploy --yes; then
    die "deploy unexpectedly succeeded while $OFFLINE_SERVER was unavailable"
  fi
  note "Deploy failed closed while $OFFLINE_SERVER was unavailable"

  restore_offline_node
  wait_for_offline_status up || die "offline node $OFFLINE_SERVER did not come back"
  run_tako "rejoined state status" state status
  run_tako "rejoined state lease" state lease
  run_tako "rejoined state repair" state repair
  run_tako "rejoined deploy" deploy --yes
}

expand_phases() {
  local input="$1"
  local expanded=()
  IFS=',' read -ra raw <<<"$input"
  for phase in "${raw[@]}"; do
    phase="$(printf '%s' "$phase" | xargs)"
    case "$phase" in
      standard)
        expanded+=(preflight invalid-config one-node env new-computer ci)
        ;;
      full)
        expanded+=(preflight invalid-config one-node env new-computer ci two-node repair offline)
        ;;
      all)
        expanded+=(preflight invalid-config one-node env new-computer ci two-node repair offline)
        ;;
      "")
        ;;
      preflight|one-node|two-node|env|new-computer|ci|repair|invalid-config|protocols|offline)
        expanded+=("$phase")
        ;;
      *)
        die "unknown phase: $phase"
        ;;
    esac
  done
  printf '%s\n' "${expanded[@]}"
}

main() {
  require_tool git
  require_tool bash
  require_app_repo
  normalize_takod_binary
  build_or_select_tako

  export TAKO_E2E_ENVIRONMENT="$ENVIRONMENT"

  note "App dir: $APP_DIR"
  note "Environment: $ENVIRONMENT"
  note "Logs: $LOG_DIR"

  phase_list=()
  while IFS= read -r phase; do
    [[ -n "$phase" ]] && phase_list+=("$phase")
  done < <(expand_phases "$PHASES")
  [[ ${#phase_list[@]} -gt 0 ]] || die "no phases selected"

  for phase in "${phase_list[@]}"; do
    note "Phase: $phase"
    case "$phase" in
      preflight) phase_preflight ;;
      one-node) phase_one_node ;;
      two-node) phase_two_node ;;
      env) phase_env ;;
      new-computer) phase_new_computer ;;
      ci) phase_ci ;;
      repair) phase_repair ;;
      invalid-config) phase_invalid_config ;;
      protocols) phase_protocols ;;
      offline) phase_offline ;;
    esac
  done

  note "Mesh E2E phases passed"
  note "Logs written to $LOG_DIR"
}

main "$@"
