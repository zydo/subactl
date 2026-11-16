# Experiment results

Each directory holds one group's runs: `summary.json` for the group, and
per run a `summary.json`, the one-second `series.jsonl` and the lead's
log. They are produced by `bench/chamber/run.sh` and are kept so that the
conclusions can be checked against the data.

## Group A: without subactl (E1)

Scenario `scenarios/radix.yaml`, five runs on 2026-11-15, in a Docker
chamber (Linux 7.0 arm64, 10 CPUs) with a 12GiB memory cap.

| run | OOM kills in the chamber | processes left after 60s | memory they held | subtasks killed and redone | packages completed |
| --- | --- | --- | --- | --- | --- |
| 1 | 3734 | 30 | 11.6GiB | 32 | 0 of 54 |
| 2 | 3703 | 18 | 7.4GiB | 32 | 0 of 54 |
| 3 | 3712 | 6 | 1.4GiB | 32 | 0 of 54 |
| 4 | 3702 | 9 | 3.2GiB | 31 | 0 of 54 |
| 5 | 3764 | 6 | 1.5GiB | 32 | 0 of 54 |

- **H1 for A holds in 5 of 5 runs:** processes are left behind every time.
  They are Storybook dev servers and their helpers, started by subagents
  that were later killed by pid only.
- **H2 for A holds in 5 of 5 runs:** the chamber is pinned at its cap from
  the fan-out on, and the kernel's OOM killer runs thousands of times. The
  watchdog never declared a meltdown: the cgroup cap contained the damage,
  which is what the chamber is for.
- **Nothing completes.** With 32 workers of about 1.7GiB each in 12GiB,
  every worker is killed before it finishes.

What this does not show: the chamber here is an outer cgroup, not a
separate machine, so "the host goes down" is observed as OOM kills inside
the cap rather than as an unresponsive host.
