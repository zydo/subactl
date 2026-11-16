#!/usr/bin/env bash
# A fixer works in the worker's worktree. One level down it applies the
# package's own change; deeper, the packages it depends on as well.
set -eu
. "$TASK_DIR/env.sh"
cd "$WT"
if [ "$DEPTH" -le 2 ]; then
  git checkout -q "$FIX" -- "packages/react/$PKG"
  echo "applied the React 19 change to $PKG"
else
  git checkout -q "$FIX" -- packages
  echo "applied the React 19 change to $PKG and every package it depends on"
fi
