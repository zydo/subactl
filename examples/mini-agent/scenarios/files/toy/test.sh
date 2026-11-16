#!/usr/bin/env bash
set -eu
sleep "${TOY_STEP:-0.2}"
if [ "${FAIL:-0}" = 1 ]; then
  echo "tests failed for $PKG at depth $DEPTH"
  exit 1
fi
echo "tests passed for $PKG"
echo ok > "$WORK/status.test"
