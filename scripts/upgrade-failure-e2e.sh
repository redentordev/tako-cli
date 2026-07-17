#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 --app-dir DIR --env NAME --tako PATH --valid-binary PATH" >&2
  exit 2
}

app_dir=""
environment=""
tako=""
valid_binary=""
while (($#)); do
  case "$1" in
    --app-dir) app_dir="${2:-}"; shift 2 ;;
    --env) environment="${2:-}"; shift 2 ;;
    --tako) tako="${2:-}"; shift 2 ;;
    --valid-binary) valid_binary="${2:-}"; shift 2 ;;
    *) usage ;;
  esac
done
[[ -d "$app_dir" && -x "$tako" && -f "$valid_binary" && -n "$environment" ]] || usage
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
failure_binary="$repo_dir/scripts/upgrade-failure-candidate.sh"
before="$(mktemp)"
failure="$(mktemp)"
after="$(mktemp)"
trap 'rm -f "$before" "$failure" "$after"' EXIT

cd "$app_dir"
"$tako" --output json --env "$environment" upgrade servers --dry-run >"$before"
set +e
"$tako" --output json --env "$environment" upgrade servers --takod-binary "$failure_binary" >"$failure"
failure_status=$?
set -e
[[ $failure_status -ne 0 ]] || { echo "failure candidate unexpectedly succeeded" >&2; exit 1; }

jq -e '.nodes[0].stage == "worker-canary" and .nodes[0].outcome == "rolled-back" and .nodes[0].rolledBack == true' "$failure" >/dev/null
jq -e '(.nodes | length) > 1 and ([.nodes[1:][] | .outcome] | all(. == "blocked"))' "$failure" >/dev/null
"$tako" --output json --env "$environment" upgrade servers --dry-run >"$after"
jq -e --slurpfile before "$before" '([.nodes[].fromVersion] == [$before[0].nodes[].fromVersion]) and ([.nodes[].outcome] | all(. != "status-unavailable"))' "$after" >/dev/null

# A valid full-plan retry proves pending/terminal transaction recovery and the
# worker-first/controller-last path after the injected rollback.
"$tako" --output json --env "$environment" upgrade servers --takod-binary "$valid_binary" |
  jq -e '([.nodes[].outcome] | all(. == "upgraded")) and .nodes[-1].stage == "controller-last"' >/dev/null

echo "upgrade failure E2E passed: rollback, stage blocking, service recovery, and valid retry"
