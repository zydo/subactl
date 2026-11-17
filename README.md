# subactl

A single-host resource controller for subagents: a quota ledger that never
oversells memory, and a guarantee that when an agent's group ends, every
process in it is gone. `subactld` is the daemon, `subactl` the CLI.

The design is in [DESIGN.md](DESIGN.md). The only contract is
[api/openapi.yaml](api/openapi.yaml).

## Status

- **M0, contract and skeleton: done.** The contract, the public Go client,
  the in-memory ledger, the lease lifecycle and the CLI.
- **M1, baseline: done.** `examples/mini-agent` in `without` mode
  reproduces the problem in five of five runs; see
  [the results](examples/mini-agent/bench/results/README.md).
- **M2, Linux enforcement core: done.** The cgroup v2 backend, the pre-exec
  shim, nested leases, whole-group reaping, escape prevention and
  fail-closed restart. Integration tests named after the acceptance
  criteria (F1-F4, F11-F14, F33) run with `make integration`.
- **M3, quota and observability: done.** Effective capacity that follows
  external load, the capacity-shrunk state, the safety net, NEAR_LIMIT,
  wall-time and output limits, the audit log, quota suggestions,
  Prometheus metrics and per-lease traces (F5-F11, F30, F32).
- **M4, agent tree and communication: done** except where noted below.
  The lead's terminal and `subactl run -it`, mini-agent's self-wrapping
  and adapter, identity by cgroup membership, the inbox, graceful
  termination, declared policies, progress beacons, idle detection,
  `subactl top`, and the skill in [skill/subactl.md](skill/subactl.md).
- **M5, experiments: done.** Three groups, five runs each, on the radix
  upgrade task: without subactl every run leaves processes behind and hits
  the memory cap; with it none does. Two of the four hypotheses hold, one
  holds without having been put under strain, one is refuted. See
  [the results](examples/mini-agent/bench/results/README.md).
- **The real brain** speaks the Anthropic Messages API over `net/http` and
  has been run against DeepSeek and Z.AI through their compatible
  endpoints, on its own and as a lead with child agents under subactl.
  Against Anthropic's own API it is tested with recorded streams only.
- **One acceptance criterion is undecided: F27's CPU bound.** Next to a
  lead that keeps every CPU busy, a quiet lead's p95 task time rose by 50%
  to 82% on the two virtual machines available, where the design allows
  20%. But on the same VM a task slowed by 143% with no cgroups and a CPU
  left free, just because the other CPUs were busy: its virtual CPUs are
  not independent, so the bound cannot be measured there. It needs a
  bare-metal Linux machine; `SUBACTL_F27_STRICT=1 make integration`
  enforces it. The control-plane half of F27 holds.
- Not done: the macOS monitor and Windows backends (M6).

## Try it

```sh
make build
export SUBACTL_SOCK=/tmp/subactl.sock
bin/subactld --backend fake --capacity 24G &   # on Linux as root, drop --backend fake
bin/subactl host
bin/subactl run --wait=false --memory 8G -- agent     # prints the lead lease
bin/subactl spawn --parent <lead> --memory 2G -- worker
bin/subactl ls
bin/subactl terminate <lead> --grace 1s
```

## The baseline experiment

`mini-agent` plays a scripted scenario: a lead fans out one worker per
package, workers start dev servers and browsers, and at a fixed time the
usage limit is hit and the harness kills each subagent's pid and nothing
else. `bench` runs that inside a blast chamber, an outer cgroup with a hard
memory cap, and measures what happens.

```sh
# The toy task needs no toolchain and runs anywhere.
bin/mini-agent --mode without --scenario examples/mini-agent/scenarios/toy.yaml

# Under subactl: start the daemon (root, Linux), then run the lead.
sudo bin/subactld --socket-group "$(id -gn)" &
bin/mini-agent --mode with --scenario examples/mini-agent/scenarios/toy.yaml
bin/subactl top

# With a real model instead of the script. The key is read from the
# environment: DEEPSEEK_API_KEY, ZAI_API_KEY or ANTHROPIC_API_KEY.
bin/mini-agent --mode with --brain real --provider deepseek --prompt "..."

# Group A on the real task (radix-ui/primitives to React 19), five runs,
# in a Docker chamber capped at 12G. Needs Docker; the first run prepares
# the task environment.
examples/mini-agent/bench/chamber/run.sh A examples/mini-agent/scenarios/radix.yaml 5 /tmp/bench/A
# ... the same for B and C, then:
bin/bench report --in /tmp/bench
```

## Develop

```sh
make lint test
```

The project uses only the Go standard library, `golang.org/x/sys` and
`golang.org/x/term`; a test fails on any other import.
