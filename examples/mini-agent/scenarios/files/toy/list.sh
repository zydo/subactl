#!/usr/bin/env bash
# The toy task stands in for the radix task on machines without its
# toolchain. Its "packages" are just names.
for i in $(seq 1 "${TOY_PACKAGES:-6}"); do echo "pkg$i"; done
