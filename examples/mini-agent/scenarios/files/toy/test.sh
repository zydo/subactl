#!/usr/bin/env bash
# Verify the package: tests, then the stories in the browser.
set -eu
# The dev server and browser are up while this runs; a scenario can make
# the step longer than the others to widen that window.
sleep "${TOY_TEST_STEP:-${TOY_STEP:-0.2}}"
if [ "${FAIL:-0}" = 1 ]; then
  echo "tests failed for $PKG at depth $DEPTH"
  exit 1
fi
echo "tests and stories passed for $PKG"
