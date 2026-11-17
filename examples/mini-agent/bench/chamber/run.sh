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
# Build the binaries here, or use the ones copied in from a machine that
# has Go when this one does not.
if command -v go >/dev/null; then
  (cd "$repo" && GOOS=linux GOARCH="$arch" go build -o bin-linux/ ./cmd/... ./examples/mini-agent/cmd/...)
elif [ ! -x "$repo/bin-linux/bench" ]; then
  echo "no Go toolchain here and no prebuilt binaries in $repo/bin-linux" >&2
  exit 1
fi
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
      # What each agent did, to diagnose a run without running it again:
      # the task results, transcripts and the logs of the checks.
      (cd "$r/run" && tar czf "/host/$r/agents.tgz" results $(cd work && ls -d */ | sed "s|^|work/|") \
        --exclude="*/shots" --exclude="*/chrome-profile-*" --exclude="work/runaway*/bash-*.out" 2>/dev/null) || true
    done
    # Group C keeps one audit log across all its runs.
    if [ -d state/audit ]; then cp -r state/audit /host/audit; fi
    # The container runs as root; let the user who started it read it all.
    chmod -R a+rX /host'
echo "results are in $out"
