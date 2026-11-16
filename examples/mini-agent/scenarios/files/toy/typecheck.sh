#!/usr/bin/env bash
set -eu
sleep "${TOY_STEP:-0.2}"
echo "typecheck passed for $PKG"
echo ok > "$WORK/status.typecheck"
