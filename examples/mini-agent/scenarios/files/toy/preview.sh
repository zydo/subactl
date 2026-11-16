#!/usr/bin/env bash
# Start a "dev server" and a "browser". Each is a sleep whose process name
# carries the run id, so the bench can count what is left behind.
set -eu
( exec -a "toy-server-$RUN_ID-$PKG" sleep "${TOY_SERVER_LIFE:-600}" ) >/dev/null 2>&1 &
server=$!
( exec -a "toy-chrome-$RUN_ID-$PKG" sleep "${TOY_SERVER_LIFE:-600}" ) >/dev/null 2>&1 &
chrome=$!
sleep "${TOY_STEP:-0.2}"
echo "screenshot taken for $PKG"
echo ok > "$WORK/status.preview"
if [ "${G2_LEAVE_SERVERS:-0}" = 1 ]; then
  echo "left the dev server ($server) and the browser ($chrome) running"
else
  kill "$server" "$chrome"
fi
