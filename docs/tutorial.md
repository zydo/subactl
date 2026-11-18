English | [简体中文](tutorial.zh-CN.md)

# Build your own multi-agent harness on subactl

This tutorial builds a small orchestrator: a lead that hands tasks to
worker agents, as many at once as its memory allows. It shows the pieces a
harness of your own needs: starting children, sizing them, collecting
results, reacting to how each one ended, and talking to a running tree.
The code is in [`examples/orchestrator`](../examples/orchestrator): the
lead as a shell script and in Go, and a worker that misbehaves on purpose.
[mini-agent](../examples/mini-agent), the harness of the
[benchmark](benchmark.md), is the full-size example of the same ideas.

## What subactl does for a harness

- **Every agent is a lease**: a group of processes with a memory limit
  (its *envelope*). A child's envelope comes out of its parent's, so a
  lead's envelope bounds its whole tree.
- **Starting a child is one call**, `subactl spawn` or `client.Acquire`
  plus `client.Spawn`. The daemon starts the process inside the lease;
  everything it starts in turn stays inside.
- **When a lease ends, everything in it ends**, however its processes were
  started (`nohup`, `setsid`, `&`), and so do the leases below it. Your
  harness needs no cleanup code; if the lead dies, the tree goes with it.
- **Every lease ends with exactly one code** that says why: 200 done, 413
  over its memory limit, 408 over its wall time, 503 the machine ran short,
  520 crashed or killed from outside, and so on.
- **Identity comes from the process.** A call from inside a lease acts as
  that lease; there are no tokens to pass around. A lease can act only on
  its own subtree.

What stays yours: what to run, how much to give it, what to do when it
fails. subactl validates, enforces and reports; it never decides.

## 1. Start the daemon

```sh
make build
export PATH=$PWD/bin:$PATH

# Linux: as root, or as the systemd service (see the README).
sudo bin/subactld --socket-group "$(id -gn)" &

# macOS: as yourself. Limits are checked every 200ms rather than enforced
# by the kernel; see "Platforms" in the README.
bin/subactld &

subactl host     # capacity, what is allocated, what the backend can enforce
```

Both the daemon and the CLI find the socket through `SUBACTL_SOCK`; set it
to the same path for both if you do not use the default.

## 2. A lead in fifteen lines of shell

A lead is any program started with `subactl run`. Inside it,
`SUBACTL_LEASE_ID`, `SUBACTL_SOCK` and `SUBACTL_SCRATCH` are set, and
every `subactl` call acts as that lease. The core of
[`fanout.sh`](../examples/orchestrator/fanout.sh):

```bash
run_one() {
  local task=$1
  while :; do
    subactl spawn --wait --kind worker --memory "$(subactl suggest worker 2>/dev/null)" --wall 5m \
      -- "$here/worker.sh" "$task" >"$out/$task.json" 2>"$out/$task.err"
    local rc=$?
    if [ $rc -eq 1 ] && grep -qE '"reason": *"(parent_budget|mem_budget|capacity_shrunk)"' "$out/$task.err"; then
      sleep 1; continue          # no room yet: wait and ask again
    fi
    return $rc
  done
}
for task in "$@"; do run_one "$task" & done
wait
```

- `spawn --wait` starts a child and blocks until it ends, printing its
  termination record (JSON). It exits 0 if the child ended 2xx, 3
  otherwise, and 1 if the request was refused.
- A refusal for lack of room is normal: the lead's envelope is full until
  a running child ends. subactl does not queue; the planner waits and asks
  again.
- `subactl suggest worker` gives the 95th percentile of past workers'
  peaks, times 1.2 (2GiB until there are five samples). Groups learn from
  their own history this way; `--kind` names the group.
- `--wall 5m` ends a child that runs too long (408). `--output 100M` would
  cap what it writes (414).

The worker, [`worker.sh`](../examples/orchestrator/worker.sh), does its
task, writes the result into its own scratch directory and reports it:

```bash
subactl beacon --state working --tool "$task"   # a progress beacon; costs no model call
...                                              # the work
echo "..." > "$SUBACTL_SCRATCH/result.txt"
subactl done --result "$SUBACTL_SCRATCH/result.txt"
```

Its task `leak` allocates without end, and its task `server` leaves a
"dev server" running in the background, as agents do. Run it:

```console
$ subactl run --memory 4G -- examples/orchestrator/fanout.sh a b c leak server d
a: 200 COMPLETED
b: 200 COMPLETED
c: 200 COMPLETED
leak: 413 OOM_LEASE_LIMIT
server: 200 COMPLETED
d: 200 COMPLETED
$ pgrep -f 'sleep 600' || echo nothing left
nothing left
```

With 4GiB and the default 2GiB per worker, two workers run at a time.
`leak` was ended at its limit without touching the others, and the dev
server that `server` left behind was ended with its lease.

## 3. The same lead in Go

[`main.go`](../examples/orchestrator/main.go) does the same through the Go
client, and adds a policy for what to do with each ending:

```go
c := client.New(client.SocketFromEnv())   // identity comes from this process

av, _ := c.Available(ctx, api.Self, "worker")            // room left, and suggestions
ls, err := c.Acquire(ctx, api.AcquireRequest{            // no Parent: a child of this lease
    Kind:     "worker",
    Envelope: api.Envelope{Memory: av.Suggest["worker"].Memory, Wall: 300},
})
var refusal *api.Error
if errors.As(err, &refusal) && refusal.Reason == api.ReasonParentBudget { /* wait, ask again */ }

c.Spawn(ctx, ls.ID, api.SpawnRequest{Argv: []string{worker, task}, Env: os.Environ(), Dir: cwd})
res, _ := c.WaitChildren(ctx, ls.ID, api.WaitChildrenRequest{Self: true})  // its termination record
```

The policy in `runTask` is an example of the planner's side of the split:
a worker that went over its limit (413) gets one more try with twice the
memory; one ended by the safety net (503) is retried later, since the
machine, not the task, was the problem; anything else stays as it ended.

```console
$ go build -o bin/orchestrator ./examples/orchestrator
$ subactl run --memory 4G -- bin/orchestrator -worker examples/orchestrator/worker.sh a b c leak server d
leak: 413 OOM_LEASE_LIMIT, peak 1339 MiB of 8 MiB
b: 200 COMPLETED, peak 2 MiB of 4 MiB
...
```

This run came after the shell run, so `suggest` had five samples of
workers that peaked at about 2MiB and now suggests 4MiB instead of the
2GiB default. The `leak` line was recorded on macOS: there the limit is
checked every 200ms, and a process that doubles its memory each step gets
far past a small limit before it is caught. On Linux the kernel stops it
at the limit.

## 4. Children that start children

A worker can be a planner too: from inside its lease, `subactl spawn`
starts a child of the worker, whose envelope comes out of the worker's.
Nothing else changes. To keep part of a parent's envelope for its own
processes, give it `--memory-self`; its children can then use only the
rest.

## 5. Talking to a running tree

| you want to | from the planner | in the child |
| --- | --- | --- |
| know it is alive and what it does | `subactl ls`, `subactl trace LEASE` | `subactl beacon --tool NAME` before each tool call |
| get its result | read the path from its `DONE` event, or its scratch directory | `subactl done --result FILE` |
| ask it to stop | `subactl terminate LEASE --grace 30s` | read `TERMINATE_REQUESTED` from the inbox, save, exit |
| give it more memory | `subactl resize LEASE --memory 6G` | `subactl resize-request --memory 6G` asks the parent |
| grow it automatically | `spawn --grow-to 6G`: once, on NEAR\_LIMIT, if there is room | nothing |

A child that reports `done` and stays alive is reclaimed after 60 seconds
(410); one silent for 30 minutes after its last beacon is reclaimed too.
`terminate` asks first, then sends SIGTERM, then kills; the termination
record says at which step it ended.

**If your agents are LLMs**, add a small adapter to the agent loop, as
mini-agent does in
[`agent/adapter.go`](../examples/mini-agent/agent/adapter.go):

- before each tool call, `c.Progress(ctx, api.Self, api.ProgressRequest{State: "working", Tool: name, Seq: n})`;
- subscribe to `c.Inbox(ctx, api.Self, ...)` and, at the next tool call or
  turn, inject what arrived as a system message with a fixed template,
  such as `[subactl control message from <source>] <type> "<reason>"`;
- after `TERMINATE_REQUESTED`, run no new tool;
- when the task is finished, `api.ProgressRequest{State: "done", Result: path}`.

Only subactl and the agent's ancestors can write to its inbox, and the
message types are a closed set, so the channel cannot be used to inject
arbitrary text.

## 6. Letting a model plan

Give the model the [subactl skill](../skills/subactl/SKILL.md) (`npx skills add zydo/subactl --skill subactl`): a page on
spawning, sizing, ending, and the termination codes. Then give it a loop
rather than asking it to dispatch one worker per turn. In the benchmark, a
model that planned by hand, one bash call per turn, started 11 to 35
workers in a run where a simple loop started 80 to 95, and completed less
than half as much as the loop, even with the same envelopes; its best run was the
one in which it wrote itself a dispatcher. A script like `fanout.sh`,
which the model starts once with its own task list, is that loop. Tell
the model to start children with a command that returns at once
(`... >log 2>&1 &`) and to check on them in later calls, never to wait for
them inside a bash call: a harness that ends a timed-out command's process
group, as mini-agent does, ends the children started in it too, in the
background or not.

## 7. Watching it

```sh
subactl top                 # every lead's tree, use against envelope, state, recent events
subactl ls                  # your subtree, as JSON
subactl trace LEASE --tree  # usage over time, events, beacons, and "spikes":
                            # the tool calls during which memory grew by 64MiB or more
subactl metrics             # Prometheus text
```

## Checklist

- Run the lead with `subactl run`, and nothing outside it: a process
  started some other way is not in the tree and not counted.
- Size children with `subactl suggest KIND`, and give each kind of work
  its own kind.
- Treat `parent_budget`, `mem_budget` and `capacity_shrunk` as "wait",
  never as failure; do not retry in a tight loop.
- Put results in files under `$SUBACTL_SCRATCH`; a parent can read a
  child's scratch directory until the parent ends.
- Decide per termination code what to do; the codes say whose problem it
  was (4xx the task's, 5xx the environment's).
- subactl does not sandbox files or the network, and does not catch a
  process started for an agent by a third party (`systemd-run --user`, a
  tmux server, docker). See the non-goals in the [design](design.md).
