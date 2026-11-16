#!/usr/bin/env bash
# The packages the React 19 upgrade has to touch.
. "$(dirname "$0")/env.sh"
git -C "$REPO" diff --name-only "$BASE" "$FIX" -- packages/react | cut -d/ -f3 | sort -u
