#!/usr/bin/env bash
# A minimal lead for the tutorial (docs/tutorial.md): one worker per task,
# as many at once as the lead's own memory allows, each with the memory
# subactl suggests for its kind. Run it as a lead:
#
#   subactl run --memory 2G -- examples/orchestrator/fanout.sh a b c leak server d
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
out="$SUBACTL_SCRATCH/results"
mkdir -p "$out"

# Start one worker and wait for it. A refusal for lack of room is not a
# failure: wait for room and ask again. Exit status: 0 if the worker ended
# 2xx, 3 if it ended otherwise, 1 if the request was refused for another
# reason.
run_one() {
  local task=$1
  while :; do
    subactl spawn --wait --kind worker --memory "$(subactl suggest worker 2>/dev/null)" --wall 5m \
      -- "$here/worker.sh" "$task" >"$out/$task.json" 2>"$out/$task.err"
    local rc=$?
    if [ $rc -eq 1 ] && grep -qE '"reason": *"(parent_budget|mem_budget|capacity_shrunk)"' "$out/$task.err"; then
      sleep 1
      continue
    fi
    return $rc
  done
}

for task in "$@"; do
  run_one "$task" &
done
wait

# Every worker has ended; nothing they started is still running.
for task in "$@"; do
  code=$(grep -oE '"code": *[0-9]+' "$out/$task.json" | head -1 | grep -oE '[0-9]+$')
  name=$(grep -oE '"name": *"[A-Z_]+"' "$out/$task.json" | head -1 | cut -d'"' -f4)
  echo "$task: ${code:-?} ${name:-}"
done
