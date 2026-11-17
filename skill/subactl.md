---
name: subactl
description: Start, size, watch and end child agents with subactl. Use it whenever you want to delegate work to another agent process on this machine.
---

# subactl

`subactl` is the only way to start a child agent. Each child runs in its own
resource group with a hard memory limit, nested inside yours. When a child
ends, every process it started is gone. You can only act on your own subtree.

## Start a child

```bash
subactl available                      # memory_free is what you can still hand out
subactl spawn --wait --kind worker --memory "$(subactl suggest worker)" --wall 30m \
  -- mini-agent --task tasks/react-dialog.md
```

- `--wait` blocks until the child ends and prints its termination record.
  The command exits 0 if the code is 2xx, 3 otherwise.
- `--kind` groups history: `subactl suggest KIND` returns the 95th percentile
  of past peaks times 1.2, in bytes.
- `--memory` is a hard limit. Everything the child starts counts against it,
  and all of it counts against your own limit.
- `--grow-to 6G` lets the limit grow once, on a near-limit warning, if you
  have room. Without it nothing grows by itself.
- `--dry-run` checks a request without doing it.

A refusal is JSON on stderr with a `reason`:

| reason | meaning | what to do |
| --- | --- | --- |
| `parent_budget` | your own limit has no room for this | wait for a child to end, ask for less, or end a child |
| `mem_budget` | the machine has no unallocated room | same, or tell the user |
| `capacity_shrunk` | other load took the room; nothing new is admitted | wait; do not retry in a loop |
| `below_usage` | a limit cannot go below current use plus a margin | pick a higher value |

## Watch and steer

```bash
subactl ls                              # your subtree, with usage and activity
subactl resize LEASE --memory 6G        # a child's limit; never your own
subactl notify LEASE --type NEAR_LIMIT --reason "wrap up"
subactl trace LEASE                     # usage over time, events, how it ended,
                                        # and "spikes": tool calls that grew memory by 64MiB+
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

## Termination codes

| code | meaning | usual reaction |
| --- | --- | --- |
| 200, 204 | completed, or released by its planner | none |
| 413 | exceeded its memory limit | more memory or a smaller task; the same request fails again |
| 408, 414 | exceeded its wall time or output limit | a longer limit, or fix what runs away |
| 410 | finished or silent, and not collected in time | collect results sooner |
| 499 | you, or an ancestor, ended it | none |
| 451 | a process left its group and was caught | do not detach processes |
| 502 | its parent ended | none; it went with the tree |
| 503 | the machine ran short and it was chosen | retry later; not the task's fault |
| 520 | killed from outside, or crashed | check the cause, then retry |
| 500, 504 | subactl itself failed | tell the user |

If you get a message marked `[subactl control message ...] TERMINATE_REQUESTED`,
save your progress, end your children and exit before the deadline.
