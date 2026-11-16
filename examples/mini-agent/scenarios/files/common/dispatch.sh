#!/usr/bin/env bash
# Fan out one worker per package that has no result yet. Invoked by the
# planner's bash tool. FANOUT bounds how many run at once.
set -u
todo=()
while read -r pkg; do
  [ -n "$pkg" ] || continue
  [ -f "$RUN_DIR/results/upgrade_$pkg.d1.json" ] || todo+=("upgrade:$pkg")
done < <("$TASK_DIR/list.sh" | head -n "${MAX_PACKAGES:-1000}")
if [ "${G5_RUNAWAY:-0}" = 1 ] && [ ! -f "$RUN_DIR/results/runaway.d1.json" ]; then
  todo+=("runaway")
fi
echo "dispatching ${#todo[@]} workers, at most ${FANOUT} at a time"
[ "${#todo[@]}" -gt 0 ] || exit 0
printf '%s\n' "${todo[@]}" | xargs -P "$FANOUT" -I{} "$MINI_AGENT" --task {}
echo "dispatch finished with status $?"
ls "$RUN_DIR/results" | wc -l
