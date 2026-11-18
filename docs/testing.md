# Testing

subactl supports Linux and macOS; on any other system its packages fail to
build, with `undefined: subactl_supports_only_linux_and_macos`. This
document lists the test suites and
the machines they have run on, and what each machine can and cannot show.
The benchmark is described in [benchmark.md](benchmark.md).

## The suites

| suite | command | needs | what it covers |
| --- | --- | --- | --- |
| lint | `make lint` | Go | `gofmt`; `go vet` for the host, Linux and macOS; and that every public package fails to build on Windows and FreeBSD, only at its `unsupported.go` |
| unit | `make test` | Go | the import rule (nothing outside the standard library, `golang.org/x/sys` and `golang.org/x/term`); the ledger, admission, the lease lifecycle and termination codes on the fake backend; the API against `api/openapi.yaml`; the client and CLI; the audit log, suggestions and metrics; mini-agent's loop, tools and LLM client (against recorded streams); on macOS also the `darwin` backend, with real processes |
| integration | `make integration` | Linux, root, cgroup v2; Node for some tests | the cgroup v2 backend on a real kernel, one test per acceptance criterion (F1 to F33): nesting, whole-group reaping, escape prevention, fail-closed restart, memory isolation, the safety net and the capacity guard, wall and output limits, termination codes, identity by cgroup membership, multi-lead isolation and CPU fairness |
| benchmark | `bench/chamber/run.sh` | Docker, network for the first run | the radix task in a blast chamber; see [benchmark.md](benchmark.md) |

On Linux `make integration` runs the test binary under `sudo`; elsewhere
it runs it in a privileged Docker container. Tests that need a tool the
machine lacks (Node, zsh, fish) are skipped, unless `SUBACTL_REQUIRE_SHELLS`
names the shells that must be there.

## Linux

| machine | used for | notes |
| --- | --- | --- |
| Physical x86-64: Intel Core Ultra 9 285K (24 cores: 8 fast, 16 efficient), 60GB, Ubuntu 26.04, Linux 7.0, cgroup v2 | the benchmark (rounds 3 to 5); CPU fairness (F27) | the only machine where F27 can be measured: next to a lead that keeps every CPU busy, a quiet lead's p95 task time rose 1% to 2% on sixteen identical cores and 9% with fast and slow cores mixed, against 20% allowed. Root is needed only inside the chamber's privileged container |
| Virtual machine (VirtualBox on Apple Silicon): Ubuntu 26.04 arm64, Linux 7.0, 6 CPUs, 15GB, cgroup v2 | the integration suite during development, including the regression tests for bugs the benchmark found | its CPUs are not independent: a task slows far more than F27 allows with no cgroups at all, so F27's bound is not enforced there (`SUBACTL_F27_STRICT=1` enforces it) |
| Docker Desktop on Apple Silicon: linux/arm64, 10 CPUs | `make integration` on a Mac; benchmark round 2 | the same limit on CPU measurements as the VM |
| GitHub Actions `ubuntu-latest` | every push: lint, unit tests with bash, zsh and fish required, build, and the integration suite with Node | |

What the Linux backend needs from the kernel: cgroup v2 with the memory
and pids controllers (and cpu for CPU limits) delegated to the daemon.
On Linux 6.5 or later the daemon also identifies callers by pidfd
(`SO_PEERPIDFD`), which rules out pid reuse; older kernels fall back to
`SO_PEERCRED`. cgroup v1 is not supported.

## macOS

| machine | used for | notes |
| --- | --- | --- |
| Mac mini, Apple M4 Pro (14 cores), 64GB, macOS 27 | development; the `darwin` backend's tests with real processes; measuring how far a lease overshoots its limit between two checks | with a check every 200ms, a lease allocating at 100MiB/s went 8MiB over its limit and one allocating at 2GiB/s 284MiB, which is why the default reserve line on macOS is 25% of memory rather than 15% |
| GitHub Actions `macos-latest` | every push: unit tests (including the `darwin` backend) with bash, zsh and fish required, and the build | |

On macOS subactl monitors rather than enforces: there is no cgroup, so the
backend keeps its own membership table from the process table, samples
each process's physical footprint, and kills a lease that is over its
limit. It reports every capability honestly (soft or monitor_only, escape
prevention unsupported), so a request that requires hard enforcement is
refused. The daemon runs as an ordinary user. The benchmark has not been
run on macOS: its chamber is a Linux cgroup.
