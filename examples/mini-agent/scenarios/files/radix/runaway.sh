#!/usr/bin/env bash
# A command stuck at an interactive prompt, repeating it without end. The
# rate is bounded so that a run without any output limit fills the disk
# slowly enough to be observed.
line="? Do you want to continue? (y/N) [yarn install is waiting for input]"
chunk=$(for i in $(seq 1 1500); do echo "$line"; done)   # about 100KB
per_second=$(( ${RUNAWAY_MBPS:-5} * 10 ))
while :; do
  for i in $(seq 1 "$per_second"); do echo "$chunk"; done
  sleep 1
done
