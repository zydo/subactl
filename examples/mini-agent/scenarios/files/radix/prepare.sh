#!/usr/bin/env bash
# One-time preparation of the task environment. Not run by any agent.
# Needs Node 18, git and network access. Usage: RADIX_HOME=/srv/radix prepare.sh
set -euo pipefail
. "$(dirname "$0")/env.sh"
mkdir -p "$RADIX_HOME" && cd "$RADIX_HOME"
corepack enable 2>/dev/null || true
[ -d primitives/.git ] || git clone -q https://github.com/radix-ui/primitives.git
cd primitives && git checkout -q --detach "$BASE"
[ -d node_modules ] || yarn install --immutable
# The React 19 dependencies, installed once and hard-linked into each worktree.
[ -d ../r19 ] || git worktree add -f --detach ../r19 "$FIX"
cd ../r19 && git checkout -q --detach "$FIX"
[ -d node_modules ] || yarn install --immutable
# Upstream #2934 made the code compatible but left the dependencies on
# React 18. Raise the runtime to 19. The types stay on 18: the reference
# fix predates the final React 19 types and does not compile against them.
yarn up 'react@^19.0.0' 'react-dom@^19.0.0' '@types/react@^18.3.0' '@types/react-dom@^18.3.0'
# Storybook 7 picks its React root API by version and only knows 18.
grep -q react-dom-shim .storybook/main.ts || sed -i 's|        ...convertTsConfigPathsToWebpackAliases(),|&\n        "@storybook/react-dom-shim": require.resolve("@storybook/react-dom-shim/dist/react-18"),|' .storybook/main.ts
mkdir -p ../pw && cd ../pw
[ -d node_modules/playwright ] || { npm init -y >/dev/null; npm i playwright@1.44; }
npx playwright install chromium
# Baseline screenshots: the stories as they render on React 18.
if [ ! -d ../shots-base ]; then
  cd ../primitives
  setsid nohup yarn storybook dev -p 9008 --ci --no-open > /tmp/storybook-base.log 2>&1 &
  sb=$!
  for i in $(seq 1 120); do curl -sf -o /dev/null http://127.0.0.1:9008/iframe.html && break; sleep 2; done
  for pkg in $("$(dirname "$0")/list.sh"); do
    NODE_PATH="$RADIX_HOME/pw/node_modules" node "$(dirname "$0")/shot.js" 9008 "$pkg" "$RADIX_HOME/shots-base" /nonexistent 0
  done
  kill -- "-$sb" 2>/dev/null || kill "$sb"
fi
echo "prepared $RADIX_HOME"
