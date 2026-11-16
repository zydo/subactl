#!/usr/bin/env bash
set -eu
sleep "${TOY_STEP:-0.2}"
echo "screenshots match the baseline for $PKG"
echo ok > "$WORK/status.screenshot"
