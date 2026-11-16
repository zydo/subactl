#!/usr/bin/env bash
# Decide the outcome of a task from the status files its steps left.
set -u
for step in "$@"; do
  if [ "$(cat "$WORK/status.$step" 2>/dev/null)" != ok ]; then
    echo "step $step did not succeed"
    exit 1
  fi
done
touch "$WORK/ok"
echo "all steps succeeded"
