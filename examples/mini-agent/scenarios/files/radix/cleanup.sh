#!/usr/bin/env bash
# Stop the dev server, unless the agent "forgets" to (G2).
set -u
if [ "${G2_LEAVE_SERVERS:-0}" = 1 ]; then
  echo "left the storybook dev server and the browser running"
  exit 0
fi
while read -r pid; do kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null; done < "$WORK/pids"
echo "stopped the storybook dev server"
