#!/usr/bin/env bash
# Start a "dev server" and a "browser" and leave them for the next steps.
# Each is a sleep whose process name carries the run id, so the bench can
# count what is left behind. cleanup.sh stops them.
set -eu
( exec -a "toy-server-$RUN_ID-$PKG" sleep "${TOY_SERVER_LIFE:-600}" ) >/dev/null 2>&1 &
echo $! > "$WORK/pids"
( exec -a "toy-chrome-$RUN_ID-$PKG" sleep "${TOY_SERVER_LIFE:-600}" ) >/dev/null 2>&1 &
echo $! >> "$WORK/pids"
sleep "${TOY_STEP:-0.2}"
echo "dev server and browser are up for $PKG"
echo ok > "$WORK/status.preview"
