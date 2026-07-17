#!/bin/sh
# Deliberately passes candidate validation but cannot run takod. Use only on a
# disposable E2E node to prove automatic node-upgrade rollback.
if [ "${1:-}" = "--version" ]; then
  echo "tako failure-injection"
  exit 0
fi
exit 42
