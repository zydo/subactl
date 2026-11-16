#!/usr/bin/env bash
# Verify the package on React 19: no type errors in it, its jest tests pass,
# and its stories render without console errors.
set -u
. "$TASK_DIR/env.sh"
cd "$WT"
fail=0
tsc --noEmit > "$WORK/tsc.verify.log" 2>&1
errs=$(grep -c "^packages/react/$PKG/.*error TS" "$WORK/tsc.verify.log")
echo "type errors in $PKG: $errs"; [ "$errs" = 0 ] || fail=1
if ls "packages/react/$PKG/src/"*.test.tsx >/dev/null 2>&1; then
  workers=2; [ "${G6_UNBOUNDED:-0}" = 1 ] && workers=$(( $(nproc) * 4 ))
  if jest "packages/react/$PKG" --maxWorkers="$workers" > "$WORK/jest.log" 2>&1; then
    echo "jest passed"
  else
    echo "jest failed"; tail -15 "$WORK/jest.log"; fail=1
  fi
else
  echo "no jest tests for $PKG"
fi
keep=0; [ "${G2_LEAVE_SERVERS:-0}" = 1 ] && keep=1
if NODE_PATH="$RADIX_HOME/pw/node_modules" node "$TASK_DIR/shot.js" "$(cat "$WT/.preview-port")" "$PKG" "$WORK/shots" "$RADIX_HOME/shots-base" "$keep" > "$WORK/shot.json" 2> "$WORK/shot.err"; then
  cat "$WORK/shot.json"
  grep -q '"errors":0' "$WORK/shot.json" || fail=1
else
  echo "screenshots failed"; tail -5 "$WORK/shot.err"; fail=1
fi
if [ "${FAIL:-0}" = 1 ]; then
  echo "verification failed for $PKG at depth $DEPTH"
  fail=1
fi
exit "$fail"
