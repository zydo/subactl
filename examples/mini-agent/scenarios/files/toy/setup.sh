#!/usr/bin/env bash
set -eu
sleep "${TOY_STEP:-0.2}"
echo "worktree ready for $PKG"
echo ok > "$WORK/status.setup"
