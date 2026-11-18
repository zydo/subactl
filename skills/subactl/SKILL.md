---
name: subactl
description: Start, size, watch and end child agents with subactl, the resource controller for agents on one machine (Linux and macOS only, with subactl installed). Use it whenever you are about to delegate work to another agent or worker process, fan work out to several of them in parallel, choose how much memory a child gets, react to how a child ended (codes such as 413, 503, 520), or handle a "[subactl control message ...]" such as TERMINATE_REQUESTED. Also use it when SUBACTL_LEASE_ID is set in your environment, which means you are running under subactl.
compatibility: Linux and macOS only. Needs the subactl CLI on PATH and a running subactld; does not work on Windows or other systems.
---

# subactl

`subactl` is the only way to start a child agent. Each child runs in its own
resource group with a hard memory limit, carved out of yours. When a child
ends, every process it started is gone, however it was started. You can act
only on your own subtree. You are running under subactl when
`SUBACTL_LEASE_ID` is set; your own files belong in `$SUBACTL_SCRATCH`.

**First, check that subactl is here.** It runs only on Linux and macOS,
and only where it has been installed:

```bash
uname -s; command -v subactl && subactl host >/dev/null && echo ready
```

If the system is not Linux or Darwin, `subactl` is not on the `PATH`, or
`subactl host` fails (no daemon), this skill does not apply: do the work
without it, and do not try to install subactl unless the user asks.

## Start one child

```bash
subactl available                      # memory_free: what you can still hand out
subactl spawn --wait --kind worker --memory "$(subactl suggest worker 2>/dev/null)" --wall 30m \
  -- mini-agent --task tasks/react-dialog.md
```

- `--wait` blocks until the child ends and prints its termination record
  (JSON). Exit status: 0 if it ended 2xx, 3 if it ended otherwise, 1 if the
  request was refused (the reason is JSON on stderr). The record's fields:
  `code`, `name` (such as `OOM_LEASE_LIMIT`), `lease`, `kind`, `peak` and
  `limit` (bytes), `message`, and `evidence` (`exit_code`, `signal`,
  `top_procs`, ...).
- `--kind` groups history. `subactl suggest KIND` prints, in bytes, the 95th
  percentile of earlier peaks of that kind times 1.2 (2GiB until there are
  five samples). Give a child at least the suggestion: a smaller limit gets
  it killed (413) and its work lost. Use one kind per kind of work.
- `--memory` is a hard limit. Everything the child starts counts against
  it, and all of it counts against your own limit.
- `--grow-to 6G` lets the limit grow once, on a near-limit warning, if you
  have room. Nothing grows by itself otherwise.
- `--wall 30m` and `--output 100M` end a child that runs too long (408) or
  writes too much (414). `--dry-run` checks a request without doing it.

## Run several children at once

`spawn --wait` runs one child at a time. To keep many running, start a
dispatcher once and let it fill your memory, instead of starting children
one by one across your turns: each of your turns takes seconds to minutes,
and the slots stay empty meanwhile. A dispatcher in a few lines:

```bash
cat > "$SUBACTL_SCRATCH/dispatch.sh" <<'EOF'
#!/usr/bin/env bash
out="$SUBACTL_SCRATCH/results"; mkdir -p "$out"
run_one() {   # start one child; while there is no room, wait and ask again
  while :; do
    subactl spawn --wait --kind worker --memory "$(subactl suggest worker 2>/dev/null)" --wall 30m \
      -- ./worker.sh "$1" >"$out/$1.json" 2>"$out/$1.err"
    rc=$?
    [ $rc -eq 1 ] && grep -qE 'parent_budget|mem_budget|capacity_shrunk' "$out/$1.err" && { sleep 2; continue; }
    return $rc
  done
}
for t in "$@"; do run_one "$t" & done
wait
EOF
chmod +x "$SUBACTL_SCRATCH/dispatch.sh"
nohup "$SUBACTL_SCRATCH/dispatch.sh" $(cat tasks.txt) >"$SUBACTL_SCRATCH/dispatch.log" 2>&1 &
```

Then check on it in later turns: `subactl ls` for what runs now, the
`results/*.json` files for what has ended, `subactl wait-children --timeout 60s`
to wait a bounded time. A command that starts children should return at
once: many bash tools kill a command that times out together with every
process it started, children included.

## When a request is refused

| reason | meaning | what to do |
| --- | --- | --- |
| `parent_budget` | your own limit has no room for this | wait for a child to end, ask for less, or end a child |
| `mem_budget` | the machine has no unallocated room | same, or tell the user |
| `capacity_shrunk` | other load took the room; nothing new is admitted | wait; do not retry in a tight loop |
| `below_usage` | a limit cannot go below current use plus a margin | pick a higher value |
| `below_allocated` | a limit cannot go below what its children already hold | end or shrink a child first |
| `enforcement_unavailable` | you required hard enforcement (`--require-memory hard`) and this machine cannot give it | drop the requirement or tell the user |
| `parent_not_running` | the lease you named as parent is not running | spawn from a running lease |

A refusal is not a failure of the task: subactl does not queue, so waiting
and asking again is your job.

## Watch and steer

```bash
subactl ls                              # your subtree, with usage and activity
subactl resize LEASE --memory 6G        # a child's limit; never your own
subactl notify LEASE --type NEAR_LIMIT --reason "wrap up"
subactl trace LEASE                     # usage over time, events, how it ended,
                                        # and "spikes": tool calls that grew memory by 64MiB+
subactl trace LEASE --tree              # the same for the whole subtree, ended leases too
```

To get more for yourself, ask your parent: `subactl resize-request --memory 6G`.

## End

```bash
subactl terminate LEASE --grace 30s --code 204   # asks first, then SIGTERM, then kills
subactl kill LEASE                               # at once
subactl wait-children                            # before you exit
subactl done --result "$SUBACTL_SCRATCH/result.md"
```

Call `subactl done` when your task is finished, then exit. A finished agent
that stays alive is reaped after 60 seconds. Results go in files under
`$SUBACTL_SCRATCH`; your parent can read them after you are gone.

If you get a message marked `[subactl control message ...] TERMINATE_REQUESTED`,
stop starting work, save your progress to `$SUBACTL_SCRATCH`, end your
children (`subactl terminate LEASE --grace 10s` for each, or let them go
with you: when you exit they are ended too), report with `subactl done`,
and exit before the deadline. After the deadline you are killed anyway.

## Termination codes

| code | meaning | usual reaction |
| --- | --- | --- |
| 200, 204 | completed, or released by its planner | none |
| 413 | exceeded its memory limit | retry once with more memory, above its `peak`, or split the task; the same request fails again |
| 408, 414 | exceeded its wall time or output limit | a longer limit, or fix what runs away |
| 410 | finished or silent, and not collected in time | collect results sooner |
| 499 | you, or an ancestor, ended it | none |
| 451 | a process left its group and was caught | do not detach processes |
| 502 | its parent ended | none; it went with the tree |
| 503 | the machine ran short and it was chosen | retry later, at the same size; not the task's fault |
| 520 | exited with an error, crashed, or was killed from outside | read its output and `exit_code` or `signal`, then retry |
| 500, 504 | subactl itself failed | tell the user |
