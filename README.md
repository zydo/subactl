# subactl

A single-host resource controller for subagents: a quota ledger that never
oversells memory, and a guarantee that when an agent's group ends, every
process in it is gone. `subactld` is the daemon, `subactl` the CLI.

The design is in [DESIGN.md](DESIGN.md). The only contract is
[api/openapi.yaml](api/openapi.yaml).

## Status

Milestone M0 (contract and skeleton) is done: the contract, the public Go
client, the in-memory ledger, the lease lifecycle and the CLI all run
against a fake backend that starts no processes and enforces nothing.
Real enforcement on Linux cgroup v2 lands in M2.

## Try it

```sh
make build
export SUBACTL_SOCK=/tmp/subactl.sock
bin/subactld --backend fake --capacity 24G &
bin/subactl host
bin/subactl run --wait=false --memory 8G -- agent     # prints the lead lease
bin/subactl spawn --parent <lead> --memory 2G -- worker
bin/subactl ls
bin/subactl terminate <lead> --grace 1s
```

## Develop

```sh
make lint test
```

The project uses only the Go standard library, `golang.org/x/sys` and
`golang.org/x/term`; a test fails on any other import.
