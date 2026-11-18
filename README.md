# subactl

A single-host resource controller for subagents: a quota ledger that never
oversells memory, and a guarantee that when an agent's group ends, every
process in it is gone. `subactld` is the daemon, `subactl` the CLI.

The design is in [DESIGN.md](DESIGN.md). The only contract is
[api/openapi.yaml](api/openapi.yaml).

## Status

- **M0, contract and skeleton: done.** The contract, the public Go client,
  the in-memory ledger, the lease lifecycle and the CLI.
- **M1, baseline: done.** `examples/mini-agent` in `without` mode hits
  the memory cap in five of five runs and leaves processes behind in four
  (in the fifth the kernel's OOM killer took the orphans too); see
  [the results](examples/mini-agent/bench/results/README.md).
- **M2, Linux enforcement core: done.** The cgroup v2 backend, the pre-exec
  shim, nested leases, whole-group reaping, escape prevention and
  fail-closed restart. Integration tests named after the acceptance
  criteria run on a real kernel with `make integration`: under sudo on
  Linux, in a privileged Docker container elsewhere.
- **M3, quota and observability: done.** Effective capacity that follows
  external load, the capacity-shrunk state, the safety net, NEAR_LIMIT,
  wall-time and output limits, the audit log, quota suggestions,
  Prometheus metrics and per-lease traces (F5-F11, F30, F32).
- **M4, agent tree and communication: done.**
  The lead's terminal and `subactl run -it`, mini-agent's self-wrapping
  and adapter, identity by cgroup membership, the inbox, graceful
  termination, declared policies, progress beacons, idle detection,
  `subactl top`, and the skill in [skill/subactl.md](skill/subactl.md).
- **M5, experiments: done.** Groups of five runs on the radix upgrade
  task, on a physical x86-64 machine: without subactl every run hits the
  memory cap and four of five leave processes behind; with it no run of
  fifteen does either. Completion is 4% without subactl, 41% with fixed
  envelopes, 53% with suggested ones, and 24% with DeepSeek as the planner
  sizing the workers itself. One hypothesis holds, one holds without having
  been put under strain, two are refuted. With DeepSeek as lead and workers
  doing the real work, the same holds: everything left behind and a full
  chamber without subactl, nothing with it. See
  [the results](examples/mini-agent/bench/results/README.md).
- **The real brain** speaks the Anthropic Messages API over `net/http` and
  has been run against DeepSeek and Z.AI through their compatible
  endpoints, on its own and as a lead with child agents under subactl.
  Against Anthropic's own API it is tested with recorded streams only.
- **F27's CPU bound is met on bare metal.** Next to a lead that keeps
  every CPU busy, a quiet lead's p95 task time rose by 1% to 2% on sixteen
  identical cores of a physical machine, and by 9% with fast and slow
  cores mixed; the design allows 20%. On virtual machines the bound cannot
  be measured, because their CPUs are not independent: there a task slows
  far more than that with no cgroups at all. `SUBACTL_F27_STRICT=1 make
  integration` enforces the bound.
- **M6, macOS: done, as monitoring only.** The `darwin` backend keeps a
  membership table from the process table, samples physical footprint and
  kills a lease that is over its limit, checking every 200ms; its default
  reserve line is 25% of memory rather than 15%, for what a lease
  allocates between two checks. It reports every
  capability as soft or monitor_only and escape prevention as unsupported,
  so a request that requires hard enforcement is refused. `subactld` runs
  there as an ordinary user. The effective capacity follows the kernel's
  own memory-pressure figure, in whole percent and after compression, so
  it moves less than the memory other programs allocate.
- Only Linux and macOS are supported; Windows is out of scope.

## Try it

```sh
make build
export SUBACTL_SOCK=/tmp/subactl.sock
bin/subactld --backend fake --capacity 24G &   # a dry run: nothing is started or enforced
# For real: on Linux as root, "sudo bin/subactld"; on macOS, "bin/subactld".
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

## Install as a service (Linux)

```sh
sudo install -m 0755 bin/subactld bin/subactl /usr/local/bin/
sudo groupadd --system subactl          # who may start agents
sudo groupadd --system subactl-admin    # who may act on every lead
sudo install -m 0644 deploy/subactld.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now subactld
```

The unit restarts the daemon at once if it dies and gives it an OOM score
of -1000. Its leases live in the service's delegated cgroup, so when the
daemon dies systemd reaps them together with it: nothing an agent started
outlives the daemon, whether or not it comes back.

## Develop

```sh
make lint test
```

The project uses only the Go standard library, `golang.org/x/sys` and
`golang.org/x/term`; a test fails on any other import.
