#!/usr/bin/env bash
# A worker for the tutorial (docs/tutorial.md). It does its task, writes its
# result into its own scratch directory and tells subactl it is done.
#
# Two tasks misbehave on purpose:
#   leak    allocates without end, until its memory limit ends it (413)
#   server  starts a "dev server" and never stops it; subactl reaps it
set -euo pipefail
task=$1
subactl beacon --state working --tool "$task" >/dev/null

case $task in
  leak)
    exec awk 'BEGIN { s = "x"; while (1) s = s s }' ;;
  server)
    # Left running in the background, as agents do with dev servers.
    nohup sleep 600 >/dev/null 2>&1 &
    ;;
esac

sleep 2   # the work
result="$SUBACTL_SCRATCH/result.txt"
echo "task $task done by lease $SUBACTL_LEASE_ID" > "$result"
subactl done --result "$result" >/dev/null
