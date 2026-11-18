English | [简体中文](README.zh-CN.md)

# subactl

A single-host resource controller for AI agents that start other agents.

A coding agent working on a large task fans out subagents, and each
subagent starts its own processes: type checkers, test runners, dev
servers, headless browsers. On one machine this goes wrong in familiar
ways. Memory runs out and the kernel kills something at random, often the
agent that was doing the work. When a subagent is stopped, by its planner,
by a usage limit or by a crash, the processes it started keep running,
holding memory nobody accounts for. And nothing tells the planner how much
room there is before it starts the next worker.

subactl puts every agent in a **lease**: a group of processes with a memory
envelope that comes out of its parent's. It keeps three promises:

- **Memory is never oversold.** A lease is admitted only if its envelope
  fits; a lease over its own limit is ended with everything below it, with
  code 413, and its neighbours do not notice.
- **When a lease ends, everything in it is gone**: every process it
  started, however it was started, and every lease below it.
- **The machine stays up.** When something outside the agents takes the
  memory, subactl stops admitting, warns the planners, and if that is not
  enough ends one lease at a time, before the kernel would.

Around this: suggested envelopes from measured peaks, NEAR_LIMIT warnings,
wall-time and output limits, termination codes that say why each lease
ended, an audit log, Prometheus metrics, `subactl top` and per-lease
traces, and an inbox and progress beacons for the agent tree. `subactld`
is the daemon, `subactl` the CLI; the only contract is
[api/openapi.yaml](api/openapi.yaml), with a Go client in [client](client).

## Install the agent skill

The [subactl skill](skills/subactl/SKILL.md) teaches an agent to start,
size, watch and end child agents through subactl. Add it to your agent
with the [skills](https://github.com/vercel-labs/skills) CLI:

```sh
npx skills add zydo/subactl --skill subactl
```

or with GitHub's CLI: `gh skill install zydo/subactl`. The skill only
teaches the commands: the machine the agent runs on also needs `subactl`
on its `PATH` and a running `subactld` (see [Try it](#try-it) and
[Install as a service](#install-as-a-service-linux)).

## Platforms

subactl supports **Linux and macOS** only. On any other system, Windows
included, the build fails at once with
`undefined: subactl_supports_only_linux_and_macos`.

- **Linux** enforces: cgroup v2, a lease per cgroup, hard memory limits,
  kernel-side group kills, escape prevention, identity by cgroup
  membership. The daemon runs as root, under systemd if you like.
- **macOS** monitors: there is no cgroup, so the backend tracks each
  lease's processes from the process table, checks their physical
  footprint every 200ms and kills a lease that is over its limit. It
  reports every capability as soft or monitor_only, and a request that
  needs hard enforcement is refused. The daemon runs as an ordinary user.

## Does it work?

subactl is developed against a benchmark: a minimal multi-agent harness
upgrading the [radix-ui/primitives](https://github.com/radix-ui/primitives)
component library to React 19, one worker per package, each with its own
dev server and browser, run in a 12GiB "blast chamber" on a physical
Linux machine. In short:

| | packages done | processes left behind | chamber out of memory |
| --- | --- | --- | --- |
| without subactl | 4% | up to 116 per run | in every run |
| subactl, fixed envelopes | 41% | none, ever | never |
| subactl, suggested envelopes | 53% | none, ever | never |

With a real model doing the work instead of the script, the same: all
packages done either way, but without subactl every run left 84 to 153
processes and filled the chamber, and with it none did. Of the four
hypotheses set before the runs, one held, two were refuted, and one held
for fixed envelopes and failed for suggested ones; the details, including
what did not work, are in [docs/benchmark.md](docs/benchmark.md).

## Documentation

- [docs/tutorial.md](docs/tutorial.md): build your own multi-agent
  harness on subactl, with a working example in shell and in Go.
- [docs/design.md](docs/design.md): the design, annotated where the
  implementation differs.
- [docs/benchmark.md](docs/benchmark.md): the benchmark: the task, the
  harness, the setup, the hypotheses and the results.
- [docs/testing.md](docs/testing.md): the test suites, and the Linux and
  macOS machines they run on.
- [skills/subactl/SKILL.md](skills/subactl/SKILL.md): the agent skill, how an agent uses subactl.
- [api/openapi.yaml](api/openapi.yaml): the API.

## Status

All milestones are done: the contract and client (M0), the baseline
benchmark (M1), Linux enforcement (M2), quotas and observability (M3), the
agent tree and its communication (M4), the experiments (M5), and the macOS
backend (M6). Every acceptance criterion, F1 to F33, has an automated
test, on a real kernel where it needs one. CPU fairness (F27) is met on bare metal: next to a
lead that keeps every CPU busy, a quiet lead's task time rose 1% to 2%,
against 20% allowed.

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

With the benchmark's harness:

```sh
# The toy scenario needs no toolchain and runs anywhere.
bin/mini-agent --mode without --scenario examples/mini-agent/scenarios/toy.yaml

# Under subactl: start the daemon (root, Linux), then run the lead.
sudo bin/subactld --socket-group "$(id -gn)" &
bin/mini-agent --mode with --scenario examples/mini-agent/scenarios/toy.yaml
bin/subactl top

# With a model instead of the script, over the Anthropic Messages API:
# Anthropic's own or a compatible endpoint. --provider picks a preset
# (bin/mini-agent -h lists them), --base-url and --model override it, and
# the key is read from the environment.
bin/mini-agent --mode with --brain real --prompt "..."
```

Running the full benchmark is described in
[docs/benchmark.md](docs/benchmark.md#running-it). To build a harness of
your own, start with [the tutorial](docs/tutorial.md).

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
make integration   # Linux: under sudo; elsewhere: in a privileged Docker container
```

The project uses only the Go standard library, `golang.org/x/sys` and
`golang.org/x/term`; a test fails on any other import. `make lint` also
checks that every public package fails to build on Windows and FreeBSD
with the message above.
