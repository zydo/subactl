# Task environment for the radix scenario: Node 18, git, and the system
# libraries Chromium needs. Run it privileged so the bench can create its
# blast-chamber cgroup:
#
#   docker run --privileged -v radix:/srv/radix -v $PWD/bin-linux:/opt/subactl ...
FROM node:18-bookworm
RUN apt-get update \
 && apt-get install -y --no-install-recommends git curl procps util-linux ca-certificates \
 && npx -y playwright@1.44 install-deps chromium \
 && rm -rf /var/lib/apt/lists/* /root/.npm
RUN corepack enable
