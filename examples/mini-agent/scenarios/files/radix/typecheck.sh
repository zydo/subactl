#!/usr/bin/env bash
# The baseline's type check is repository-wide: every worker runs all of it.
set -u
. "$TASK_DIR/env.sh"
cd "$WT"
n=1
if [ "${G6_UNBOUNDED:-0}" = 1 ]; then
  n=$(( $(nproc) * 4 ))                     # G6: far more parallelism than cores
  ( find / -xdev -type f 2>/dev/null | wc -l > "$WORK/walk.count" ) &   # G6: an unbounded file walk
fi
for i in $(seq 1 "$n"); do
  tsc --noEmit > "$WORK/tsc.$i.log" 2>&1 &
done
wait
echo "tsc: $(grep -c 'error TS' "$WORK/tsc.1.log") errors in the repository, $(grep -c "^packages/react/$PKG/.*error TS" "$WORK/tsc.1.log") in $PKG"
echo ok > "$WORK/status.typecheck"
