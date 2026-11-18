# Group L, informed (round 5)

In round 4 the model as planner (group L) completed 24% against C's 53%,
and most of its envelopes were below the suggestion. Here the same model
(DeepSeek `deepseek-v4-pro`) gets a prompt that says how the suggestion is
made (the 95th percentile of past peaks, times 1.2), that envelopes below it
lose work, and to start as many workers as fit
(`scenarios/radix-llm-planner-informed.yaml`). Everything else is round
4's group L: same scenario, machine and chamber, workers scripted, history
carried from run to run. Five runs on 2026-11-17, from commit `f9a9f6b`.

## Result

| run | 1 | 2 | 3 | 4 | 5 | mean |
| --- | --- | --- | --- | --- | --- | --- |
| completion | 9% | 19% | 24% | 39% | 19% | 22% |
| upgrade workers started | 11 | 18 | 35 | 33 | 20 | |
| their envelopes | 2.0GiB | 2.1 to 4.4GiB, most 2.9 | 2.9GiB | 2.9GiB | 2.9GiB | |
| over their limit (413) | 2 | 6 | 20 | 8 | 6 | |

For comparison, round 4: L 24% (11% to 39%), B 41%, C 53%. Like every
group under subactl, no process was left and the chamber's limit was never
reached.

**Telling the model how to size its workers fixed the sizing, not the
result.** From run 3 on every upgrade worker got the suggestion, 2.9GiB,
the envelope group C uses; round 4's planner gave between 1.7 and 5.9GiB.
Completion stayed where it was.

**What limits the model as planner is how many workers it keeps running.**
B and C start 80 to 95 upgrade workers in a run; the informed planner
started 11 to 35. It dispatches by hand, one bash call at a time, and a
model's turn takes from seconds to minutes. In run 1 it twice waited for
its children inside a bash call until the tool's two-minute timeout. Run
4, the best, is the exception that shows it: after six minutes of reading
the task, the model wrote its own dispatcher (`planner_monitor.py`), handed
it the rest of the run, and kept the slots full with 13 turns in all.

## How far this reaches

- One model, one prompt, five runs, as in round 4.
- The scenario directory is mounted in the chamber at `/sc`, readable by
  the agents, and the planner looked there in every run of this group, as
  it did in 13 of round 4's 15 L runs (`L/`, `L-before-guard/`): the
  scenario file with its comments and group B's envelopes, once
  `scenario.go`. Its envelopes do not follow B's (3.5GiB), and nothing
  there tells it how to dispatch, but a later round should not mount the
  scenario where agents can see it.
- What a planner model would need is not better numbers but a loop: a
  dispatcher it starts once, as in run 4, or one that subactl or the
  harness provides. Group C is that loop with the suggestion built in.
