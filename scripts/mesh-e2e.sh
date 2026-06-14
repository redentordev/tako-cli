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
#   TAKO_E2E_CONFIRM=run      Required for setup/deploy/repair/env phases.
#   TAKO_E2E_LOG_DIR          Directory for logs. Defaults outside app worktree.
#   TAKO_E2E_KEEP_WORKDIR=1   Keep temporary fresh-clone workdirs.

set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

APP_DIR="${TAKO_E2E_APP_DIR:-$PWD}"
ENVIRONMENT="${TAKO_E2E_ENVIRONMENT:-${TAKO_E2E_ENV:-production}}"
PHASES="${TAKO_E2E_PHASES:-preflight}"
TAKO_BIN="${TAKO_E2E_TAKO_BIN:-}"
CONFIRM="${TAKO_E2E_CONFIRM:-}"
KEEP_WORKDIR="${TAKO_E2E_KEEP_WORKDIR:-0}"

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
  --yes               Allow mutating phases.
  --keep-workdir      Keep temporary fresh-clone workdirs.
  --help, -h          Show this help.

Phases:
  preflight       Local config and remote state visibility checks.
  one-node        setup, deploy, state status, history, ps, drift.
  two-node        setup, repair, status, deploy, status.
  env             env push, temporary local env removal, env pull --force.
  new-computer    fresh clone, env pull, state pull, status, deploy.
  ci              fresh clone with CI env, env pull, state pull, status, deploy.
  repair          state status, state repair, state status.
  offline         status, drift, deploy while you have manually taken a node down.
  standard        preflight, one-node, env, new-computer, ci.
  full            standard plus two-node, repair, offline.

Mutating phases require --yes or TAKO_E2E_CONFIRM=run.
Set TAKO_ENV_PASSPHRASE for env, new-computer, and ci phases.
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
    --yes)
      CONFIRM="run"
      shift
      ;;
    --keep-workdir)
      KEEP_WORKDIR="1"
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
    shift
    TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 "$@"
  ' _ "$dir" "$TAKO_BIN" --env "$ENVIRONMENT" "$@"
}

run_tako() {
  local label="$1"
  shift
  run_tako_in "$APP_DIR" "$label" "$@"
}

run_tako_status() {
  local label="$1"
  shift
  run_cmd_status "$label" bash -c '
    cd "$1"
    shift
    TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 "$@"
  ' _ "$APP_DIR" "$TAKO_BIN" --env "$ENVIRONMENT" "$@"
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
  run_tako "doctor skip remote" doctor --skip-remote
  run_tako "state status" state status
}

phase_one_node() {
  require_confirm "one-node"
  require_deploy_ready_worktree "$APP_DIR"
  run_tako "setup" setup
  run_tako "deploy" deploy --yes
  run_tako "state status after deploy" state status
  run_tako "history" history
  run_tako "ps" ps
  run_tako "drift" drift
}

phase_two_node() {
  require_confirm "two-node"
  require_deploy_ready_worktree "$APP_DIR"
  run_tako "two-node setup" setup
  run_tako "two-node repair" state repair
  run_tako "two-node status before deploy" state status
  run_tako "two-node deploy" deploy --yes
  run_tako "two-node status after deploy" state status
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
  run_tako_in "$clone_dir" "new-computer env pull" env pull --force
  run_tako_in "$clone_dir" "new-computer state pull" state pull
  run_tako_in "$clone_dir" "new-computer state status" state status
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
    shift
    CI=true TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 TAKO_HOST_KEY_MODE="${TAKO_HOST_KEY_MODE:-strict}" "$@" --env "$TAKO_E2E_ENVIRONMENT" env pull --force
    CI=true TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 TAKO_HOST_KEY_MODE="${TAKO_HOST_KEY_MODE:-strict}" "$@" --env "$TAKO_E2E_ENVIRONMENT" state pull
    CI=true TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 TAKO_HOST_KEY_MODE="${TAKO_HOST_KEY_MODE:-strict}" "$@" --env "$TAKO_E2E_ENVIRONMENT" state status
    CI=true TAKO_SKIP_UPDATE_CHECK=1 TAKO_NONINTERACTIVE=1 TAKO_HOST_KEY_MODE="${TAKO_HOST_KEY_MODE:-strict}" "$@" --env "$TAKO_E2E_ENVIRONMENT" deploy --yes
  ' _ "$clone_dir" "$TAKO_BIN"
}

phase_repair() {
  require_confirm "repair"
  run_tako "repair status before" state status
  run_tako "state repair" state repair
  run_tako "repair status after" state status
}

phase_offline() {
  require_confirm "offline"
  require_deploy_ready_worktree "$APP_DIR"
  note "offline phase expects one node to already be unreachable"
  run_tako "offline state status" state status
  run_tako "offline drift" drift
  run_tako "offline deploy" deploy --yes
}

expand_phases() {
  local input="$1"
  local expanded=()
  IFS=',' read -ra raw <<<"$input"
  for phase in "${raw[@]}"; do
    phase="$(printf '%s' "$phase" | xargs)"
    case "$phase" in
      standard)
        expanded+=(preflight one-node env new-computer ci)
        ;;
      full)
        expanded+=(preflight one-node env new-computer ci two-node repair offline)
        ;;
      all)
        expanded+=(preflight one-node env new-computer ci two-node repair offline)
        ;;
      "")
        ;;
      preflight|one-node|two-node|env|new-computer|ci|repair|offline)
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
  build_or_select_tako

  export TAKO_E2E_ENVIRONMENT="$ENVIRONMENT"

  note "App dir: $APP_DIR"
  note "Environment: $ENVIRONMENT"
  note "Logs: $LOG_DIR"

  mapfile -t phase_list < <(expand_phases "$PHASES")
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
      offline) phase_offline ;;
    esac
  done

  note "Mesh E2E phases passed"
  note "Logs written to $LOG_DIR"
}

main "$@"
