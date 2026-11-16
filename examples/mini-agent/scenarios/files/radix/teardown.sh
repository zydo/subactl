#!/usr/bin/env bash
# Run by the bench after a run: remove the run's worktrees.
. "$(dirname "$0")/env.sh"
rm -rf "$RADIX_HOME/wt/${RUN_ID:?}"
git -C "$REPO" worktree prune
