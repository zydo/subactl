# Sourced by every radix step. RADIX_HOME is laid out by prepare.sh:
#   primitives/   the repository at the baseline commit, with React 18 installed
#   r19/          the upstream fix with react and react-dom raised to 19, installed
#   pw/           Playwright with Chromium, and shot.js
#   shots-base/   screenshots of the stories at the baseline
#   wt/<run>/     one worktree per package, per run; removed by teardown.sh
RADIX_HOME="${RADIX_HOME:-/srv/radix}"
BASE=af1e3b41cb5c49d7b8c073d2aec34d5074c6dd17   # parent of upstream #2934
FIX=06de2d4a8acc8024aa5130aeab03cfd8982f0e48    # upstream #2934, React 19 compatibility
REPO="$RADIX_HOME/primitives"
# Worktrees live beside the prepared trees: hard links to node_modules do
# not cross filesystems.
WT="$RADIX_HOME/wt/${RUN_ID:-none}/${PKG:-none}"
export CYPRESS_INSTALL_BINARY=0 HUSKY=0 CI=1
export PLAYWRIGHT_BROWSERS_PATH="$RADIX_HOME/pw/browsers"
# A worktree shares the prepared node_modules by hard links and is never
# "installed" by yarn, so its tools are run directly rather than through yarn.
export PATH="$WT/node_modules/.bin:$PATH"
