#!/usr/bin/env bash
# Run an experiment group in a Docker blast chamber and copy the results out.
#
#   bench/chamber/run.sh GROUP SCENARIO RUNS OUT_DIR [CHAMBER_SIZE]
#   bench/chamber/run.sh A scenarios/radix.yaml 5 /tmp/bench-a 12G
#
# The first run prepares the radix task environment in the Docker volume
# subactl-radix-check, which takes a few minutes and needs network access.
set -euo pipefail
group=${1:?group, e.g. A}; scenario=${2:?scenario file}; runs=${3:?number of runs}; out=${4:?output directory}
chamber=${5:-12G}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
arch=$(docker info --format '{{.Architecture}}' | sed 's/aarch64/arm64/; s/x86_64/amd64/')

mkdir -p "$out" "$repo/bin-linux"
(cd "$repo" && GOOS=linux GOARCH="$arch" go build -o bin-linux/ ./cmd/... ./examples/mini-agent/cmd/...)
docker build -q -t subactl-radix-env "$here" >/dev/null
docker volume create subactl-radix-check >/dev/null

docker run --rm --privileged \
  -v subactl-radix-check:/srv/radix \
  -v "$repo/bin-linux:/opt/subactl:ro" \
  -v "$(cd "$(dirname "$scenario")" && pwd):/sc:ro" \
  -v "$out:/host" \
  -e GROUP="$group" -e SCENARIO="$(basename "$scenario")" -e RUNS="$runs" -e CHAMBER="$chamber" \
  subactl-radix-env bash -c '
    set -e
    chown node:node /srv/radix
    su node -c "git config --global --add safe.directory \"*\"
      /opt/subactl/bench extract radix /tmp/task
      [ -d /srv/radix/shots-base ] || RADIX_HOME=/srv/radix /tmp/task/prepare.sh
      git -C /srv/radix/primitives worktree prune"
    /opt/subactl/bench run --group "$GROUP" --scenario "/sc/$SCENARIO" --runs "$RUNS" --chamber "$CHAMBER" \
      --user node --workdir /srv/radix/primitives --out /tmp/out
    # Copy out the measurements; the run directories stay in the container.
    cd /tmp/out/"$GROUP"
    cp summary.json /host/
    for r in run-*; do
      mkdir -p "/host/$r"
      cp "$r/summary.json" "$r/series.jsonl" "$r/lead.log" "/host/$r/"
      if [ -f "$r/subactld.log" ]; then cp "$r/subactld.log" "/host/$r/"; fi
      # The audit log of the run (group B keeps one per run).
      if [ -d "$r/state/audit" ]; then cp -r "$r/state/audit" "/host/$r/audit"; fi
    done
    # Group C keeps one audit log across all its runs.
    if [ -d state/audit ]; then cp -r state/audit /host/audit; fi'
echo "results are in $out"
