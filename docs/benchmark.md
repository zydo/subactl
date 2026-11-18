English | [简体中文](benchmark.zh-CN.md)

# The benchmark

subactl is developed against a benchmark of its own: a small multi-agent
harness, `mini-agent`, given a real engineering task, run in a sealed
"blast chamber" with and without subactl, and measured. This document says
what the task is, how the harness behaves, how a run is set up and
measured, and what the runs have shown. The code is in
[`examples/mini-agent`](../examples/mini-agent); the measurements, reports
and charts are in
[`examples/mini-agent/bench/results`](../examples/mini-agent/bench/results).

## The task: upgrade a real UI library to React 19

The agents upgrade [radix-ui/primitives](https://github.com/radix-ui/primitives),
a widely used React component library, to React 19.

- **Starting point.** The repository at commit `af1e3b4`, the parent of
  upstream pull request [#2934](https://github.com/radix-ui/primitives/pull/2934)
  ("React 19 compatibility"): a yarn workspace on Node 18 with 54 packages
  under `packages/react/`, a repository-wide `tsc` type check, jest tests,
  and Storybook 7 for the component stories.
- **The work.** A lead agent lists the packages and sends one worker per
  package. Each worker gets its own git worktree, moves the package to
  React 19, runs the type check and the package's tests, starts a Storybook
  dev server and renders the package's stories in a headless Chromium
  (through Playwright) to check that they still render. When a check
  fails, the worker starts a fixer below it, which may start one of its
  own: up to three levels.
- **Done means:** no type errors in the package, its jest tests pass, and
  its stories render without console errors.
- **Where the memory goes.** The type check is fast (about six seconds);
  the weight is Storybook's webpack build plus a browser, about 1.7GiB per
  worker. Several workers want Storybook's port 9009 at once, and a worker
  that starts a dev server "to have a look" tends to leave it running.
  That is the pressure the benchmark is about, and it comes from the task's
  own tools, not from anything simulated.

The task environment (the cloned repository, its dependencies installed
once and hard-linked into every worktree, Playwright and the baseline
screenshots) is prepared once by
[`scenarios/files/radix/prepare.sh`](../examples/mini-agent/scenarios/files/radix/prepare.sh).
The steps the agents call are small scripts beside it: `setup.sh`,
`typecheck.sh`, `test.sh`, `preview.sh`, `fix.sh`, `cleanup.sh`.

## The harness: mini-agent

`mini-agent` is a minimal multi-agent harness in Go, built on subactl's
public client and CLI only. It gives the model four tools, `read`,
`write`, `edit` and `bash`, and no special tool for subagents: an agent
starts a child by running `mini-agent --task ...` from bash. Run from a
shell, it becomes the lead; under subactl, each child gets its own lease,
nested in its parent's.

It has two brains behind the same loop, tools and harness:

- **scripted**: a seeded script that plays the scenario. Runs are
  deterministic and cost nothing, so groups can be compared run for run.
  The comparison experiments use it.
- **real**: any model served over the Anthropic Messages API, the provider's
  own or a compatible endpoint (`--provider`, `--base-url`, `--model`; the
  key is read from the environment). It reads the failures, edits the code
  and runs the checks itself.

`--mode without` runs the agents as plain child processes, the way common
harnesses do; `--mode with` runs them under subactl. Nothing else differs.

## Greedy behaviour

The script switches on six behaviours that agents show in practice, each
taken from a reported failure of a real coding-agent harness. A real
model shows some of them unprompted: in the real-model runs below, the
workers left their dev servers running without being told to.

| | behaviour | in this task |
| --- | --- | --- |
| G1 | fanning out too far | one worker per package, up to 32 at once, fixers three levels deep |
| G2 | leaving servers and browsers running | dev servers and Chromium are started and never stopped |
| G3 | context growing without end | every agent's transcript buffer grows 8MB a step and is never freed |
| G4 | no cleanup on the error path | at the usage limit the harness kills each subagent's pid, and nothing it started |
| G5 | runaway output | one worker is stuck at an interactive prompt and writes to stdout without end |
| G6 | unbounded parallelism | one worker in four runs type checks and tests at four times the cores, with an unbounded file walk |

## A run

Every run follows the same schedule, from
[`scenarios/radix.yaml`](../examples/mini-agent/scenarios/radix.yaml):

| time | event |
| --- | --- |
| 2m | the fan-out starts (G1) |
| 5m | the model provider's usage limit: the harness kills the running subagents' pids only (G4) |
| 7m | a process that is not an agent takes 30% of the machine for four minutes |
| 9m | runaway output starts (G5) |
| 15m | the scenario ends; the chamber is watched for 60 more seconds |

**The blast chamber.** Without subactl the point is to exhaust the
machine, so each run happens inside an outer cgroup capped at 12GiB, in a
privileged Docker container. "The host runs out of memory" means the
chamber hits its own cap. Under subactl the daemon runs inside the chamber
and treats the chamber as the whole machine, with a reserve line of 15%.

**The groups.** Each compared on the same script, seed and machine:

| group | setup | question |
| --- | --- | --- |
| A | no subactl | does the problem reproduce? |
| B | subactl, fixed envelopes (lead 9GiB, worker 3.5GiB, fixer 1GiB) | do never overselling and hard limits make it safe? |
| C | subactl, envelopes from `subactl suggest` (the 95th percentile of past peaks, times 1.2), grown by half on NEAR_LIMIT, history kept across runs | does measuring make it safe and use the machine better? |
| L | as C, but the lead is a real model that decides each worker's envelope and how many run at once; the workers stay scripted | can a model plan the memory as well as a rule? |

**What is measured.** Peak memory of the chamber; how often its cap was
hit; processes left 60 seconds after the end and the memory they hold; the
share of packages done; work lost to kills; mean use of the chamber; how
often the safety net acted; and how long the run took. `bench report`
turns the measurements into a report, charts and a Parquet file of the
per-second series.

**Hypotheses**, fixed before the runs and judged by code in `bench report`:

1. **H1.** After the usage-limit kill, A leaves processes behind; B and C
   never do.
2. **H2.** A hits the chamber's cap at least once in five runs; B and C
   never do.
3. **H3.** C's mean use of the machine is at least 15 points above B's,
   and C loses no more work than B.
4. **H4.** Under external interference, B and C need the safety net at most
   once per run, and planners are warned before it acts.

## Results

Physical x86-64 machine (24 cores, 60GB), Linux 7.0, 12GiB chamber; the
machines are described in [testing.md](testing.md). Each row is a group's
mean or range over its runs. The benchmark runs on Linux only: its chamber
is a cgroup.

### Round 4: the comparison (five runs per group)

| group | packages done | processes left | chamber cap hit |
| --- | --- | --- | --- |
| A, no subactl | 4% | 38, 55, 0, 12, 116 | in every run |
| B, fixed envelopes | 41% | 0 in every run | never |
| C, suggested envelopes | 53% | 0 in every run | never |
| L, a model as planner | 24% (11% to 39%) | 0 in every run | never |

| | verdict | why |
| --- | --- | --- |
| H1 | refuted, in one run of A | in A's third run the kernel's OOM killer killed 2,540 processes, the orphans among them, so none were left to count; in B, C and L nothing was ever left |
| H2 | holds | |
| H3 | refuted | C's use of the machine is 4.3 points above B's, not 15, and C loses more work (21.4 against 15.2 subtasks): tighter envelopes let more workers run and finish, and let the greedy ones hit their limits more often |
| H4 | holds, but untested | the interference never pushed B or C below the reserve line |

Completion compares the groups; it does not measure the task. Not all 54
packages fit into 15 minutes at two or three workers at a time, and one
package, `slot`, fails under React 19 in every run.

### Round 5: H4 under real pressure, and a better-informed planner

**H4** was run again with the interference at 75% of the chamber, arriving
while workers run, so that the safety net has to act
([results](../examples/mini-agent/bench/results/h4/README.md)):

| group | packages done | safety net kills per run | warned first | processes left | chamber cap hit |
| --- | --- | --- | --- | --- | --- |
| B | 39% to 43% | 1 | always | 0 | never |
| C | 48% to 59% | 2 | always | 0 | never |

H4 holds for B and is refuted for C: C's tighter envelopes mean one killed
worker gives back too little, so a second goes. At 50% interference no
kill was needed at all: subactl stops admitting new leases as soon as the
machine is short, and the running ones finish on their own. This round
also found a bug, now fixed: the kernel's guard over all leases ended a
whole lead with its workers rather than one worker.

**The model as planner, told how to size its workers**
([results](../examples/mini-agent/bench/results/L-informed/README.md)):
it then gave every worker exactly the suggested envelope, as C does, and
still completed only 22% (9% to 39%). The limit was dispatch, not sizing:
B and C start 80 to 95 workers in a run, the model 11 to 35, one bash call
per turn with seconds to minutes between turns. Its best run was the one in
which it wrote itself a dispatcher script and let it run.

### A real model doing the work

With a real model as the lead and as every worker, editing the code
itself, on eight of the packages
([results](../examples/mini-agent/bench/results/real-model/README.md)):

| group | runs | packages done | processes left | memory they held | chamber cap hit |
| --- | --- | --- | --- | --- | --- |
| A, no subactl | 3 | 8/8 each | 153, 84, 141 | 4.25, 1.66, 2.94GiB | 3,938, 78,563 and 9,432 times |
| B, subactl | 2 | 8/8 each | 0 | | never |

The model left Storybook and esbuild running on its own, without a script
telling it to. H1 and H2 hold with a real model as they do with the script.

### What this does not show

- The chamber is a cgroup, not a separate machine.
- One task, one machine, five runs per group (two or three with a real
  model); the directions are consistent, the numbers are not precise
  estimates.
- The model-as-planner results come from one model and one prompt.
- How each round was run, what went wrong and was redone, and the full
  tables are in the
  [results README](../examples/mini-agent/bench/results/README.md) and the
  design's [comparison experiments](design.md#comparison-experiments).

## Running it

```sh
make build

# The toy scenario needs no toolchain and runs anywhere.
bin/mini-agent --mode without --scenario examples/mini-agent/scenarios/toy.yaml

# The radix task, five runs of a group in a Docker chamber capped at 12G.
# Needs Docker and network access; the first run prepares the task
# environment, which takes several minutes.
examples/mini-agent/bench/chamber/run.sh A examples/mini-agent/scenarios/radix.yaml 5 /tmp/bench/A
examples/mini-agent/bench/chamber/run.sh B examples/mini-agent/scenarios/radix.yaml 5 /tmp/bench/B
examples/mini-agent/bench/chamber/run.sh C examples/mini-agent/scenarios/radix.yaml 5 /tmp/bench/C
bin/bench report --in /tmp/bench

# Group L, or any group with a real model as the lead: pass the brain
# through BENCH_ARGS and the key in the environment.
BENCH_ARGS="--brain real" examples/mini-agent/bench/chamber/run.sh L \
  examples/mini-agent/scenarios/radix-llm-planner.yaml 5 /tmp/bench/L
```

The other scenarios are `radix-real.yaml` (a real model as lead and
workers, eight packages), `radix-h4.yaml` (round 5's interference),
`radix-llm-planner-informed.yaml` and `radix-smoke.yaml`.
