# H4 under real pressure (round 5)

H4: under external interference, B and C use the safety net at most once
per run, and planners are warned before it acts. In round 4 the
interference took 30% of the chamber, the memory left never fell below the
reserve line, and H4 held without being tested. Here the interference is
raised until the safety net has to act.

- Scenario `scenarios/radix-h4.yaml`: round 4's `radix.yaml` with the
  interference at 75% of the chamber (9GiB), starting at 5m30s and
  ramping up in 30 seconds, held for four minutes.
- Same physical machine and 12GiB chamber as round 4; reserve line 1.8GiB.
  Three runs each of B and C, on 2026-11-17. B from commit `9c2f681`,
  C from `f9a9f6b`, which fixes the bug the first C run found (below).

## Result

| group | run | completion | safety net | of them by the guard | warned first | processes left | chamber cap hit |
| --- | --- | --- | --- | --- | --- | --- | --- |
| B | 1 | 41% | 1 | 0 | yes | 0 | never |
| B | 2 | 43% | 1 | 0 | yes | 0 | never |
| B | 3 | 39% | 1 | 0 | yes | 0 | never |
| C | 1 | 48% | 2 | 1 | yes | 0 | never |
| C | 2 | 59% | 2 | 1 | yes | 0 | never |
| C | 3 | 54% | 2 | 1 | yes | 0 | never |

**H4 is refuted for C, and holds for B.** Every kill came after the
planners were told the machine was short (`CAPACITY_SHRUNK`, reason
`below_reserve`), and the chamber's limit was never reached, but every run
of C needed two kills.

In C, the interference rose faster than the safety net samples: the
capacity guard ended one worker in the kernel (503, `detected_by:
kernel`), which left 1.49GiB, still below the 1.8GiB line, and when its
five-second deadline ran out the safety net ended a second. C's envelopes
are tighter than B's, so its workers hold less (about 0.8GiB at that
moment) and one of them is not enough to give back what 9GiB of
interference takes. In B the net ended one worker and the memory left came
back above the line.

Completion stays where round 4 had it (B 41%, C 53%): the kills cost one
or two workers out of about 70 subtasks per run.

## The trials that did not test H4

Under subactl, admission stops as soon as the leases' envelopes exceed the
shrunken capacity (`CAPACITY_SHRUNK`, reason `allocated_exceeds_capacity`):
no new worker starts, and the ones running finish. The safety net can act
only when the interference alone leaves less than the reserve line plus
what the workers already running use. Before the setting above:

| interference | at | what happened |
| --- | --- | --- |
| 30% (round 4) | 7m | never below the reserve line |
| 50% (`B-50-percent/`) | 7m | admission stopped; the workers running finished seconds later; the agents held 1.15GiB for the rest of the interference; 43% completion, no kill |
| 75% | 7m | the same: both running upgrade workers ended 200 as the interference began (stopped, not kept) |
| 60% | 5m30s | the running workers peaked at 3.6GiB; the memory left never fell below 3GiB (stopped, not kept) |

The 50% run is kept as what this looks like when it goes well: admission
alone is enough, and nothing is killed.

## The bug the first run of C found

`C-before-fix/`: the run ended after six minutes at 15%. The capacity
guard was hit, and the kernel ended the lead with all its workers. Every
lease's cgroup had `memory.oom.group` set, and on a hit the kernel kills
the topmost cgroup with it set between its victim and the limit that was
hit: under the guard, which sits above all leases, that is the lead. The
lead was also reported as 520 (killed from outside), since the guard's hit
had already been counted for the worker. Commit `f9a9f6b` sets
`memory.oom.group` on each lease's `_self` leaf only, so the kernel ends
one lease's own processes and subactld ends the subtree of the lease whose
own limit was hit, as before. The capacity guard's integration test now
runs its burst in a worker of a lead and fails without the fix. C was run
again from the start with it; B's runs never hit the guard and are kept.
