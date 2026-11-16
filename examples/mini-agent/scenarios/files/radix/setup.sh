#!/usr/bin/env bash
# A worktree for this package, moved to the React 19 dependencies.
set -eu
. "$TASK_DIR/env.sh"
mkdir -p "$(dirname "$WT")"
git -C "$REPO" worktree add -f --detach "$WT" "$BASE" >/dev/null 2>&1
cd "$WT"
# Move the repository to React 19: dependencies, lockfile, Storybook config.
cp "$RADIX_HOME/r19/package.json" "$RADIX_HOME/r19/yarn.lock" .
cp "$RADIX_HOME/r19/.storybook/main.ts" .storybook/main.ts
cp -al "$RADIX_HOME/r19/node_modules" node_modules
# Build caches must not be shared: hard-linked cache files would be written
# by every worktree's dev server at once.
rm -rf node_modules/.cache
# The first attempt changes only this package. A seeded failure skips even
# that, as an agent that misreads the task would.
if [ "${FAIL:-0}" != 1 ]; then
  git checkout -q "$FIX" -- "packages/react/$PKG"
fi
echo "worktree $WT is on React 19"
echo ok > "$WORK/status.setup"
