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
- Next is M3: capacity tracking, the safety net, telemetry, audit and trace.

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

# Group A on the real task (radix-ui/primitives to React 19), five runs,
# in a Docker chamber capped at 12G. Needs Docker; the first run prepares
# the task environment.
examples/mini-agent/bench/chamber/run.sh A examples/mini-agent/scenarios/radix.yaml 5 /tmp/bench-a
```

## Develop

```sh
make lint test
```

The project uses only the Go standard library, `golang.org/x/sys` and
`golang.org/x/term`; a test fails on any other import.
