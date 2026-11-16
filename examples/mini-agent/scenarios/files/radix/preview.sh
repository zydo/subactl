#!/usr/bin/env bash
# Start a Storybook dev server for this worktree. Every worker wants port
# 9009, so ports are handed out from a shared counter.
set -eu
. "$TASK_DIR/env.sh"
cd "$WT"
exec 9>"$RUN_DIR/port.lock"
flock 9
port=$(( $(cat "$RUN_DIR/port.next" 2>/dev/null || echo 9009) ))
echo $((port + 1)) > "$RUN_DIR/port.next"
flock -u 9
echo "$port" > "$WT/.preview-port"   # in the worktree: fixers work there too
setsid nohup storybook dev -p "$port" --ci --no-open > "$WORK/storybook.log" 2>&1 &
sb=$!
echo "$sb" > "$WORK/pids"
for i in $(seq 1 120); do
  kill -0 "$sb" 2>/dev/null || break   # the server died; do not wait for it
  if curl -sf -o /dev/null "http://127.0.0.1:$port/iframe.html"; then
    echo "storybook is up on port $port"
    echo ok > "$WORK/status.preview"
    exit 0
  fi
  sleep 2
done
echo "storybook did not come up on port $port"; tail -5 "$WORK/storybook.log"
exit 1
