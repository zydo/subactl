#!/usr/bin/env bash
# Stop what preview.sh started, unless the agent "forgets" to (G2).
set -u
if [ "${G2_LEAVE_SERVERS:-0}" = 1 ]; then
  echo "left the dev server and the browser running"
  exit 0
fi
xargs kill < "$WORK/pids" 2>/dev/null
echo "stopped the dev server and the browser"
