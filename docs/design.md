English | [中文](design.zh-CN.md)

# Subagent Controller: single-host resource management for subagents

Nov 4, 2026 · @Karl · Revised 2026-11-17 to match the v1 implementation: wherever the implementation differs from the original design, or a decision was made during implementation, the text now describes what was actually built and is marked "(implementation)". A summary is in "Implementation record" at the end.

Subagent Controller is a resource controller for subagents on a single host. It consists of a daemon, `subactld`, and a CLI, `subactl`, both written in Go; the accompanying demo harness, `mini-agent`, is also in Go. The core of v1 is two promises: keep a quota ledger that never oversells, and guarantee that when an acgroup ends, every process in it ends. On top of that, subactl is the only way to create an agent: a person starts the lead agent from their own shell with `subactl run -it`, and agents start child agents from their bash tool with `subactl spawn`; together they form one agent tree. subactl is also the only control channel between a planner and its child agents. How much to allocate, when to grow, whom to end: all of that is the planner's policy; subactld only validates, executes and reports. The "Controller" in the name means a control tool (the "ctl" of systemctl), not a controller that actively reconciles in the Kubernetes sense.

## Background and positioning

The triggering problem: when several agents are orchestrated on one machine, subagents often end by an abnormal path: killed from outside, crashed, or cut off by an upstream service. The heavy child processes they started (a browser, for example) are not reclaimed. Meanwhile, mainstream agents constrain subagents only by "how many", never by "how much they use" (see the survey below). Together, the two eventually exhaust the machine's memory. The problem recurs across mainstream coding agents, and the community has only after-the-fact reaper scripts. Cluster-side agent orchestration (such as Google's Scion / AX / Agent Substrate) targets containers and Pods, a different setting from a process tree on one machine; it is a reference, not a point of comparison.

**Survey: mainstream agents use a count limit instead of resource awareness** (surveyed 2026-11. Codex, OpenCode and Pi from source, at openai/codex@b016cd1, sst/opencode@d432a06 and earendil-works/pi@c3cc916; Claude Code is not open source, so from its official documentation):

|  | Concurrency limit | Nesting depth | Checks free memory/CPU before dispatching | Process cleanup |
| --- | --- | --- | --- | --- |
| Claude Code | 20 running at once per session by default (`CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS`, since v2.1.217); ultracode is exempt; resuming a finished subagent does not check the limit | 3 levels by default | No; not mentioned in the docs | The docs admit that a background subagent may leave background Bash commands running after its turn ends |
| Codex | 6 threads by default in v1; 4 per session by default in v2 (including the main thread) | 1 level by default in v1 | No | Commands start a process group with setsid and the whole group is ended; SIGTERM on parent death works only on Linux and only for direct children; v2 unloads idle subagents, the only idle reclamation among the four |
| OpenCode | No concurrency counter found; subagents always run in the background and the call returns at once | 1 level by default (`experimental.subagent_depth`) | No | The command's process group is ended as a whole, SIGKILL 3 seconds after SIGTERM |
| Pi | The core deliberately has no subagents; the official example extension allows at most 8 tasks, 4 at a time | Not limited in the example | No | The bash tool ends the whole group; but the example extension kills only the child pi process itself, whose command process groups are detached, so a hard-killed child pi leaves them orphaned |

Conclusions:

- **Unbounded fan-out is no longer the mainstream state.** Every agent has some limit; Claude Code's concurrency limit was added only in July 2026, to keep a single message from fanning out into unbounded background agents, which shows it had been a real problem.
- **A count limit is not resource awareness.** The limit is a fixed constant, the same from an embedded board to a top-end workstation: 20 subagents may be conservative on a 128GB workstation and enough to bring down an 8GB laptop or dev board. Nor does the limit care what subagents do: 20 subagents each compiling or opening Chrome look the same to it as 20 that only read files.
- **Limits are per session, and sessions cannot see each other.** With several sessions, several terminal windows and different agents in use at once, nobody manages the machine's total.
- **The limit is a soft constraint enforced by the agent itself.** It can be raised, and it has exemptions and gaps (ultracode, resumed subagents not checked).
- **Process cleanup covers only the one process group the agent knows about.** Grandchildren of a hard-killed subagent, non-Linux platforms, and processes detached with setsid or nohup are all outside it.

This is why subactl exists. It does not compete with these count limits; it adds the two things they lack: a global ledger across sessions, sized to the machine's actual capacity and metered by measured use, and whole-group reclamation guaranteed by process membership.

Sources: [Claude Code subagent docs](https://code.claude.com/docs/en/sub-agents); [Codex config/mod.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/config/mod.rs), [Codex process\_group.rs](https://github.com/openai/codex/blob/main/codex-rs/utils/pty/src/process_group.rs), [Codex residency.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/agent/control/residency.rs); [OpenCode subagent.ts](https://github.com/sst/opencode/blob/dev/packages/core/src/tool/plugin/subagent.ts); [Pi README](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/README.md), [Pi subagent example](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/examples/extensions/subagent/index.ts).

v1 addresses six problems:

1. After a subagent ends abnormally, its descendant processes are left behind.
2. A single subagent uses too many resources.
3. The total use of subagents gets out of control and brings down the machine.
4. The planner cannot see what each subagent actually uses, and has nothing to decide on.
5. A subagent stays alive after finishing its task, or after nobody manages it any more, and keeps holding resources (idle leaks).
6. The planner can only hard-kill a child agent; it cannot let it save its progress and exit first.

Design principles:

- **Separate policy from mechanism.** How much to allocate, when to grow, whom to end: the planner decides; subactld only validates, executes and reports, and makes no policy decision of its own (the one exception is the safety net below).
- **Safety comes from static invariants, not from reaction speed.** A quota that is never oversold, plus a hard limit on every acgroup, keeps the machine safe when the planner is slow or offline.
- **Enforcement does not depend on the agent's cooperation.** The boundary must sit where the agent cannot cross it. Real agents may actively step out of a boundary imposed from outside; for a typical example see [anthropics/claude-code#60488](https://github.com/anthropics/claude-code/issues/60488).
- **Meter actual use.** Not VSZ (Virtual Set Size, all the virtual address space a process maps); processes are counted by acgroup membership, not by stitching a tree from PPIDs.
- **Every mechanism must answer one of the six problems above.** A mechanism that answers none does not go into v1.
- **State platform differences honestly.** No silent degradation; a guarantee that cannot be met goes into the non-goals, not into the table as hard.

Goals (v1):

1. Deliver the daemon `subactld` and the CLI `subactl` (including `subactl top` and `subactl trace`), written in Go. The only contract is `api/openapi.yaml`; v1 provides a single public Go client package, written by hand on the standard library, checked against the contract by CI. The whole project uses only the Go standard library and the official extension libraries golang.org/x/sys and golang.org/x/term, with no third-party dependencies (implementation: YAML is parsed by a strict-subset parser of our own, `yamlite`, and the experiments' Parquet files are written by a minimal hand-written writer). subactl supports only Linux and macOS: Linux (cgroup v2) is the main platform, macOS is supported at the monitor level. cgroup v1 is out of scope. On any other system, Windows included, the build fails at once with `undefined: subactl_supports_only_linux_and_macos` (implementation: every public package and command has an `unsupported.go` compiled only on other systems; `make lint` checks that every public package fails there, and only there, on Windows and FreeBSD).
2. Resource dimensions managed: hard memory limit, CPU, number of processes, wall time, output limit.
3. A quota ledger that never oversells, with envelopes of running subagents adjustable.
4. Whole-group reclamation when an acgroup ends, whether because its main process exited, its parent exited (including the lead's terminal being closed), or it was aborted from outside.
5. Maintain the agent tree: the lead and child agents are all created by subactl; subtree permissions, cascading reclamation, a graceful termination protocol, idle detection; control messages delivered both ways through subactl; leads isolated from each other in resources and in the control plane.
6. Observability: usage can be queried and subscribed to; Prometheus metrics and a per-lease timeline; a uniform termination code at every end; an audit log throughout.
7. Deliver the demo harness mini-agent: in Go, an independent module under `examples/mini-agent`, implementing its own agent loop, four tools and a line-mode TUI with no third-party libraries; with a scripted brain and a real one, run three comparison groups on the React 19 upgrade of radix-ui/primitives. The real brain speaks the Anthropic Messages API and works with Anthropic itself and with the Anthropic-compatible endpoints of DeepSeek and Z.AI (implementation).

Non-goals (v1):

- Overselling, and automatic pressure handling (applying pressure, pausing, choosing a victim on its own).
- Multi-host deployment and Kubernetes.
- Fixing memory leaks in the agent harness itself. subactld only limits their blast radius.
- File system and network sandboxing. Escapes in which an agent asks a third party over IPC to start a process for it are therefore outside v1's protection, for example `systemd-run --user`, a tmux server, the docker daemon, launchd: the new process is not a descendant of the agent and usually does not inherit its environment. Several agents in the same directory modifying the same file at once is also outside it (observed with a real model during implementation: three child agents edited the same shared file at once).
- Disk quotas. A lease without an output limit writes its output into its scratch directory without limit.
- Keeping leases running after subactld itself crashes. v1 is fail-closed; see "Core model".
- cgroup v1, pausing and resuming.
- LLM quota management. Running out of quota is handled as an external abort, with termination code 520 EXTERNAL\_ABORT.
- Windows. Originally planned for v1.1 with Job Objects; now out of scope: subactl supports only Linux and macOS.
- Isolating a single subagent within one process. A subagent must run as its own process.
- Third-party harnesses, and the subagents they start internally on their own.
- Other deferred mechanisms are in the "Reserved for v2" section at the end.

## Terms and cross-platform mapping (normative)

The core layer uses only the abstract terms defined in this section; platform mechanisms such as cgroup and kqueue may appear only in backends. Every backend must declare an enforcement level for each capability, and every platform difference must be exposed explicitly, never silently degraded.

**Terms**:

| Term | Definition |
| --- | --- |
| acgroup (agent control group) | The resource boundary of a group of processes. Descendant processes belong to it automatically; it is created and destroyed by the backend. The name echoes Linux's cgroup, but it is a cross-platform abstraction: implemented by cgroup v2 on Linux, and by process groups plus subactld's membership table on macOS; it is not the same thing as a cgroup |
| acgroup tree | acgroups nest by lease hierarchy, and a child acgroup's limit does not exceed its parent's. It is the only mapping of the agent hierarchy; OS parent-child process relations play no part in membership |
| lease | An agent node's registration and quota credential in subactld. Each lease has one acgroup, one main process and one termination code; its states are admitted → running → reaping → reaped |
| envelope | The per-dimension limits submitted when requesting a lease: memory, memory\_self, cpu, pids, wall, output |
| lead | The entry agent of an agent tree, i.e. a top-level lease with no parent, started by a person with `subactl run -it`. Not called root, to avoid confusion with the Unix root user |
| planner | Any node that creates child agents, the lead included. Responsible for semantic and planning decisions |
| holder | The process a lease's life depends on: for a nested lease, the main process of its parent lease; for a lead, the `subactl run -it` CLI process. When the holder dies, the leases under it are reclaimed in cascade |
| operator | A caller that belongs to no acgroup, i.e. a person or a tool a person runs; authorised by uid |
| membership | The single source of truth for which acgroup a process belongs to, provided by the backend. PPID and process group are only auxiliary. Membership is also the caller's identity when calling subactld |
| effective capacity | The total memory available to all top-level leases: min(configured cap, MemTotal − reserve line − external use). External use is the memory of processes not managed by subactld; the reserve line is the margin kept for the system, and available memory falling below it triggers the safety net. The reserve line defaults to max(MemTotal × 15%, 8GiB), but not more than half of MemTotal (implementation) |
| memory basis (mem\_basis) | How memory use is measured. The two platforms measure differently, and all usage data must carry its basis label: working\_set, phys\_footprint |
| hard memory limit (mem.hard) | Exceeding it ends the whole acgroup. Platforms reach this semantics in different ways; see the table below |
| whole-group termination (kill\_tree) | End every process in the acgroup and verify that zero remain |
| termination code | The single code every lease produces when it ends, giving the reason, in HTTP style: 2xx normal, 4xx over its own quota or ended deliberately, 5xx environmental; see "Termination codes and audit" |
| harness adapter | A small piece of code embedded in the agent harness that injects the inbox's control messages into the conversation at tool calls and turn changes, and reports progress beacons. An agent without an adapter can be communicated with only by signals |
| inbox | The control message queue subactld keeps for each lease; it accepts messages only from subactld itself and from ancestor planners |
| progress beacon | The state (working / awaiting\_input / done), timestamp and tool name the adapter reports at every hook, used for idle detection and traces. No beacon does not mean the process is dead |
| fail-closed | When subactld crashes or restarts, it does not take over running leases; at start it ends every existing acgroup as a whole (500). The kernel's hard limits stay in force while it is down |
| enforcement level | Four levels: hard is guaranteed by the kernel; soft is best effort, possibly with a window of time; monitor\_only is detection after the fact and then action, with a delay of seconds; unsupported means not supported |

**Implementation on each platform** (formatted as "mechanism / enforcement level"):

| Capability | Linux (cgroup v2) | macOS native | macOS + Linux VM |
| --- | --- | --- | --- |
| acgroup | cgroup directory; the limits are set on the lease directory and the processes placed in its leaf `_self/` (see Core model) | process group + subactld's membership table | a cgroup inside the VM |
| membership | children inherit it automatically / hard | at every sample the process table is scanned and processes are registered by parent chain and process group; the member table is persisted for fail-closed cleanup after a restart / soft (NOTE\_FORK carries no child pid, NOTE\_TRACK is unavailable) | hard |
| memory basis | working\_set: memory.current − inactive\_file | sum of phys\_footprint | working\_set |
| hard memory limit | memory.max + memory.oom.group / hard | checked every 200 milliseconds (implementation); over the limit, the whole group is ended, recorded as 413 / monitor\_only | hard |
| CPU | cpu.max / hard | monitor\_only | hard |
| number of processes | pids.max / hard | monitor\_only | hard |
| whole-group termination | cgroup.kill / hard | kill each process in the membership table and verify / soft | hard |
| parent death detection | pidfd / hard | kqueue NOTE\_EXIT / soft | heartbeat across the VM / soft |
| escape prevention (leaving through cgroupfs) | cgroup namespace + the kernel's migration reachability check + no\_new\_privs (implementation) / hard | unsupported. Implementation: not even detection after the fact by an environment marker is possible: macOS's `kern.procargs2` gives an ordinary user only the arguments of other processes, not their environment (on macOS the daemon runs as an ordinary user) | hard |
| caller identity | SO\_PEERCRED → its acgroup; on 6.5+ also SO\_PEERPIDFD, confirming after the membership lookup that the caller is still alive / hard | LOCAL\_PEERPID → membership table / soft | hard |
| host available memory | MemAvailable from `/proc/meminfo`; inside a capped outer cgroup, that cgroup's memory.max and usage can be used instead (implementation) | `kern.memorystatus_level`, the share the kernel's memory-pressure mechanism considers available (implementation) | meminfo inside the VM |
| delivery | v1 | v1 (monitor, implementation) | v2 |

**Rules**:

1. The core sections (core model, admission, API, termination codes, division of work with the planner) may refer only to the terms of this section. Platform mechanism names appear only in backends and in this section.
2. A backend reports its capability matrix at start, exposed through `GET /v1/host`. The `require` field of an acquire is matched against enforcement levels; if it cannot be met, the answer is `enforcement_unavailable`.
3. All usage data carries `mem_basis`. Values on different bases cannot be added or compared directly.
4. When a capability is missing or at a lower level: if the hard memory limit is only monitor\_only (macOS native), never overselling can be guaranteed only approximately, so the reserve line has to be raised accordingly and the safety net fills the gap; if kill\_tree is only soft, the membership table must be rescanned after every termination to confirm nothing is left.
5. "Escape prevention / hard" in the table covers only leaving through cgroupfs. Escapes by asking a third party to start a process are out of scope; see the non-goals.

## Core model: invariants, leases and acgroups

Every subagent runs in its own acgroup, obtained by requesting a lease. The three invariants below are all that subactld promises; every other mechanism exists to implement them.

**Three invariants**:

1. **I-A Membership and lifetime**: every process subactld starts (with its descendants) belongs to exactly one acgroup. When an acgroup ends, every process in it ends, and zero remaining processes is verified.
2. **I-B Never oversell**: the sum of the hard memory limits of all live top-level acgroups does not exceed the effective capacity; a nested acgroup's limit is contained in its parent's and not counted again. Effective capacity = min(configured cap, MemTotal − reserve − external use), where external use = MemTotal − MemAvailable − Σ acgroups' current working\_set, i.e. what processes not managed by subactld use; subactld recomputes it every second. MemAvailable cannot be used directly: it already subtracts the acgroups' own use, so the more subagents used, the smaller the capacity would become, a loop. CPU and process count may be overcommitted, because overcommitting them only makes processes slower; it does not bring the machine down.
3. **I-C Observable**: every acgroup's use, limits and state can be queried at any time. Every state change produces an event and is written to the audit log.

Constraints on subactld's own implementation:

- A reservation is released on every termination path; none leaks.
- **fail-closed**: when subactld crashes or restarts, it does not try to take over running leases. At start it ends every acgroup the backend reports, as a whole. While it is down the kernel's hard limits stay in force and the machine stays safe; only cascading reclamation is delayed until the restart. So the ledger need live only in memory (the audit log is persisted separately), and the problem of "after a restart, no longer the main process's parent and unable to get its exit status" never arises. Restarting without interruption is a v2 option; see "Reserved for v2".
  - Implementation: a new daemon knows nothing of the previous daemon's leases, so those acgroups are only ended as a whole and logged; no termination record is produced. When the daemon is stopped normally (SIGTERM), it first reaps every lease with 500 SUBACTL\_INTERNAL and then exits.
  - Implementation: under systemd, the lease subtree lives in the service's own cgroup (see the next item); when the daemon dies, systemd at once cleans up the whole service cgroup, and the leases end with the processes in them, sooner than at a restart. Measured on a VM: restarted within a second after `kill -9`, nothing left.
- subactld sets its own oom\_score\_adj = −1000 and is supervised by systemd with Restart=always. Implementation: the repository provides `deploy/subactld.service` (`Restart=always`, `OOMScoreAdjust=-1000`, `Delegate=yes`). The daemon creates `leases/` under the cgroup delegated to it for all leases, and first moves itself into a leaf `_daemon/`: cgroup v2 does not let a cgroup that holds processes enable controllers for its children.

**Leases and envelopes**: the planner requests a lease for each subagent and submits an envelope with it, the limit in each dimension. Once subactld approves, it creates the acgroup and starts the subagent in it.

| Dimension | Field | Counts toward never overselling | What happens over the limit |
| --- | --- | --- | --- |
| hard memory limit | memory | yes (top level only) | whole group ended, code 413 |
| memory kept for itself | memory\_self | no (contained in memory) | when nested, the part reserved for the parent's own processes, which child leases cannot take |
| CPU | cpu | no (may be overcommitted) | processes slow down; nothing is ended |
| number of processes | pids | no | no new process can be created; guards against fork bombs |
| run time | wall | no | whole group ended, code 408. Checked every second, at most 1 second over |
| output | output | no | whole group ended, code 414. Implementation: the agent's stdout and stderr are pipes, read by subactld, written to files in the scratch directory and counted; over the limit subactld stops reading, the writer blocks on the pipe, the file is exactly the limit in size, and the group is ended at the next check (within 1 second). This output is not counted in the lease's memory. When a lead writes to a terminal, the bytes relayed are counted |

- **subactld starts the processes**: the planner does not fork subagents itself; subactld starts each subagent in its acgroup through a pre-exec shim. subactld is the main process's parent and gets its exit status by waiting on it directly. OS parent-child relations play no part in membership.
- **How the main process's exit is attributed** (implementation): exit status 0 is 200; a non-zero exit or a signal is 520, with the exit status and signal in the evidence. For a signal, memory is checked first: if the lease's or an ancestor's acgroup hit its own hard limit (`oom` in `memory.events.local`, which is counted only at the level whose limit was hit), the level that hit its limit is 413 and the subtree it takes along is 502; if none did and the capacity guard was hit (see the safety net), 503. `oom_group_kill` cannot be used to decide: measured, it is counted at the level killed as a group, even when the limit hit was the level above.
- **Nesting**: when a subagent starts a subagent, the child acgroup nests inside the parent's, and the child's use counts toward the parent's quota. cgroup v2 does not let a cgroup that has enabled controllers for its children hold processes itself (the no-internal-processes rule), so each lease is a directory: the limits are set on `<lease>/`, the lease's own processes are in the leaf `<lease>/_self/`, and child leases in `<lease>/<child>/`. When the parent is over its limit the kernel OOMs at the parent, and memory.oom.group ends the whole subtree with it (413, the subtree in cascade). Implementation: `memory.oom.group` is set only on each lease's `_self/`, not on the lease directory. On a limit the kernel ends "the highest cgroup with oom.group set, going up from the victim process, below the level whose limit was hit"; set on lease directories, a hit of the capacity guard (on the parent cgroup of all leases) would end the topmost lead with all its workers (that is how the first run of group C in round 5's H4 experiment lost its whole tree at the sixth minute). Set on `_self/`, the kernel ends one lease's own processes at a time; that lease's main process then exits, and subactld looks up through the ancestors for the level whose own hard limit was hit and ends it with its subtree (413, the subtree 502); with no such ancestor and the guard hit, only that one lease is 503.
- **The parent is the holder**: a lease's holder is its parent node. A top-level lease's parent is the subactl run -it CLI process that started it (the lead's holder); a nested lease's parent is the parent lease's main process. When the parent dies (detected by pidfd), all leases under it are reclaimed in cascade at once, code 502. On one machine pidfd is exact, so v1 has no TTL heartbeat and does not support a parent taking over again after a restart. Implementation: the holder is the operator process that created the lead, with or without `-it`; `subactl run` exits on a hangup and the daemon reaps with 502 (an earlier version killed first, which recorded a closed terminal as 499). macOS uses kqueue's NOTE\_EXIT.
- **The main process's exit means reclamation**: as soon as a lease's main process exits, for whatever reason, the lease goes into reaping, and not one descendant is left.

**State machine**: admitted → running → reaping → reaped, four states. v1 has no queued state, no automatic degraded state and no paused state. Every transition produces an event. The reason for ending is expressed only by the termination code (see "Termination codes and audit"); there is no separate exit\_reason enum.

## Admission and quotas

subactld gives only two answers to a quota request: approved, or refused with a reason. It does not queue and does not choose for the planner; whether to wait, try another way, or release another subagent first is the planner's decision.

**Admission checks** (at acquire):

1. Top-level lease: the sum of allocated top-level hard memory limits plus this request does not exceed the effective capacity.
2. Nested lease: `Σ siblings.memory + this request + parent.memory_self ≤ parent.memory`.
3. The backend can reach the enforcement level declared in the request's `require`.
4. The machine is not currently in the capacity-shrunk state (see below).

A refusal's reason is one of: `mem_budget`, `parent_budget`, `enforcement_unavailable`, `parent_not_running`, `capacity_shrunk`; resizing also has `below_usage` and `below_allocated` (implementation, see below). Every request may carry `dry_run`, which only checks and reserves nothing.

**Resizing a running lease** (resize):

- Raising: may use only unallocated room (when nested, the parent's room). Without enough room it is refused, reason `mem_budget` or `parent_budget`; the planner must first end or shrink other acgroups.
- Lowering: the new value may not be below "current use + margin". The margin defaults to the larger of 10% of current use and 64MB. Below that it is refused, reason `below_usage`, so that the resize itself does not kill the process.
- Lowering below `memory_self` plus the sum of live child leases' hard limits is likewise refused, reason `below_allocated` (implementation).
- A node cannot resize itself, 403 `self_resize_forbidden` (implementation); it can only ask its parent through resize-request.
- How it takes effect: on Linux, memory.max is changed directly; on macOS, the monitor's threshold is updated.

**NEAR\_LIMIT warning**: when an acgroup's memory use reaches 90% (configurable) of its own hard limit, subactld pushes a `NEAR_LIMIT` event with the current use and growth rate. The planner can raise the envelope, let it hit the limit and end, or end it deliberately. subactld itself does nothing, unless the planner declared a `grow_to` rule beforehand (see "Declarative policies"). Implementation: one crossing is pushed once, and re-armed only when use falls back below 85%; after growing by `grow_to`, it is re-armed at the new limit. The event is also put into the lease's own inbox as a control message.

**Capacity shrinking and the safety net**: the effective capacity changes with external use, and two thresholds handle it:

- **Capacity shrunk (refuse only, end nothing)**: external use grows until the sum of allocated top-level hard limits exceeds the effective capacity, i.e. I-B temporarily does not hold. subactld pushes `CAPACITY_SHRUNK`, and from then on new acquires and raises are refused (`capacity_shrunk`) until it recovers. Allocated is not used, so no acgroup is ended at this point.
- **Safety net (subactld's only autonomous action)**: the host's MemAvailable actually falls below the reserve line, three samples in a row (hysteresis, against flapping). subactld first pushes `CAPACITY_SHRUNK`, giving the planners a short window (5 seconds by default) to release memory themselves; if there is still not enough when the window ends, it ends one acgroup at a time, waits for it to be reaped and its memory freed, and then re-evaluates, until available memory is back above the reserve line. Termination code 503 SAFETY\_NET. Implementation: after each termination the hysteresis count is reset, so the next needs at least three new readings below the line in a row. The two kinds of `CAPACITY_SHRUNK` are told apart by `reason`: `allocated_exceeds_capacity` (refuse only) and `below_reserve` (the safety net follows).
  - **Capacity guard** (implementation): the sampling safety net needs at least 3 seconds before it acts, too slow for a faster burst. In an experiment (group L, run 5), while external interference held 3.6GiB, several workers admitted earlier started hundreds of processes at once and grew from 0.3GiB to 8.3GiB in two seconds; the whole blast chamber hit its own limit and the kernel emptied it. So the cgroup2 backend sets `memory.max` on the parent cgroup of all leases, updated every second to MemTotal − external use − half the reserve line: half a reserve line below where the safety net triggers, so when there is time the safety net still warns first and then chooses; only a burst faster than the sampling is ended by the kernel, which picks one lease among them to end as a group, instead of letting the host OOM. Neither the parent cgroup nor the lease directories have `memory.oom.group` set (only `_self/` has it; see nesting), so one lease is ended at a time, never the lead above it. A lease ended this way is 503, with `detected_by` set to `kernel` in the evidence. In the integration test the same burst is ended within 63–98 milliseconds and the host's limit is not reached; with the guard off, the host's limit is hit.
- **Order of termination**: if the planner submitted a `kill_order` hint beforehand, it is followed; otherwise leaves are ended first, and among leaves the largest current use first, to free the memory with the fewest terminations. Implementation: first the lead tree with the largest current use; within the tree, if a lease carries `kill_order`, the smallest value; otherwise the leaf with the largest use; equal use is ordered by id for reproducibility.
- A 5-second window is mostly too short for an LLM planner to respond; in practice it matters only to planners written as code and to a `kill_order` declared beforehand. The safety net is a fixed rule and involves no judgment. In normal operation it never fires; when it fires, the reserve line or the external load needs to be re-evaluated.

## Metering and observability

Everything the planner needs comes from measurements subactld reads from the backend; subagents do not report their own use. Every usage value carries a `mem_basis` label, and values on different bases can be neither added nor compared directly.

| Metric | Linux | macOS | Sampling |
| --- | --- | --- | --- |
| acgroup memory | `working_set`: on v2 `memory.current` − `inactive_file` | sum of `phys_footprint` | every second |
| limit hit / OOM | `memory.events` | process exit status and signal | event-driven |
| CPU | `cpu.stat` | `proc_pid_rusage` | every second |
| number of processes | `pids.current` | count in the membership table | every second |
| host available memory | `MemAvailable`, or the outer cgroup's limit and use (implementation) | `kern.memorystatus_level` (implementation) | every second |

- **Fields exposed per acgroup**: use, limits, peak, growth rate, CPU, number of processes, state, age, and `mem_basis`.
- **Fields exposed for the host**: effective capacity, allocated amount, external use (processes not managed by subactld), reserve line, and the backend's capability matrix.
- **Events**: state changes, `NEAR_LIMIT`, `CAPACITY_SHRUNK`, termination events with their code, `escape`.
- **Accuracy**: within 10% of the kernel's accounting. Implementation: on Linux −8.5% to −9.3% from the sum of the processes' VmRSS (working\_set does not count pages shared between processes twice); on macOS phys\_footprint is within about 5% of the system's `footprint` tool. macOS's effective capacity is computed from the kernel's memory-pressure percentage, which moves in whole percent and after memory compression, so it moves less than the memory other programs actually allocate.

**Suggested envelopes** (provided by the CLI and the public Go client; the result is deterministic): `subactl suggest(kind)` reads the historical peaks of the same kind from the audit log and returns p95 × 1.2 as the suggested initial envelope, with the sample count. With too few samples it returns a default, marked as such. The planner can take it or override it. This is the numeric part of "learnable"; the part that needs judgment belongs to the planner, see "Division of work between the planner and subactl".

Implementation: only leases that ended normally (2xx) are counted. A lease ended by its hard memory limit has its limit as its peak; one ended early has only its progress so far. Counting them makes the suggestion chase the limit: in an experiment, group C pushed the workers' suggestion from 2GiB to 9.6GiB within one round, until no worker fit into the lead's envelope. With fewer than 5 samples it returns the default 2GiB; p95 uses the nearest-rank method; at most 1000 peaks are kept per kind; only samples on the backend's mem\_basis are used; the daemon rebuilds them from the audit log at start. The CLI's `subactl suggest KIND` prints only the number of bytes to stdout, for `--memory "$(subactl suggest worker)"`.

**Metrics and tracing**:

- **Prometheus `GET /metrics`**: effective capacity, allocated amount, external use, counts per termination code, safety-net terminations, admission refusals by reason, distribution of use per kind (histogram). Written by hand in the Prometheus text format, without client\_golang. Labels are only kind, depth and code, never a lease or lead id: both are created and destroyed all the time and would make the series count explode. Per-lease data goes into the timeline below.
- **Per-lease timeline**: for every live lease subactld keeps a ring buffer at 1-second resolution in memory (10 minutes by default), downsampled to 5 seconds and persisted with the audit log as JSONL. `subactl trace <lease>` rebuilds the full timeline: spawn → memory and CPU curves → events → injected control messages → tool-call beacons → termination code and evidence. Implementation: the 1-second timelines of the most recent 1024 ended leases stay in memory, older ones are rebuilt from the audit log at 5-second resolution; at most 500 events are kept per lease; `--tree` returns the whole subtree (ended leases included, recovered from memory or the audit log) as `children`. A trace also carries `spikes`: intervals in which memory grew by 64MiB or more, attributed to the beacon of the tool call running then (implementation).
- **Tool-call overlay**: progress beacons carry the tool name and sequence number (not its arguments, against injection and leaks), and the trace lays them over the usage curve, answering directly "which bash call grew memory by 4GB". This is the single most valuable aid to analysing agent behaviour.
- **`subactl top`**: an htop-like TUI showing each lead's agent tree, each node's use against its envelope as a bar, its state (working / awaiting\_input / done) and recent events. It is an ordinary client of the API, independent of any harness. Implementation: at 90% the bar switches to another character; a node without a beacon for long is marked with how long it has been silent; `--once` prints one frame and exits. For this the lease object gained an `activity` field (the last beacon).
- **Self-reported data**: subactld cannot measure LLM token use; only the adapter can report it, recorded as `self_reported` and kept apart from measured data.
- **Observability failures do not affect enforcement**: sampling, export and persistence run in their own goroutines with bounded buffers; under back-pressure data is dropped and counted, and enforcement is never blocked.
- OpenTelemetry export (the lease tree maps directly onto a span tree) is for v1.1, as an optional exporter.

## Enforcement backend and escape prevention

A backend does only five things: create an isolation unit, place a process in it before exec, read samples, kill the whole group, and destroy the unit; plus two queries: which acgroup a pid belongs to (for identity), and which acgroups exist (for fail-closed cleanup at start). A backend must report its enforcement level for every dimension, and the planner may require a minimum level.

```
type Backend interface {
    Capabilities() Caps // EnforcementLevel per capability: hard|soft|monitor_only|unsupported
    Create(ctx context.Context, lease LeaseID, parent ACGroupRef, env Envelope) (ACGroup, error)
    Spawn(ctx context.Context, g ACGroup, cmd Cmd) (Proc, error) // pre-exec placement; Wait on Proc gives the exit status
    Sample(ctx context.Context, g ACGroup) (Usage, error)        // use carries mem_basis
    SetLimits(ctx context.Context, g ACGroup, env Envelope) error
    KillTree(ctx context.Context, g ACGroup) (KillReport, error)
    Destroy(ctx context.Context, g ACGroup) error
    Identify(ctx context.Context, pid int) (ACGroupRef, error)   // caller identity: pid → its acgroup
    List(ctx context.Context) ([]ACGroupRef, error)              // fail-closed cleanup at start
}
```

Implementation: the interface also has `Name()` and `MemBasis()`. Extra capabilities a backend can offer are declared by optional interfaces, which the daemon enables when present: `HolderWatcher` (exact detection of holder death: Linux pidfd, macOS kqueue), `EscapeScanner` (escape scanning), `OutputMeter` (output byte counts), `Scratcher` (scratch directories), `TTYProc` (the lead's pty). A backend without `HolderWatcher` does not get a weaker substitute.

| backend | platform | memory | cpu / pids | whole-group kill | in v1 |
| --- | --- | --- | --- | --- | --- |
| cgroup2 | Linux unified, subactld runs as root | hard: memory.max + memory.oom.group | hard | cgroup.kill | yes |
| fake | any | simulated in memory | simulated in memory | simulated | yes (for testing the ledger and admission) |
| darwin-monitor | macOS | monitor\_only: samples phys\_footprint, kills over the limit | monitor\_only | kill each process in the membership table, then verify | yes (implementation): `subactld --backend darwin`, the default on macOS, can run as an ordinary user |
| systemd | Linux non-root, user manager + Delegate=yes | as cgroup2 | hard | as cgroup2 | v2: no cgroup namespace and no separate uid, so escape prevention falls to soft |
| cgroup1 | Linux hybrid / legacy | hard (max), no oom.group, subactld has to finish the kill | hard | freeze, then walk | v2 |
| linux-vm | macOS (Lima / OrbStack / Apple container) | delegated to cgroup2 inside the VM | hard | as cgroup2 | v2 |

**Deployment requirements**: v1's cgroup2 backend needs Linux ≥ 5.14 (cgroup.kill since 5.14, clone3's CLONE\_INTO\_CGROUP since 5.7), and on Linux 6.5+ also uses SO\_PEERPIDFD. Implementation: the memory and pids controllers must be delegatable, or the daemon fails to start; if the cpu controller cannot be delegated, CPU is honestly reported as unsupported. The socket's group is set by `--socket-group` (default `subactl`). subactld runs as root (a system service), because creating a cgroup namespace needs CAP\_SYS\_ADMIN and switching an agent's uid needs setuid capability. **Agents themselves never run as root**: by default a lead uses the uid of the operator who ran `subactl run -it` (from SO\_PEERCRED), and child agents inherit their parent's uid; with per-lead uids enabled, a separate uid from a pool is used instead. If the operator is root, a non-root uid must be given explicitly, or the start is refused. The reason: an agent running as root can setns back into the initial cgroup namespace and rewrite cgroup files directly, and all isolation fails. For non-root mode see the systemd row above.

**Pre-exec placement**: subactld does not exec the target command directly. It uses Go's `SysProcAttr.UseCgroupFD` (underneath, `clone3(CLONE_INTO_CGROUP)`, which needs Linux 5.7+ and Go 1.20+) to create a shim directly in `<lease>/_self/`. The shim is a re-exec entry of subactld itself (`subactld __exec`), not a separate binary. The shim does the following, in order, and then execs the agent, so that the agent is within its limits before its first instruction:

1. Enter a new cgroup namespace (unshare), whose root is `<lease>/_self/`.
2. Set oom\_score\_adj and, where configured, switch to the agent's uid (see the deployment requirements). Writing oom\_score\_adj with privilege also sets a floor, which the agent cannot lower without CAP\_SYS\_RESOURCE. The floor must be above −1000: a process with oom\_score\_adj −1000 is not killed by memory.oom.group. Beyond that, this affects only the global OOM; an OOM inside an acgroup is handled by memory.oom.group. The implementation uses 200.
3. Set the environment variables `SUBACTL_LEASE_ID` and `SUBACTL_SOCK`; redirect stdout and stderr to files in the lease's scratch directory. Implementation: `SUBACTL_SCRATCH` is also set; stdout and stderr are pipes to subactld, which writes the files (see the envelope table).
4. Close extra file descriptors.
5. (implementation) Keep all of the agent user's supplementary groups (among them the group that may reach the daemon's socket; without it an agent cannot start children), then set `no_new_privs`: from then on no process in the lease can gain privileges through a setuid program (such as sudo), and descendants inherit this irrevocably. This was found on a VM with a real model: after a permission error, the model switched to sudo on its own, and on a machine with passwordless sudo that would have made the agent root.

**Escape prevention**:

Threat model and coverage:

- The agent writes itself into another cgroup (a real case: [anthropics/claude-code#60488](https://github.com/anthropics/claude-code/issues/60488)). Blocked by the cgroup namespace, hard: cgroup v2 requires both the source and the destination cgroup of a migration to be visible from the writer's cgroup namespace, or returns ENOENT. The agent running as an ordinary uid without write permission on cgroup directories is a second barrier.
- double-fork plus setsid. Not an escape on Linux: cgroup membership is inherited across fork, regardless of process group. It matters only on native macOS, covered by periodically scanning the membership table, soft.
- Lowering its own oom\_score\_adj so that the kernel kills someone else. Blocked by the floor the shim sets (provided the agent has no CAP\_SYS\_RESOURCE), and it affects only the global OOM anyway.
- (implementation) Gaining root through sudo or another setuid program, then leaving the cgroup namespace. Blocked by no\_new\_privs, hard. macOS has no equivalent, consistent with declaring "escape prevention: unsupported" there.
- Asking a third party over IPC to start a process (`systemd-run --user`, a tmux server, docker, WMI, scheduled tasks). The new process is not a descendant of the agent and usually does not inherit its environment. Out of v1's scope; it needs a sandbox, see the non-goals.

Countermeasures:

1. **cgroup namespace**: the cgroup root an agent sees is its own `_self/`. Even if the host's cgroupfs mount is still visible, the kernel's containment check refuses a migration outside the namespace root.
2. **Permission isolation**: cgroup directories are owned by root. Optionally the agent runs as a separate uid, reducing what it can see of other processes (reading /proc, sending signals).
3. **Escape scanning (a soft heuristic)**: every 5 seconds subactld scans for processes outside the lease tree whose environment carries `SUBACTL_LEASE_ID` (reading `/proc/<pid>/environ`, which needs root). `env -i` gets around it, so it serves only to find unintended leaks and is not a security boundary. A finding is recorded as an escape event and ended, code 451.

The single source of truth for membership is cgroup membership; PPID and process group are only auxiliary.

**Minimum level**: an acquire can declare `require: {memory: hard}`. If the current backend cannot reach it, the answer is `enforcement_unavailable`, never a silent downgrade.

## Daemon API and module boundaries

The system has two process boundaries: `subactld` (the Go daemon) and `subactl` (the Go CLI). Every caller, whether the CLI an LLM runs from bash, the client a code planner uses, or `subactl top`, depends only on one contract, `api/openapi.yaml`, and subactld does not know who the caller is. It has to be a separate daemon for two reasons: some process must still be there to reclaim after any agent or orchestrator crashes, and several leads side by side need one global ledger.

\[embedded content: subactl architecture · two kinds of callers, subactld's 7 modules, the acgroup trees of several leads\]

A planner sends requests and subscribes to events through the CLI or the public Go client. subactld's internal modules only keep accounts, validate, sample and execute; subagents exist only in the acgroup tree, and nested acgroups count toward their parent's quota.

```
subactl/                         # module: github.com/zydo/subactl
  api/                           # wire types (Go) and openapi.yaml (the only contract, embedded in the binary)
  client/                        # the public Go client, hand-written on net/http, tested against openapi.yaml
  yamlite/                       # a strict YAML subset parser; public because mini-agent also reads scenario.yaml
  cmd/subactld/                  # the daemon (with the __exec entry, the pre-exec shim)
  cmd/subactl/                   # the CLI, commands below
  internal/ledger/               # the in-memory quota ledger and lease tree
  internal/daemon/               # lease lifecycle, the HTTP layer, observation and control plane (inbox, beacons, idle), fail-closed cleanup at start
  internal/capacity/             # effective capacity, host readings, the safety net's hysteresis and choice rule
  internal/telemetry/            # event bus, ring buffers
  internal/metrics/              # /metrics in the Prometheus text format
  internal/audit/                # the JSONL audit log, suggest()
  internal/pty/                  # pty allocation and the attach frame format
  internal/backend/              # the interface and fake; cgroup2/, darwinmon/
  internal/contract/             # reads openapi.yaml for the conformance tests
  internal/lint/                 # dependency rules (run as tests)
  deploy/subactld.service        # systemd unit
  skills/subactl/SKILL.md        # the skill that teaches an LLM to use subactl, independent of any harness
  examples/mini-agent/           # module: github.com/zydo/subactl/examples/mini-agent (its own go.mod)
    cmd/mini-agent/
    cmd/bench/                   # the comparison experiments and reports (run / report / extract / hog)
    agent/                       # agent loop, steer queue, hooks, adapter, run directory
    llm/                         # the brain interface; scripted and Messages API implementations
    tools/                       # read, write, edit, bash
    tui/                         # line-mode output
    scenarios/                   # scripts, scenario.yaml, the toy and radix tasks' scripts
    bench/                       # blast chamber, metrics, reports, Parquet writer; chamber/ holds the Docker runner
  go.work                        # joins the two modules during development
```

(Implementation: the planned persistence in internal/telemetry did not appear as such and was folded into audit; internal/backend/jobobject left the scope with Windows.)

Dependencies go one way only: `examples/mini-agent → client → api ← subactld` (implementation: mini-agent also depends on the public `yamlite`; a test in `internal/lint` rejects any import that breaks these rules). mini-agent is a separate Go module, so the toolchain guarantees it cannot import subactl's `internal` packages and can use only the public `client` package and the CLI; if it needs something it can get only from inside, the public contract has a gap. The two modules live in one repository, joined by `go.work` during development, so one commit can change both; the repository is to be split once mini-agent has users of its own. The whole project depends only on the Go standard library and the official extension libraries `golang.org/x/sys` and `golang.org/x/term`, with no third-party libraries. The transport is HTTP/JSON over a Unix socket (mode 0660, group subactl; an agent running as its own uid must be in that group).

**Identity**: no tokens. subactld takes the peer pid from the connection and asks the backend which acgroup it belongs to; that acgroup is the caller's node identity. The peer pid comes from SO\_PEERCRED on Linux, SO\_PEERPIDFD on 6.5+ to rule out pid reuse races, and LOCAL\_PEERPID on macOS. An agent's tool subprocesses are in the same acgroup as the agent and have the same identity; processes in a child lease have the child node's identity. Why no tokens: a token in an environment variable can be read by any process of the same uid from `/proc/<pid>/environ`, while cgroup membership on v2 cannot be forged. A process in no acgroup is an operator (a person), authorised by uid; see "Leads and isolation between leads". Implementation: identity is established once per connection; on Linux 6.5+ SO\_PEERPIDFD gives the peer's pidfd, and after the membership lookup the peer is confirmed still alive, which rules out the pid having been reused by another process.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | /v1/host | effective capacity, allocated amount, external use, reserve line, capability matrix |
| GET | /v1/leases/{id}/available | the remaining budget of this node's subtree and suggested envelopes; `?kind=` names kinds to include (implementation) |
| POST | /v1/leases | request a lease: envelope, parent, require, tty, and optionally kill\_order and declarative policies. Without a parent it creates a lead, and the requesting operator process becomes the holder. 201 means approved, 409 refused with a reason |
| POST | /v1/leases/{id}/spawn | start a process in the acgroup |
| GET | /v1/leases/{id}/attach | the lead's pty relay (upgraded to a two-way stream), used by subactl run -it. Implementation: `Upgrade: subactl-pty`; the client sends typed, length-prefixed frames (input, window size), and the daemon sends back raw terminal output; output before the attach is buffered up to 64KB and replayed at the first attach; only one client at a time, and only the operator who owns the lead may connect |
| PATCH | /v1/leases/{id} | resize, under the rules in "Admission and quotas" |
| POST | /v1/leases/{id}/resize-request | a child asks its parent for more, delivered to the parent as an event |
| POST | /v1/leases/{id}/terminate | graceful termination (notify → wait → SIGTERM → kill\_tree), with reason, grace, code (released is 204, killed 499), recursive |
| POST | /v1/leases/{id}/kill | end the whole group at once, 499. Optional body `{"dry_run": true}` (implementation) |
| POST | /v1/leases/{id}/notify | an ancestor planner delivers a control message to a descendant through subactld |
| POST | /v1/leases/{id}/progress | the adapter reports a progress beacon and state (working / awaiting\_input / done) |
| POST | /v1/leases/{id}/wait-children | wait for the whole subtree to end, returning each one's termination code. Implementation: optional `self` (wait for the lease itself too; used by `spawn --wait` and `run`) and `timeout_seconds` |
| GET | /v1/leases/{id}, /v1/leases?lead= | the state and use of one lease or a whole lease tree |
| GET | /v1/leases/{id}/trace | the lease's timeline: usage series, events, injected control messages, tool-call beacons, termination code |
| GET | /v1/leases/{id}/inbox | this node's inbox (SSE), for the harness adapter to subscribe to |
| GET | /v1/events | SSE event stream, scoped to the caller's subtree or the leads an operator may see |
| GET | /metrics | Prometheus metrics |

Every `{id}` must be in the caller's own subtree (itself included) or belong to a lead the operator may act on; otherwise 403 `out_of_subtree`. The rules are in "Division of work between the planner and subactl". Implementation: `{id}` may be written `self` for the caller's own lease; sizes are always transmitted as integers in bytes; an error body is `{error, reason, message, field, expected, example}`, and a validation failure names the field, the expected value and an example; an unknown field rejects the whole request.

```bash
# A person starts a lead from their own shell (bash, zsh or fish)
subactl run -it --memory 24G -- mini-agent
# More common: just run mini-agent; finding itself outside any lease, it execs the command above
mini-agent

# The lead, or any planner, starts a child agent from its bash tool and waits for the result
subactl available
subactl spawn --wait --kind worker --memory "$(subactl suggest worker)" --wall 30m \
  -- mini-agent --task tasks/react-dialog.md
```

- **Self-wrapping**: at start mini-agent checks `SUBACTL_LEASE_ID`. Without it, it execs `subactl run -it -- mini-agent …` and becomes a lead; with it, it execs `subactl spawn --wait -- mini-agent …` and becomes a child of the current node. Even if the LLM writes `mini-agent --task X &` in bash directly, the process is enrolled in the agent tree and cannot go around subactl. `--wait` blocks until the child ends and returns its termination code and result path, the same semantics as Pi's synchronous self-call from bash.
  - Implementation: a lead adds `-it` only when both stdin and stdout are terminals. A child does not exec but keeps a waiting process running `subactl spawn --wait`, for two reasons: when the request is refused with `parent_budget`, `mem_budget` or `capacity_shrunk`, it retries every 2–4 seconds (in scripted scenarios, until the script ends); and the exit status reports whether the task succeeded (from the result the child wrote, not from the lease's termination code), so that `test || mini-agent --task fix` works. A real model's tasks are free text, and its children's kind is always recorded as `worker`.
- **The lead's pty**: a process started by the daemon has no terminal of the user's, so subactld allocates a pty for the lead's TUI; `subactl run -it` switches the user's terminal to raw mode, relays input and output, and forwards window size changes (like `docker run -it`; the pty is allocated with golang.org/x/sys, without third-party libraries). Child agents are headless, get no pty, and write their output to files in the scratch directory. The CLI cannot move itself into a cgroup and exec: it has no privilege, so it can neither migrate cgroups nor create a cgroup namespace. Implementation: done on both Linux and macOS; `subactl run` without `-it` also forwards the lead's output to its own stdout and stderr as it is written (the output is in scratch, deleted when the lease ends; otherwise the user would see nothing), and writes the termination record to stderr.
- **Shell-independent**: subactl is an ordinary executable that needs no shell functions, aliases or sourcing, so any shell can call it; completion scripts are generated for bash, zsh and fish (implementation: `subactl completion bash|zsh|fish`). The agent's tool has fixed bash semantics (falling back to `/bin/sh` without bash), configurable but not following the user's `$SHELL`: LLM-written commands mostly assume bash, and zsh's differences in word splitting, errors on unmatched globs and array indexing would break them; a fixed shell also keeps experiments reproducible.
- `subactl run --memory 8G -- <third-party agent command>` (without `-it`): manages an unmodified third-party agent as a whole, as a degenerate leaf node. It has no adapter and can be communicated with only by signals; it gets only the hard limits, wall time and whole-group reclamation as a backstop. Implementation: kept. On a VM, programs that leak memory, hang, flood output and leave background processes behind were wrapped this way, and got 413, 408, 414 and 200 respectively, with no process left in any case.

**CLI commands** (implementation): `host`, `run`, `spawn`, `available`, `ls`, `get`, `resize`, `terminate`, `kill`, `wait-children`, `suggest`, `trace`, `audit`, `events`, `notify`, `inbox`, `beacon`, `done`, `resize-request`, `metrics`, `top`, `completion`. Exit status: 0 for success or a lease that ended 2xx; 1 when the daemon refused or failed, with the error as JSON on stderr; 2 for a usage error; 3 when the lease waited for ended other than 2xx; 130 when interrupted. If the connection to the daemon breaks mid-way, the CLI says that the daemon has stopped and its leases have all ended.

## Termination codes and audit

When a lease ends, subactld produces exactly one termination code, with evidence. Codes are grouped by "where the problem lies", like HTTP status codes. subactld only states facts; retrying, changing approach, escalating to a person are the planner's policy, and subactld gives no advice.

| Class | Meaning | Codes |
| --- | --- | --- |
| 2xx normal | ended normally, or released gracefully by the planner | 200 COMPLETED, 204 RELEASED |
| 4xx over its own quota, in violation, or ended deliberately | the task exceeded its own envelope, or the planner ended it; retrying as is will most likely fail again | 413 OOM\_LEASE\_LIMIT, 408 WALL\_EXCEEDED, 414 OUTPUT\_EXCEEDED, 451 ESCAPE\_DETECTED, 499 KILLED (ended by the planner), 410 IDLE\_REAPED (reclaimed after idling past the deadline once done, or stalling) |
| 5xx environmental | not a problem of the task itself | 503 SAFETY\_NET, 502 PARENT\_DIED (reclaimed in cascade because a parent or the lead exited), 520 EXTERNAL\_ABORT, 504 REAP\_INCOMPLETE, 500 SUBACTL\_INTERNAL (including fail-closed cleanup when subactld restarts) |

**Structured termination record** (pushed with the event, and available from `GET /v1/leases/{id}`):

```json
{
  "code": 413, "name": "OOM_LEASE_LIMIT",
  "lease": "ls_7f3a", "kind": "renderer", "parent": "ls_01c2",
  "dimension": "memory", "mem_basis": "working_set",
  "peak": 3242719232, "limit": 3221225472, "enforcement": "hard",
  "evidence": {"oom_kill": 1, "top_procs": ["chrome (renderer) x11"], "reaped_pids": 14},
  "message": "renderer went over its hard memory limit of 3GiB and was ended as a group; the peak came mostly from 11 Chrome renderers."
}
```

Implementation: sizes are always integers in bytes; there is also `at` (when it ended). The message is generated from a template, in English; the evidence has `top_procs` (the 3 processes using the most memory: pid, name, memory; resident set on Linux, phys\_footprint on macOS), `reaped_pids` (how many processes were reclaimed), `exit_code`, `signal`, `step` (where graceful termination stopped), and more. Once the kernel has killed a group at its hard limit there is nothing left to list, so once a lease's use reaches 75% of its limit, or would hit it within two samples at the current rate, a list is recorded at every sample, and at the end, if the processes are gone, the one from the last 10 seconds is used. When reclamation is actually incomplete (processes remain after the kill, or the cgroup cannot be removed), the code is 504, and the code it would have had is in the evidence's `intended_code`.

**Attribution must be exact**. In the four cases below the planner's right response is completely different; subactld tells them apart by limit events, safety-net records, the exit signal, and whether subactld sent that signal:

- OOM within its own hard limit: 413. The envelope was too small, or the task itself too large.
- Ended by the safety net: 503. An environmental cause, unrelated to the task.
- Main process killed from outside (a signal subactld did not send, for example a harness hard-killing a subagent when its LLM quota ran out, or the agent crashing): 520. subactld does not judge the outside cause; it records only the exit signal and time. Implementation: a main process exiting with a non-zero status (no signal) is also 520, with the status in the evidence; only exit status 0 is 200.
- The main process exited normally but left descendants behind: every descendant is reclaimed; the code follows the main process's exit status, and the evidence lists the leftover processes reclaimed.

**Audit log**: append-only JSONL at `$SUBACTL_STATE/audit/YYYY-MM-DD.jsonl` (implementation: the state directory is set by `--state`, default `/var/lib/subactl`; writing happens in its own goroutine with a buffer of 4096 records, and when full or on a write failure records are dropped and counted). Every state transition, resize and safety-net termination writes a record with: the lease tree path, kind, envelope and its changes, peak use, termination code and evidence, and caller identity. The audit log has two uses: `subactl audit --lease <id>` rebuilds a single lease's complete timeline from it, and `suggest()` computes the historical distribution from it.

## Division of work between the planner and subactl

Any node that creates child agents is a planner: the lead is, and so is a child agent that starts deeper agents. The planner makes low-frequency semantic and planning decisions, triggered by events; events here are not only failures, since creating a child agent is itself a normal event. subactl does real-time, numeric, deterministic execution and does not depend on an LLM. An LLM planner calls the subactl CLI from its bash tool, as taught by the accompanying skill. The skill teaches only subactl itself: how to use spawn, available, terminate, wait-children and done, how to choose an envelope, and what each termination code means. This is knowledge pre-training does not provide, kept to a few hundred tokens. A planner written as code uses the public Go client package.

|  | planner (any node in the agent tree) | subactl (subactld) |
| --- | --- | --- |
| Frequency | low, event-triggered | real time, high |
| Kind of decision | semantic and planning: whether to create, how much to allocate, what to try after a failure, whether to tell the user | deterministic execution: validate, account, enforce, report |
| Autonomous actions | anything within its own subtree | only declared rules: termination at the limit, cascading termination, the safety net, idle reclamation, policies the planner declared beforehand |
| Does not | fork child processes directly, act outside its own subtree | make trade-offs of its own: grow automatically, retry automatically, choose whom to end by itself, give heuristic advice |

**The agent tree and the lead**

- subactl is the only interface for creating child agents, and maintains the complete agent tree: parent-child relations, and descendant relations where orchestration nests.
- The **lead** is the entry of an agent tree, the top-level lease with no parent, started by a person with `subactl run -it` (or by running mini-agent directly, which self-wraps). Not called root, to avoid confusion with the Unix root user.
- The lead goes into an acgroup like every other node, has its own hard limit and is counted in the ledger, so the harness's own leaks are contained too; "external use" is then only non-agent processes. The lead's holder is the `subactl run -it` CLI process: when the user closes the terminal, the CLI exits and the whole tree is reclaimed in cascade (502). Detach and reattach are for v2.
- When any node (the lead included) exits, normally or not, subactld ends its whole subtree in cascade, code 502.

**Leads and isolation between leads**: several leads can run on one machine at once, sharing one subactld.

| Dimension | Isolated | Mechanism |
| --- | --- | --- |
| memory | yes, hard | a lead's envelope is the amount reserved for the whole tree, all descendants nest inside it, and once admitted no other lead can take it. Competition happens only at admission and growth: first come, first served, 409 when capacity is short |
| CPU | proportionally | each lead's acgroup gets an equal cpu.weight by default: idle CPU can be borrowed, under contention each gets its share. v1 does not control IO; io.weight is optional. Implementation: measured to meet the bound on a physical machine, see acceptance criterion F27 |
| control plane | yes | identity is by acgroup, and a node in one lead's tree cannot query, end or notify a node in another tree (403) |
| operator (a person) | by uid | a process in no acgroup is an operator. A uid can see and end the lead trees it started, and `subactl top` shows all of this uid's leads by default; only the root user or the subactl-admin group can act on every lead. Implementation: a lead belongs to the operator who started it (`owner_uid`), separately from the uid the agents run as; root must name a non-root user with `--uid` |
| OS level | not by default | agents of different leads run as the same uid by default, and can go around the API to kill each other's processes or read each other's scratch directories. An optional per-lead uid mode gives each lead tree its own uid from a pool. Off by default, because file system sandboxing is a non-goal and several uids bring read/write permission problems in worktrees |
| safety net | by rule | a kill\_order hint applies only within the subtree of the planner that submitted it. Across leads, the lead with the largest current use is chosen first, and then within it by its own hints and the default rules |
| failure domain | no | there is only one subactld; when it crashes, fail-closed reclaims every lead. v1 accepts this; v2 solves it with per-lease supervisors |

- **A lead's default envelope**: when `subactl run -it` has no `--memory`, min(effective capacity × 50%, remaining capacity), shown in the TUI's status line; ask explicitly for more. The default cannot be "everything left", or the first lead would take the whole capacity.
- **What is shared on purpose**: `suggest()`'s history is shared across leads by kind, more data making it more accurate; the audit log is one set of files, filtered by permission when read through the API.

**Permissions: only your own subtree**

- The caller's identity is the acgroup of the process at the other end of the connection (see identity in "Daemon API"); no tokens.
- A node can query, create, resize and end only within its own subtree, never its siblings or ancestors.
- A child cannot grow itself, only ask its parent (resize-request). The request has only structured fields (target value, current use), and reaches the parent's planner as an event, which decides whether to approve.

**Declarative policies**: when creating a child agent, a planner can attach rules that subactld executes deterministically at run time. Responses within seconds then do not go through the LLM, while the decision stays with the planner. v1's rules are a closed set of two; anything not declared is not acted on. Retrying belongs to the planner, and subactld offers no retry rule.

```yaml
on_near_limit: {grow_to: 6GiB}  # on NEAR_LIMIT, grow to this value within the parent's room; not at all if the room is short
kill_order: 2                   # an ordering hint for the safety net, smaller numbers ended first
```

**Creating a child agent**

1. The planner calls `subactl available` for the resources available. The answer is its own subtree's remaining budget, not raw data about the whole machine: remaining memory, remaining CPU, number of live children, the host's effective capacity, and `suggest()`'s value for each kind. The exact granularity is to be decided; everything carries mem\_basis.
2. The planner decides whether to create it now, and how much to give.
3. The planner calls acquire. The query was only a reference: acquire is an atomic check and reservation, and returns 409 if the room has meanwhile been used by another planner.

**Communication and the termination protocol**

subactld needs to communicate with planners and their child agents, including inserting control messages into a child agent's current conversation. subactld cannot write into an LLM's context directly, so communication has two layers: subactld keeps an inbox for each lease, and the agent's harness adapter injects the inbox's messages into the conversation. v1 has only two delivery channels:

| Channel | When | For |
| --- | --- | --- |
| harness hook adapter | injected as a system message before the agent's next tool call or at the start of its next turn | harnesses built on subactl that support hooks. v1 provides one reference adapter, built into mini-agent |
| signals only | immediate, but without meaning | agents without an adapter, such as a third-party agent wrapped by subactl run |

A message is seen only at the agent's next action, and an agent that is generating or stuck may never see it, so graceful termination must have a timeout:

1. **Notify**: deliver TERMINATE\_REQUESTED to the target agent's inbox, with a reason and a deadline. The agent should save its progress, end its own subtree and exit. Implementation: with `recursive`, it is delivered to every running lease in the subtree.
2. **Wait**: 30 seconds by default, or as the planner says.
3. **SIGTERM**: if it has not exited, send SIGTERM to the main process and wait 10 more seconds.
4. **Whole-group termination**: if it still has not exited, kill\_tree the whole acgroup with its subtree, and verify zero processes.

The termination code is terminate's code parameter (204 or 499); terminations triggered by rules keep their own code. The evidence records the step at which it ended.

**Trusting messages**: a channel into the conversation is itself an entry point for prompt injection, so it is tightly limited:

- An inbox accepts only two sources: control messages subactld generates itself, and messages from the target's ancestor planners forwarded by subactld. Siblings and children cannot send to it. Implementation: the operator who owns the lead tree may also send, with the source marked `operator`; a node cannot send to itself; only the lease itself can read its inbox; each inbox keeps the latest 256 messages.
- Every message carries a source label (subactld or which ancestor lease), injected by the adapter in a fixed template, and a message's content cannot forge its source.
- Control message types are a closed enum (TERMINATE\_REQUESTED, NEAR\_LIMIT, RESIZED, CAPACITY\_SHRUNK). Free text can appear only as the reason field from an ancestor planner. Implementation: control characters are removed from reason, newlines folded to spaces, and it is cut at 500 bytes; the adapter injects with the fixed template `[subactl control message from <source>] <type> ...`, and reason appears only in quotes.
- Events sent up to a planner are all generated by subactld from structured data. The only string a child submits is done's result path: it must be inside that child's scratch directory and use only whitelisted characters; events carry only the path, never the file's content inline.

**The only channel: control plane only (option A)**

- Control-plane communication between a planner and its subagents must go through subactl, and only through subactl. The control plane is: lifecycle, resources, state, progress beacons and control messages.
- The data plane (task descriptions, result files) does not go through subactl and is passed only by reference. For example a subagent writes its result into its lease's scratch directory and hands the path to the planner with done. So the audit log sees "who delivered what, and when", while subactl carries no content itself and takes on no persistence or back-pressure.

**Interjecting both ways**: subactl can interject both down, to a subagent, and up, to a planner (not necessarily the lead). Upward events include: a child hit its limit, exited, asked to grow, stalled, idles after finishing. The adapter injects only at the planner's turn changes, so delivery is naturally batched: one summary per turn, the same (lease, event type) merged into one line, with no separate rate-limiting engine. Implementation: the event types are `TERMINATED`, `NEAR_LIMIT`, `RESIZED`, `RESIZE_REQUESTED`, `DONE`, `IDLE_DONE`, `STALLED`, `CAPACITY_SHRUNK`, `ESCAPE`, plus `BEACON`, which goes only into the trace and is not broadcast; a merged summary reads like `ls_xxx NEAR_LIMIT (x100)`.

**Advice, deadlines and fallbacks**: an LLM is responsible for following subactl's advice gracefully, but cannot be guaranteed to follow it, so every piece of advice subactl gives must come with a deadline and a deterministic fallback when it passes. subactl gives only the advice in the table below, never suggestions based on heuristics.

| Advice | To | Deadline | Fallback |
| --- | --- | --- | --- |
| please end yourself | subagent | grace (30 seconds by default) | SIGTERM, then kill\_tree |
| about to hit your limit | subagent and planner | use reaching the hard limit | the kernel ends the group (413) |
| the host is short, please release | planner | 5 seconds (only when MemAvailable falls below the reserve line) | safety net (503) |
| a child is idle after finishing | planner | max\_idle\_done (60 seconds by default) | reclaimed (410) |
| a child has stalled | planner | max\_idle (30 minutes by default) | reclaimed (410) |

Implementation: a stall is reported to the planner once after 10 minutes of silence and reclaimed at 30 minutes; idling after done is reported once as soon as done is reported, and reclaimed at 60 seconds. Only leases that have sent a beacon take part in idle detection; a new beacon restarts the clock.

**Progress beacons and idle detection**

A hook-style adapter runs only at tool calls, turn changes and similar moments, and cannot answer polls. So v1 has no polled heartbeat; instead the adapter reports a progress beacon at every hook (`POST /progress`), with the state (working / awaiting\_input / done), a timestamp and the turn count. Reporting does not go through the LLM and costs no tokens.

- subactl does not disturb the planner in normal operation, and pushes upward only in two cases: **idle after finishing** (the state is done, but the process is still alive); **stalled** (the state is working, but no new beacon for N minutes in a row).
- No beacon does not mean the process is dead: an LLM generating for a long time or a 20-minute compile produces no beacons. A process actually exiting is detected exactly by pidfd and the main process's exit, so v1 has no "reclaim when the heartbeat stops". A stall only notifies the planner; if the planner never acts, the lease is reclaimed only at the max\_idle deadline.
- On a push the planner decides: end it, ask through notify, or wait a while.
- **done**: when a subagent finishes its task, the adapter calls `subactl done` (optionally with a result path). "Finished but nobody reclaims it" is then found at once.
- **awaiting\_input**: waiting for human input is legitimate idling, and the idle clock pauses in this state.
- An agent without an adapter has no beacons, and only the wall-time limit is its backstop.

**Orphans and leaks**

- Orphans: in the agent-tree model there are structurally none, since when a parent exits its subtree is reclaimed in cascade. The three remaining sources of orphans each have a mechanism: escaping processes (escape prevention, with the coverage described earlier), descendants left after the main process exits (reclaimed when the main process exits), and subactld itself crashing (fail-closed cleanup at restart).
- A parent still alive but not stopping its children: covered by the idle detection above.
- Non-process resources: each lease gets a scratch directory. v1 sets no size quota; disk is not among the six problems. When a child lease is reclaimed, its scratch directory is kept until its parent lease ends and deleted then, so that the planner can collect results after the child is reclaimed; a lead's scratch is deleted when the lead ends. Ports, sockets and the like are released with whole-group termination.

**Finishing**: a sensible way for a planner to finish is to call `subactl terminate --recursive` or `subactl wait-children` first, confirm that all child agents have ended, and then exit. This is the graceful path, and correctness does not depend on it: when a node exits abnormally, subactld still reclaims its subtree in cascade.

**Validation and errors**: an invalid argument or command is refused as a whole, never partly executed. Errors are structured and name the faulty field, the expected value and a correct example, so that an LLM can correct itself. Every mutating command supports `--dry-run`.

**Scope**: subactl applies only to agent harnesses built on it. Third-party harnesses (and the subagents they start internally) do not go through subactl and are not managed by it. `subactl run` can wrap a third-party agent as a whole, as a degenerate leaf node.

**Settled** (implementation): `available` returns the remaining memory, `memory_self`, the amount given to children, the number of live children, the host's effective capacity and the suggestion per kind, all with mem\_basis and without CPU; `max_idle` is 30 minutes, the stall notice 10 minutes, `max_idle_done` 60 seconds; `subactl run`'s degenerate third-party mode is kept.

## Demo: mini-agent and the demo task

mini-agent is a minimal multi-agent harness built on subactl, used to demonstrate and stress subactl. It borrows Pi's minimalist design: the model gets only four tools (read, write, edit, bash), and the system prompt stays under 1000 tokens; child agents are started not by a dedicated tool but by bash calling subactl.

|  | Pi | mini-agent |
| --- | --- | --- |
| A person starts the entry agent | run `pi` in a shell | run `mini-agent` in a shell; it wraps itself as `subactl run -it` and becomes the lead |
| Starting a child agent | `bash: pi -p "..."` | `bash: subactl spawn --wait -- mini-agent --task ...`, or simply `mini-agent --task ...`, enrolled by self-wrapping |
| A child agent's resource boundary | none | its own acgroup, nested in its parent's |

**Implementation**:

- **Written in Go, a separate module**: in `examples/mini-agent`, depending only on subactl's public `client` package and CLI. No third-party libraries; everything below is implemented in-house, about 1000–1500 lines in all.
- **Agent loop** (about 300–500 lines): message list → call the LLM → parse tool calls → execute in order → append results → loop. It provides event callbacks, a steer queue (inbox messages injected after the current turn's tool calls end), a hook before each tool call (no new tool runs after TERMINATE\_REQUESTED) and an abort signal.
- **LLM access** (about 200–300 lines): the brain interface is a single streaming call. The scripted brain implements it directly; the real brain speaks the Anthropic Messages API over `net/http`, parses SSE itself and assembles tool-call JSON that arrives in fragments. Implementation: `--provider` picks the endpoint: `anthropic` (native, default claude-opus-5-5, with the server-side refusal fallback and streamed tool input enabled), `deepseek` (`https://api.deepseek.com/anthropic`, default deepseek-v4-pro), `zai` (`https://api.z.ai/api/anthropic`, Bearer authentication, default glm-5.3); `--model` and `--base-url` override them. Assistant messages are replayed to the provider as received (including signed thinking blocks). 429, 402, rate\_limit and billing errors count as running out of quota; an over-long prompt counts as context overflow; a refusal is its own error; an error event mid-stream and a broken stream are both errors. A tool call cut off by max\_tokens mid-way is not executed. The compatible endpoints have all been tested live; the native endpoint only against recorded streams.
- **Four tools** (about 200 lines): read, write, edit (exact-match replacement with a uniqueness check, and readable errors on a mismatch so the model can correct itself), bash (fixed bash semantics, with a timeout and an output limit). Implementation: bash's output goes to a file rather than a pipe (otherwise background processes left by the command would keep the call hanging), and the model sees its head and tail with the path of the full file; on timeout the command's whole process group is killed; under subactl, a child agent's command output is also copied to its own stdout, so it falls under the lease's output limit.
- **Line-mode TUI** (about 150 lines): output appended line by line, one input line at the bottom, raw mode switched with `golang.org/x/term`. Implementation: each message is printed whole once complete, not streamed character by character; input is ordinary line reading, and raw mode is used only on the `subactl run -it` side. No full-screen panes or differential rendering, which also avoids the problems of CJK character widths and input methods. A node's transcript is viewed with `subactl trace` or by reading the file; resources are observed with `subactl top`; for demos the two are split side by side in tmux.
- **Built-in adapter**: reads the inbox at the loop's tool-call and turn-change hooks, injects control messages, and reports progress beacons (with the tool name and sequence number).
- **Context overflow**: no compaction. Tool output has a hard limit, the excess is cut and the model told which file holds the full content; when the conversation exceeds the context window, a worker ends with a non-zero exit status and notes context\_overflow in done, and the planner decides whether to split the task and dispatch again.
- **Ctrl-C**: the lead runs in a pty. The first press interrupts the current turn, the second exits the lead, and the whole tree is reclaimed with it. Implementation: tool calls of the interrupted turn that have no result yet all get an "interrupted by the user" result, so the conversation can continue; Ctrl-C at the prompt exits directly. When a real model hits the provider's quota limit, the lead makes the request once, reports, and returns to the prompt without retrying.

**Two brains**, sharing the tool layer, harness and adapter entirely:

- **scripted**: a seeded script implements the brain interface directly. Results are deterministic and reproducible, cost no tokens and need no HTTP server; used for the comparison experiments. At a given time the script returns an out-of-quota error, to inject an external abort deterministically.
- **real**: the Anthropic Messages API over the standard library's `net/http`, for demonstration; it consumes quota. The network paths (a broken SSE stream, rate-limit errors) are covered only under this brain.

**The demo task: upgrade all of radix-ui/primitives to React 19**

- **Target repository and baseline**: [radix-ui/primitives](https://github.com/radix-ui/primitives), with the baseline pinned at commit `af1e3b41cb5c49d7b8c073d2aec34d5074c6dd17` (2024-06-03, "Publish release candidate"), the parent commit of upstream PR [#2934 React 19 compatibility](https://github.com/radix-ui/primitives/pull/2934).
- **Baseline state** (verified from the repository's history): yarn berry workspaces at the time, not pnpm as now; Node 18; 54 packages under `packages/react/`, 41 stories files, 14 jest test files. The type check is a single repository-wide `tsc --noEmit`, tests use jest, component previews Storybook 7 (webpack5, port 9009), end-to-end tests Cypress 8; the react dependency is ^18.0.0.
- **The reference answer** consists of three upstream changes: #2934 (2024-06-10, React 19 compatibility in the code, 105 files changed, about 45 of them component sources), #2952 (2024-06-19, adding the React 19 RC to peer dependencies), #3283 (2024-12-12, fixing composeRefs under React 19). The demo task's goal is the released React 19; these three changes are only a reference, and need not be matched line by line.
- **Completion**: with @types/react 19 in place, the root tsc passes; the package's jest tests pass; the package's stories render under React 19 without console errors, and their screenshots show no regression against the React 18 baseline. Screenshots are taken by Playwright driving Chrome, a tool in the task environment, not a dependency of subactl or mini-agent.
  - Implementation: the goal became "React 19 at run time, types still @types/react 18": the upstream reference fix does not compile against @types/react 19, and the scripted brain does not write code of its own. Done means no tsc errors in the package, jest passing, and stories rendering without console errors (404s from hot-reload polling do not count); differences from the baseline screenshots are only recorded, not counted as failures.
  - Implementation: Storybook 7 chooses React's root API by version number and knows only 18; `@storybook/react-dom-shim` must be pointed at its react-18 variant in the webpack configuration to render under React 19.
  - Implementation: the task's package list is the packages under `packages/react/` that upstream #2934 changed, 54 in all; a worker applies upstream's change for its package in its own worktree, and a fixer applies that package's change at the second level and every package's changes at the third.
- **Flow**: the lead reads the package list and fans out one worker per package; each worker changes the package's dependencies and sources in its own git worktree, runs the root tsc and the package's jest, then starts Storybook and checks the package's stories with screenshots in Chrome; when a test fails it starts a child worker to find the problem, recursively.
- **The resource pressure comes from the task itself**: the baseline's type check is repository-wide, so every worker runs a full-repository tsc; Storybook 7's webpack build is memory-hungry; several workers starting Storybook at once fight over port 9009, so agents have to change ports themselves, and easily forget to stop what they started. These are exactly the behaviours to observe. Measured: the root tsc takes only about 6 seconds; the pressure comes mostly from Storybook plus Chrome, about 1.7GiB per worker.

Keep the two toolchains apart: radix is the TypeScript/React project the agents transform, and yarn, tsc, jest, Storybook and Playwright belong to the task environment, called by agents from their bash tool; they are what produce the resource pressure to be observed. subactl and mini-agent themselves use only Go and depend on none of them.

**Greedy behaviour**: the behaviours in the table below are triggered deliberately in the scripted script, and may also arise naturally under the real brain. Each corresponds to a known real failure.

| # | Greedy behaviour | Form in the demo task | Corresponding real failure |
| --- | --- | --- | --- |
| G1 | dispatching too much | one worker per package, fan-out up to 32 at once, recursive dispatch on failure, depth 3 | [anthropics/claude-code#74001](https://github.com/anthropics/claude-code/issues/74001): 5–7 background agents, about 6.5GB each |
| G2 | starting browsers and services and never stopping them | a worker starts Chrome with Playwright for a screenshot and never closes it; starts a storybook dev server "to have a look" and never stops it | [anthropics/claude-code#15861](https://github.com/anthropics/claude-code/issues/15861), [anthropics/claude-code#50783](https://github.com/anthropics/claude-code/issues/50783) |
| G3 | context bloat | the transcript buffer inside the agent process grows X MB per step and is never freed (simulated by the script) | [anthropics/claude-code#33350](https://github.com/anthropics/claude-code/issues/33350), [anthropics/claude-code#74001](https://github.com/anthropics/claude-code/issues/74001) |
| G4 | no cleanup on the error path | mid-run the scripted brain returns an out-of-quota error, and in without mode the harness SIGKILLs only the subagent itself | the triggering problem in the background, [anthropics/claude-code#83412](https://github.com/anthropics/claude-code/issues/83412) |
| G5 | runaway output | a worker is stuck at a yarn interactive prompt or in watch mode, writing to stdout without end | [anthropics/claude-code#47771](https://github.com/anthropics/claude-code/issues/47771) |
| G6 | unbounded parallelism and search | tsc -b and tests in parallel at 4 times the number of cores, with an unbounded file walk at the same time | [anthropics/claude-code#91523](https://github.com/anthropics/claude-code/issues/91523), [anthropics/claude-code#86238](https://github.com/anthropics/claude-code/issues/86238) |

Implementation: G2's browser is started by the agent as a separate process and then left unmanaged; G5 writes at about 5MB/s, so that it can be observed even without an output limit; G6 applies only to the quarter of workers chosen by the seed: if every worker did it, forty concurrent type checks would need more memory than the whole chamber, and no envelope could hold them.

**Script parameters** (`scenarios/*.yaml`): seed, fan-out limit, recursion depth, leak per step X, fault-injection times, and a switch for each greedy behaviour. Implementation: there are also `fail_rate`, `max_packages`, the time and share of external interference, `greedy.unbounded_share`, and, under subactl, `envelopes` (the fixed envelope per kind), `grow_factor` and `output_limit`. The radix scenario's fixed envelopes are lead 9GiB, worker 3500MiB, fixer 1GiB, runaway 256MiB.

**Blast chamber**: the without group is designed to bring the machine down, so each run happens inside an outer isolation unit, a separate VM or an outer cgroup with a hard limit (16G, for example). Implementation: an outer cgroup capped at 12GiB, built in a privileged Docker container; groups B and C run subactld inside the chamber, with `--capacity-cgroup` taking the chamber's limit and use as "the whole machine", and a reserve line of 15% of the chamber. A watchdog starts a probe process in the chamber every 5 seconds and declares a meltdown after 60 seconds without a response. Groups B and C would prefer a VM: subactld's effective capacity is based on MemTotal and MemAvailable, which in an outer cgroup reflect the whole host rather than the chamber's limit. If only an outer cgroup is possible, subactld has to be configured to use that cgroup's memory.max and memory.current as its capacity basis. This protects the development machine and still collects complete data inside the chamber. Outside it there is also a watchdog: if the chamber does not respond for 60 seconds in a row, it is forcibly ended and the run recorded as a meltdown.

**Two ways of running**: in without mode mini-agent turns self-wrapping off, the harness starts child agents directly as child processes, and deliberately reproduces a real defect: on a limit error it sends SIGKILL to the subagent's PID only, not its process group, so grandchildren (Chrome, dev servers) become orphans. with mode is the normal subactl integration. The two modes differ by one start-up switch; if more than that had to change, the boundary design would have failed. Implementation: the switch is `--mode with|without`; with mode also attaches the adapter. The harness's "kill the PID only" is the same code in both modes; only in with mode, the killed main process makes subactld reclaim the whole acgroup.

## Comparison experiments

Three groups use the same script, the same seed and the same hardware, with at least 5 runs each. They are compared on: whether the machine goes down, whether processes are left behind, task completion, work lost, and resource utilisation.

| Group | Setup | Question |
| --- | --- | --- |
| A without | the orchestrator spawns subagents directly, with no limits | does the problem reproduce reliably? |
| B static envelopes | under subactld; a fixed envelope per kind of subagent, the same in every run | do never overselling and hard limits alone make it safe? |
| C adaptive envelopes | under subactld; initial envelopes from `suggest()`, grown by rule on `NEAR_LIMIT`, history accumulated across runs | can adjusting envelopes from measurements raise utilisation while staying safe? |

Group C's planner is a deterministic rule (a code planner), for reproducibility. A version with an LLM as planner is a later variant. Implementation: done as group L; results under "Results" below.

Implementation: group B starts every run with empty history; group C's five runs share one audit directory, so suggestions accumulate across runs. Group C's "rule" is that a worker takes `subactl suggest`'s value as its initial envelope (B's fixed envelope when there is no history) and declares a `grow_to` of 1.5 times.

**Fault-injection schedule** (the same in every run):

- t = 2 minutes: G1's fan-out begins.
- t = 5 minutes: an external abort. The scripted brain returns an out-of-quota error, and the harness at once SIGKILLs the running subagents (only the subagents themselves, not their process groups).
- t = 7 minutes: external interference. A non-agent process takes 30% of the effective capacity within 30 seconds and holds it for 4 minutes.
- t = 9 minutes: G5's runaway output.
- t = 15 minutes: the script ends, followed by 60 more seconds of observation.

| Metric | Definition |
| --- | --- |
| host\_peak\_ws | the blast chamber's peak working set (GB) |
| host\_oom | outer OOMs, and whether the watchdog declared a meltdown. Implementation: how often the chamber's own limit was hit (`oom` in the chamber's `memory.events.local`); leases killed inside the chamber at their own limits are not counted here but recorded as oom\_kills |
| orphans\_after | marked processes left 60 seconds after the end |
| leaked\_ws\_after | memory held by those processes (GB) |
| completion | share of subtasks completed |
| work\_lost | subtasks ended by 413 or 503 that must be redone. Implementation: group A has no termination codes, so it counts the subagents the harness killed at the quota limit |
| util\_mean | mean utilisation: the sum of the acgroups' use ÷ effective capacity. Implementation: the chamber's limit is the denominator, so the three groups are comparable |
| safety\_net\_count | safety-net terminations |
| makespan | total time of the script (minutes) |

**Falsifiable hypotheses** (conclusions follow the data; refuted hypotheses are reported as such):

1. **H1**: after G4, group A's orphans\_after > 0; groups B and C are always 0.
2. **H2**: group A has at least 1 outer OOM or meltdown in 5 runs; groups B and C always have host\_oom 0.
3. **H3**: group C's util\_mean is at least 15 percentage points above group B's, and its work\_lost is no higher than B's.
4. **H4**: during external interference, groups B and C have safety\_net\_count ≤ 1 per run, and the planner received `CAPACITY_SHRUNK` before the safety net acted.

**Results** (implementation, 2026-11-17, round 4, physical x86-64 machine, 12GiB blast chamber, 5 runs per group; reports and data in `examples/mini-agent/bench/results/`):

| | Verdict | Basis |
| --- | --- | --- |
| H1 | refuted (one run of A fails it) | group A left 38, 55, 0, 12 and 116 processes; groups B, C and L left 0 in all 15 runs. In A's run 3 the OOM killer in the chamber killed 2540 processes, the orphans among them, so none were left when counted |
| H2 | holds | group A hit the chamber's limit in every run; groups B, C and L never did |
| H3 | refuted | group C's utilisation is 4.3 points above B's (15 required); work lost, C 21.4, B 15.2 |
| H4 | holds for B, refuted for C (round 5) | in round 4 the interference took only 30% of the chamber and the safety net never fired; round 5 raised the interference to 75%, arriving while workers run: B ended 1 lease in each of its 3 runs, C 2 in each of its 3 (one of them by the capacity guard in the kernel); every time `CAPACITY_SHRUNK` came first, nothing was left, and the chamber's limit was never reached |

Completion: group A 4%, B 41%, C 53%, L 24%. Group C's suggested worker envelope was stable at about 2.9GiB in all five runs (B's fixed 3.5GiB), no longer rising with 413s; the tighter envelopes let more workers run at once and complete more, but greedy workers hit their limits more often too, so more work is lost. H1's rule was fixed before the experiment and is not changed because of the result.

**A model as planner (group L, implementation)**: the lead is replaced by DeepSeek, which decides each worker's envelope and how many run at once; the workers and fixers below it are the same scripted brain as in groups B and C. It kept the machine safe (nothing left, the chamber's limit never reached), but completed 11%–39%, 24% on average, below group B's fixed envelopes and further below group C's rule: its envelopes for the same kind of worker were inconsistent between runs and within a run, mostly smaller than greedy workers need, so more hit their limits; when it gave less, it also completed less. Group C's rule, which needs no judgment (the p95 of historical peaks × 1.2), does better. This is the result of one model, one prompt and five runs.

Round 5 also ran an "informed" version (`results/L-informed/`): the prompt says how the suggestion is made, that envelopes below it lose work, and to run as many as fit. That solved the envelope problem (from run 3 on every upgrade worker got the suggestion, 2.9GiB, the same as group C), yet completion did not rise: 9%–39%, 22% on average. The bottleneck is dispatch speed: B and C start 80–95 upgrade workers per run, it started only 11–35, because it dispatches by hand, one bash call at a time, and each of the model's turns takes seconds to minutes. Its best run (39%) was precisely the one in which the model wrote itself a dispatcher script and handed the rest of the time to it. As a planner, what a model lacks is not better numbers but a loop: group C is that loop with the suggestion built in.

**The real-model check (implementation)**: DeepSeek as both lead and workers, doing the code changes itself, 8 packages, 2 runs each of groups A and B. Group A completed 8/8 both times but left 153 and 84 processes (storybook, esbuild, up to 4.25GiB), and hit the chamber's limit thousands to tens of thousands of times; group B completed 8/8 both times, left nothing, and never reached the limit. (Round 5 added a third run of A for its per-second data: again 8/8, 141 processes and 2.94GiB left, the chamber's limit hit 9432 times.) H1 and H2 as found with the scripted brain hold with a real model too; and a real model leaves dev servers running on its own, without being told to by a script.

**Capacity guard**: in the first 5 runs of group L, run 5 burst within two seconds during the external interference and hit the chamber's limit once (see the capacity guard under the safety net). With the guard added, group L was run again 5 times; in groups B and C and in group L's other runs, the chamber's use never exceeded "total − reserve line", where the guard would not act, so they were not run again.

The three earlier rounds: round 1 is void (a defect in the task environment); round 2 ran on Docker Desktop and found the defect of suggested envelopes rising with every 413; round 3 ran on the physical machine with the same conclusions as this round, but G5's runaway worker was taken for a free-text task and got the default 1GiB instead of the script's 256MiB, costing group C one worker for the last 6 minutes of every run; after the fix, groups B and C were run again as round 4, while group A kept round 3's runs (it has no envelopes and is unaffected). The first attempt of round 3 is also void: that Ubuntu 26.04 host restricts unprivileged user namespaces, and the Chromium the screenshot check starts exited for lack of a usable sandbox; it now runs without the sandbox, as Playwright does.

**Outputs**: `bench/report` generates the comparison charts and summary tables of the three groups, and saves the raw time series as parquet, for reproducibility. Implementation: `bench report` generates `report.md` (the verdict and basis for each of the four hypotheses, means per group, a table per run), `memory.svg`, `metrics.svg` and `series.parquet`; the rules for the verdicts are in code, and a hypothesis the data contradicts is judged refuted.

## Acceptance criteria

The functional criteria F1–F33 must all pass on Linux cgroup v2; macOS is accepted at the levels its capability matrix declares. The experimental criteria E1–E3 must be met. Each has an automated test or experiment script.

Implementation status (2026-11-17): `[x]` met, `[~]` partly met (the gap in parentheses). F1–F33 are all met. The integration tests pass in four Linux environments: Docker Desktop (arm64), GitHub Actions, a VirtualBox VM (Ubuntu 26.04), and a physical machine (x86-64, in privileged Docker); the macOS backend's tests pass on the development Mac and in the macOS job of GitHub Actions.

**Functional**:

- [x] F1 reclamation on parent death: after the planner holding the leases is SIGKILLed, 3 acgroups each with its own Chrome reach zero processes within 5 seconds, code 502, and no process with SUBACTL\_LEASE\_ID is left on the host. (Implementation: integration test: after the holder is SIGKILLed the whole tree is reclaimed within 7–13ms, recorded as 502)
- [x] F2 main-process exit and external abort: with the parent still alive, SIGKILL sent directly to a subagent's main process, bypassing subactld, reclaims all its descendants within 5 seconds, code 520; when the main process exits normally but leaves descendants, they are likewise all reclaimed, and the evidence lists them. (Implementation: integration test, including descendants that left the process group with setsid and nohup)
- [x] F3 escape prevention: a process in an acgroup that tries to migrate outside its acgroup through cgroupfs fails; a process outside the lease tree whose environment was not cleared is found by the scan and ended within 5 seconds, recorded as 451. Escapes by asking a third party are not part of acceptance. (Implementation: integration test: the migration is refused by the kernel; the scan finds and ends it within 0.5 seconds, recorded as 451)
- [x] F4 memory isolation: when an acgroup leaks up to its hard limit, only it is ended (413), and other acgroups are unaffected. (Implementation: integration test, including attribution when a parent and a nested lease each hit their limit)
- [x] F5 never oversell: at every moment the sum of allocated top-level hard memory limits does not exceed the effective capacity; the effective capacity does not change with the acgroups' own use; beyond it, acquire returns 409 mem\_budget at once. (Implementation: unit tests (simulated host readings) and property tests with random operations)
- [x] F6 resizing and nested admission: raising beyond the room is refused; lowering below "use + margin" is refused (below\_usage); a valid resize takes effect within 1 second; a nested acquire beyond `parent.memory − parent.memory_self − Σ siblings.memory` returns 409 parent\_budget. (Implementation: unit tests)
- [x] F7 NEAR\_LIMIT: after use reaches 90% of the hard limit, the event is pushed within 2 seconds. (Implementation: real kernel: pushed about 1 second after the allocation)
- [x] F8 capacity shrinking and the safety net: when an external process makes the allocated amount exceed the effective capacity, CAPACITY\_SHRUNK is pushed within 1 second, new acquires are refused (capacity\_shrunk), and no acgroup is ended; when the external process grows further and MemAvailable falls below the reserve line, at the end of the window leases are ended one at a time in termination order until available memory recovers, code 503, with none ended more than needed. (Implementation: real memory: an outer cgroup of 640MiB as the whole machine; after the warning and the 5-second window, exactly the largest leaf is ended)
- [x] F9 accurate metering: use is within 10% of the kernel's accounting (on the platform's mem\_basis), with sampling delay at most 2 seconds. (Implementation: on Linux −8.5% to −9.3% from the sum of the processes' VmRSS)
- [x] F10 termination code attribution: inject faults for 413, 408, 414, 451, 499, 503, 502, 520 and 410 one by one; every ended lease produces exactly one code, matching the injection, 100% accuracy. (Implementation: on a real kernel 11 faults (including 200 and 204) injected one by one, all 11 codes right, three times in a row)
- [x] F11 output limit: over the output limit the whole group is ended (414), and the output file is no larger than the limit plus one second of writing. (Implementation: the limit is exact as implemented: the file is exactly the limit in size, and output is not counted in the lease's memory)
- [x] F12 fail-closed restart: after kill -9 on subactld, each acgroup's hard limit still holds while it is down; within 5 seconds of the restart all acgroups that existed before reach zero processes, code 500. (Implementation: integration test; under systemd the daemon restarts within a second of kill -9, nothing left)
- [x] F13 honest levels: the macOS monitor backend reports its true enforcement levels; when the backend cannot reach the level `require` declares, enforcement\_unavailable is returned. (Implementation: both the Linux and macOS backends report honestly)
- [x] F14 nesting: a child acgroup's use counts toward its parent's quota; when the parent hits its limit or ends, the whole subtree is reclaimed; the parent's own processes are in the `_self/` leaf, and creating a nested lease never hits EBUSY. (Implementation: integration test)
- [x] F15 lead cascade: after a registered lead is SIGKILLed, its whole subtree reaches zero processes within 5 seconds. (Implementation: as F1; also measured on a VM by dropping the SSH session)
- [x] F16 subtree permissions: a child's process trying to end a sibling, grow itself or message a sibling gets 403 in every case, with no side effects; reading another node's environment does not let it impersonate that node. (Implementation: unit tests; identity comes only from the cgroup of the process at the other end of the connection, and no environment variable is read)
- [x] F17 graceful termination: an agent with an adapter exits on its own within grace; an agent that ignores the message is ended as a group within grace + 10 seconds; the evidence says at which step it ended. (Implementation: unit tests (simulated time): the three paths of exiting on its own, SIGTERM, and whole-group termination at grace + 10 seconds)
- [x] F18 declarative policies: on\_near\_limit.grow\_to takes effect within 2 seconds when there is room, and does not grow when there is none; kill\_order changes the safety net's order; where nothing is declared, subactld does nothing. (Implementation: unit tests)
- [x] F19 validation: a command with invalid arguments is refused as a whole with no side effects, and the error contains the field, the expected value and an example. (Implementation: unit tests covering the HTTP layer and --dry-run of every mutating command)
- [x] F20 idle detection: inject idling after done and stalling; each is pushed to the planner within 10 seconds of its threshold, exactly once; if the planner does nothing, the lease is reclaimed at the corresponding deadline, code 410; awaiting\_input does not count as idle; a long tool call with no beacon for 20 minutes is not reclaimed. (Implementation: unit tests (simulated time), including the 20-minute call without beacons not being reclaimed)
- [x] F21 beacons cost no LLM calls: progress beacons are reported by the adapter throughout, and the subagent's number of LLM calls does not rise because of them. (Implementation: the adapter calls the API directly from its hooks, not through the model. Test: the same scripted run makes 4 model calls with and without the adapter, plus 6 beacons with it)
- [x] F22 merging upward: 100 events of the same kind injected within one planner turn arrive at the next turn as one merged summary. (Implementation: adapter unit test: 100 events of one kind merged into one line)
- [x] F23 fallbacks for advice: every row of the "Advice, deadlines and fallbacks" table carries out its fallback after the deadline when the LLM does not respond. (Implementation: covered together by the tests of F4, F8, F17 and F20)
- [x] F24 scratch lifetime: after a child lease is reclaimed, its scratch directory can still be read by its parent; it is deleted when the parent lease ends; done with a path outside the scratch directory is refused. (Implementation: integration test)

* [x] F25 lead and self-wrapping: running `mini-agent` directly makes it a lead automatically, and its TUI works interactively in the user's terminal (including resizing); closing the terminal reclaims the whole tree within 5 seconds (502); when an agent runs `mini-agent --task X &` directly in bash, that process becomes a child of the current node. (Implementation: on a VM: in with mode the lead gets a terminal automatically and follows the window size; after dropping SSH the whole tree is reclaimed within 1 second, recorded as 502; children a real model started by calling mini-agent in bash joined the agent tree)
* [x] F26 shell-independent: leads started from bash, zsh and fish behave the same; the shell the agent's tool uses does not depend on the user's `$SHELL`. (Implementation: CI installs zsh and fish on Linux and macOS, and the test requires all three shells, failing if one is missing; the agent's bash tool always uses bash, regardless of `$SHELL`)
* [x] F27 isolation between leads: with two leads running at once, when one fans out to its envelope, the other lead's completion does not fall, and while that lead's CPU demand is no more than half the cores and the test load is mainly CPU (v1 does not control IO), its p95 task latency rises no more than 20%; cross-lead queries, terminations and notifications all return 403. (Implementation: on 16 identical cores of a physical machine p95 rose 1%–2% (four times), 9% with big and little cores mixed; on virtual machines the virtual CPUs are not independent and it cannot be measured. Cross-lead control-plane isolation passes)
* [x] F28 operator permissions: an operator of the same uid can see and end its own lead trees and cannot act on another uid's leads; the subactl-admin group can act on all. (Implementation: integration test over the real socket: another user (nobody) can neither see nor end someone else's lead, and can do both once in the subactl-admin group; the system's groups are restored after the test)
* [x] F29 safety net across leads: the safety net chooses what to end from the lead with the largest current use; one lead's kill\_order does not affect another lead's nodes. (Implementation: unit tests)
* [x] F30 metrics: `/metrics` exposes the listed metrics; after 10000 leases are created in a row, the number of series does not grow with the number of leases. (Implementation: unit test: the series count is unchanged after ten thousand leases)
* [x] F31 trace: for any ended lease, `subactl trace` rebuilds the timeline and attributes an injected memory spike to the specific tool-call beacon. (Implementation: the trace has 1-second samples, events, injected control messages and beacons with tool names, and can be rebuilt from the audit log; it also has `spikes`: taking the time between two consecutive beacons as one tool call, it lists calls in which memory grew by 64MiB or more, largest growth first. On a real kernel, a 256MiB allocation was attributed to the call in which it happened, three times out of three)
* [x] F32 isolating observability failures: with the time-series directory made read-only or full, the latency of NEAR\_LIMIT pushes and kill\_tree is unaffected and the drop count rises. (Implementation: unit test: with the audit directory read-only, the latency of NEAR\_LIMIT and kill\_tree is unchanged and drops rise)

- [x] F33 run-as identity: the uid of the lead's and the child agents' processes equals the uid of whoever started them, and no agent runs as root; when the operator is root and no non-root uid is given, `subactl run -it` is refused. (Implementation: integration test; also verified that sudo is refused under no\_new\_privs)

**Experimental**:

- [~] **E1 the baseline reproduces**: all 5 runs of group A satisfy the parts of H1 and H2 about group A. If not, make the script "greedier" before comparing the other groups. (Implementation: H2's part holds in all 5 runs; H1's part in 4. The run that fails it was not too little greedy but too greedy: the OOM in the chamber killed the orphans as well, so the script was not made heavier on this account)
- [x] **E2 safety**: all 5 runs of groups B and C satisfy orphans\_after = 0 and host\_oom = 0. (Implementation: all 10 runs satisfy it)
- [x] **E3 conclusions**: a verdict of "holds" or "refuted" for H3 and H4; the report and charts are generated by script and reproducible. (Implementation: H3 refuted; H4 held but untested in round 4, and after more pressure in round 5 holds for B and is refuted for C; `bench report --in <directory>` regenerates the report from the committed data)

Experiment status: the conclusions rest on round 4 on the physical machine, with H4 tested again in round 5; results under "Comparison experiments".

## Risks, open questions and milestones

The main cost of never overselling is low utilisation: subagents mostly do not use their whole envelope. Adaptive envelopes (group C) are meant to ease this, and how far they do is for the experiments to show; if it is still not enough, overselling comes back as a v2 option.

| Risk | Effect | Mitigation |
| --- | --- | --- |
| low utilisation from never overselling | fewer subagents can run at once | tighten initial envelopes with suggest(), grow on NEAR\_LIMIT; quantify the effect with experiment H3 |
| a sudden rise in external load | I-B temporarily does not hold | refuse new requests while capacity is short; the safety net acts only when MemAvailable falls below the reserve line, and CAPACITY\_SHRUNK first gives the planner a window |
| an agent actively escaping ([anthropics/claude-code#60488](https://github.com/anthropics/claude-code/issues/60488)) | hard limits fail | cgroup namespace, separate uid, escape scanning; see F3 |
| platforms measuring differently (macOS's phys\_footprint includes compressed memory, unlike Linux's working set) | the same envelopes behave differently on different platforms | all data carries mem\_basis; envelope suggestions are computed per platform |
| macOS can only monitor | hard limits are unreliable | state the level honestly and raise the reserve line; for hard limits use a Linux VM (v2). Implementation: the reserve line is 25%, and limits are checked every 200 milliseconds; see open question 1 |
| simulation differs from reality | experimental conclusions are hard to generalise | the demo task uses a real repository and real yarn, tsc and Chrome; the scripted conclusions are spot-checked with the real brain, and verified by wrapping a third-party agent with subactl run |
| edge cases of an in-house harness | errors in stream parsing, edit matching and the like make the experiments unstable | scope limited to what the demo needs (one provider, tools executed in order, line-mode TUI); what decides the experiments' conclusions is the scripted brain, which uses no network or parsing path; the real brain's SSE parsing is regression-tested with recorded responses |
| a single subactld is a shared failure domain | when the daemon crashes, every lead is reclaimed | fail-closed guarantees nothing leaks; v2 introduces per-lease supervisors |

**Open questions**:

1. ~~The default reserve line~~ Settled (implementation): max(15%, 8GiB), but not more than half of MemTotal, so that small machines still have capacity. On macOS raised to max(25%, 8GiB), likewise not more than half: without a kernel hard limit a lease goes over its limit by about what it allocates between two checks. Measured with a check every second, a lease allocating at 100MiB/s went 33MiB over and one at 2GiB/s 1GiB over; so on macOS the limit check also moved to every 200 milliseconds, bringing the overshoot down to 8MiB and 284MiB. The extra reserve is room for several leases overshooting at once.
2. ~~Defaults for `max_idle`, `max_idle_done` and the stall threshold N~~ Settled (implementation): 30 minutes, 60 seconds, 10 minutes.
3. ~~Whether to keep `subactl run`'s degenerate third-party mode~~ Settled (implementation): kept.
4. How to implement adapters for mainstream harnesses (Claude Code, Codex, Pi and others). v1 delivers only the adapter built into mini-agent. Still open.

**Milestones** (in dependency order; the baseline comes first, to prove the problem reproduces before anything else):

1. M0 contract and skeleton: OpenAPI, the public Go client package, the fake backend, ledger unit tests.
2. M1 baseline: mini-agent (scripted brain, without mode), the agent loop and four tools, the radix task and script, the blast chamber, metric collection. Meets E1.
3. M2 Linux enforcement core: the cgroup2 backend, the pre-exec shim, the `_self` leaf layout and nesting, whole-group reclamation, escape prevention, fail-closed restart. Meets F1–F4, F12–F14.
4. M3 quotas and observability: admission without overselling, resizing, NEAR\_LIMIT, capacity shrinking and the safety net, metering, termination codes, audit, suggest(), `/metrics`, timelines and `subactl trace`. Meets F5–F11, F30–F32.
5. M4 agent tree and communication: the lead (pty and self-wrapping), identity by membership and subtree permissions, operator permissions, isolation between leads, the inbox, graceful termination, declarative policies, progress beacons and idle detection, `subactl top`. Meets F15–F29, F33.
6. M5 experiments: the comparison of groups B and C and the report; the real-brain demo. Meets E2, E3.
7. M6 cross-platform: the macOS monitor backend (originally also a Windows Job Object backend, now out of scope).

Implementation status: M0–M5 are done; M6 delivered the macOS monitor backend; Windows is out of scope. The actual order followed the table, and after M4 problems found during verification led to further work: running the daemon under systemd, no\_new\_privs, output written through the daemon, and more.

## Reserved for v2

The mechanisms below came up in discussion but are not needed for v1's six problems, or cost more than they bring, so they were moved out of v1. They come back only when experiments or real use show v1 is not enough.

| Mechanism | Why it is out of v1 | When to bring it back |
| --- | --- | --- |
| overselling + automatic pressure handling (applying pressure, pausing, choosing a victim on its own, a pressure ladder) | once it oversells, subactld must make policy decisions on its own and becomes a second decision-maker, and the system becomes unpredictable | when H3 shows utilisation is still not enough with adaptive envelopes |
| browser pool | Chrome is already confined in the subagent's acgroup and the hard limit holds it; a pool is only an optimisation, not a matter of correctness | when browser overhead becomes the main bottleneck of utilisation |
| scenario-specific resource classes (an exclusive display, GPU slots or GPU memory) | v1's scenarios have no such failures | when computer use or GPU tasks need orchestrating |
| cgroup v1 backend | needs a hierarchy kept per controller, and has no memory.oom.group, cgroup.kill or namespace containment check; the work is close to a separate backend; systemd 258 removed cgroup v1 support entirely, and new versions of mainstream distributions use only v2 | when the target users' main environment is still cgroup v1 |
| non-root deployment (systemd user manager + Delegate=yes) | no cgroup namespace and no separate uid, so agents can migrate within the delegated subtree and escape prevention falls to soft | when subactld cannot be deployed as root |
| pause / resume | maps to none of v1's six problems; memory is not freed while paused either | when CPU needs to be given up temporarily without losing progress |
| restarting without interruption (per-lease supervisor) | needs a resident conmon-style supervisor to hold the output and record exit status; v1 uses fail-closed instead | when the work lost to daemon restarts is unacceptable, or the shared failure domain of several leads must go |
| detaching and reattaching a lead | needs the pty kept without a client, and reconnection; in v1 closing the terminal reclaims | when leads need to run unattended for long |
| subactl-mcp, an MCP notification channel, a stdin channel | an LLM planner has a shell, and the CLI is enough; most MCP clients do not inject server notifications into the model's context; writing to an interactive harness's stdin mid-generation has no defined meaning | when a target harness can integrate only through MCP |
| semantic state queries | essentially a conversation between a planner and a child agent, which is the data plane; free text going upward is also an injection entry point | when progress beacons are not enough for a planner's decisions |
| OpenTelemetry export | timelines and Prometheus already cover v1's analysis needs | when an existing observability platform must be connected (planned for v1.1) |
| Python client | v1's callers are only the CLI and harnesses written in Go | when an orchestrator written in Python needs to integrate; generated from openapi.yaml |
| hard quotas on scratch and output | needs a per-lease file system or project quotas; disk is not among v1's six problems. Implementation: stdout and stderr are already written through the daemon and cut exactly at `output`; files an agent writes to disk itself still have no quota | when running out of disk becomes an actual failure |
| a configuration skill (probe / init / tune) | capacity is probed automatically when subactld starts, and envelope suggestions come from suggest() | when deployments are varied enough to need guided configuration |
| a Linux VM path for macOS | much work; v1 first labels macOS honestly as monitor\_only | when a main machine needs hard memory isolation |

**References**:

**Abbreviations**:

| Abbreviation | Full name | Meaning in this document |
| --- | --- | --- |
| acgroup | agent control group | this document's core abstraction: the resource boundary of a group of processes, which descendants join automatically. The name echoes cgroup, but it is cross-platform: implemented by cgroups on Linux, and by process groups and a membership table on macOS; see Terms |
| cgroup | control group | the Linux kernel's mechanism for grouping processes and applying resource limits, in two generations, v1 and v2; this document's v1 supports only v2 |
| cgroupns | cgroup namespace | makes a process treat a given cgroup as its root; on v2 it also stops it from migrating processes outside that root |
| pidfd | process file descriptor | a file descriptor referring to a process (Linux 5.3+), with which a process's exit can be watched without pid reuse races |
| pty | pseudo-terminal | gives a process without a real terminal an interactive terminal, whose input and output are relayed by the other end |
| SO\_PEERCRED | socket option: peer credentials | the option to read the peer process's pid, uid and gid on a Unix socket; SO\_PEERPIDFD (Linux 6.5+) returns the peer's pidfd directly |
| TUI | Text-based User Interface | a text interface that runs in a terminal |
| OTLP | OpenTelemetry Protocol | OpenTelemetry's transport protocol, the optional export format of v1.1 |
| CI | Continuous Integration | continuous integration |
| JSONL | JSON Lines | a log format with one JSON object per line |
| MCP | Model Context Protocol | a protocol for agents to call external tools |
| OOM | Out Of Memory | memory exhausted; the kernel's OOM killer kills processes to free memory |
| SSE | Server-Sent Events | one-way server event push over HTTP |
| VM | Virtual Machine | a virtual machine |
| VSZ | Virtual Set Size | all the virtual address space a process maps, including reserved parts not actually used; usually far larger than actual memory use, and no basis for resource decisions |
| working set | — | the memory basis on Linux: a cgroup's memory use minus reclaimable inactive file cache. For macOS's basis see Terms |

Note: the Claude Code issues cited here serve only as typical failures in real agent settings, showing that these problems do exist. They are not the basis of the design, and Claude Code is not a reference implementation; subactld's behaviour is defined by this document alone.

- [Claude Code #74001: CliGovernor under-counts and does not evict](https://github.com/anthropics/claude-code/issues/74001)
- [Claude Code #60488: escaping a systemd scope](https://github.com/anthropics/claude-code/issues/60488)
- [Claude Code #83412: a subagent hard-killed at the usage limit](https://github.com/anthropics/claude-code/issues/83412)
- [Claude Code #74006: rolling windows and background subagents](https://github.com/anthropics/claude-code/issues/74006)
- [Claude Code #50783: agent-browser's Chrome left orphaned](https://github.com/anthropics/claude-code/issues/50783)
- [Claude Code #47827: subagent processes left behind](https://github.com/anthropics/claude-code/issues/47827)
- [OpenClaw #136882: a proposal for resource-aware execution](https://github.com/openclaw/openclaw/issues/136882)
- [AgentCgroup (arXiv 2602.09345)](https://arxiv.org/abs/2602.09345)
- [Google Scion settings example](https://github.com/GoogleCloudPlatform/scion/blob/main/settings.yaml.example)
- [About Agent Substrate on GKE](https://docs.cloud.google.com/kubernetes-engine/ai-ml/about-agent-substrate)
- [cc-reaper](https://github.com/theQuert/cc-reaper)

## Implementation record (2026-11-17)

Places marked "(implementation)" in the text already describe what was built. They are summarised here by category, for comparison with the original design.

**Scope**
- Linux and macOS only. The macOS monitor backend is delivered; Windows, originally planned for v1.1, is out of scope. On any other system the build fails at once and says why; the implementations of pty, peer identity and total memory that used to return an error quietly on other systems are gone.
- YAML is parsed by a strict-subset parser of our own and Parquet written by a minimal hand-written writer, to keep zero third-party dependencies.

**API and CLI**
- Sizes are transmitted as integers in bytes; `{id}` may be written `self`; error bodies carry the faulty field, the expected value and an example; an unknown field rejects the whole request.
- New refusal reason `below_allocated`, new 403 `self_resize_forbidden`.
- `wait-children` gains `self` and `timeout_seconds`; `trace` takes `?tree=true`; `available` takes `?kind=`; `kill`, `terminate`, `notify` and `spawn` all support `dry_run`.
- The lease object gains `scratch` and `activity` fields, and usage gains `oom_events`.
- attach uses `Upgrade: subactl-pty` and its own frame format; terminal output before the attach is buffered up to 64KB.
- `subactl run` without `-it` shows the lead's output live and writes the termination record to stderr.

**Termination codes and attribution**
- A non-zero exit is 520 (only exit status 0 is 200); hitting a memory limit is attributed by `oom` in `memory.events.local` to the level whose limit was hit.
- `memory.oom.group` is set only on a lease's `_self/`, so the kernel (above all the capacity guard) ends one lease's processes at a time, and subactld ends the subtree at the level whose limit was hit.
- The evidence's `top_procs` is collected beforehand, once use reaches 75% or is about to hit the limit, because after the kernel kills the processes there is nothing left to list.
- acgroups cleaned up fail-closed produce no termination record; a daemon stopped normally reaps every lease with 500; incomplete reclamation is 504.

**Security and deployment**
- The shim keeps the user's supplementary groups and sets no\_new\_privs, blocking privilege escalation through sudo (which a real model actually tried).
- Identity is established once per connection, with pidfd on 6.5+ kernels to rule out pid reuse.
- systemd unit: the leases live under the cgroup delegated to the service, the daemon moves itself into a `_daemon/` leaf; when the daemon dies, systemd at once cleans up every lease.

**Quotas and observability**
- Output is written through pipes from the daemon instead, stopped at the limit and not counted in the lease's memory; the original design's "check the file size every second" would let an output flood fill memory first and be misjudged as 413.
- Suggested envelopes count only leases that ended 2xx; otherwise they rise with every 413.
- NEAR\_LIMIT is pushed once per crossing, re-armed only after use falls below 85%.
- After each lease it ends, the safety net again requires three low readings in a row; within a tree it chooses by `kill_order`, then the largest leaf.
- macOS's effective capacity comes from `kern.memorystatus_level`; the reserve line defaults to 25% (15% on Linux), and limits are checked every 200 milliseconds, everything else still every second.

**mini-agent and the experiments**
- A child's self-wrapping does not exec but keeps a waiting process, to retry when the quota is short and to let the exit status report whether the task succeeded.
- The real brain supports Anthropic and the compatible endpoints of DeepSeek and Z.AI; it does not retry at a provider's quota limit.
- The radix task's goal became "React 19 at run time, types 18"; Storybook 7 needs the react-dom-shim alias; screenshot differences are only recorded, not counted as failures.
- G6 applies to a quarter of the workers only; the blast chamber is an outer cgroup in a privileged Docker container.
- The actual definitions of the experiment metrics are in "Comparison experiments"; F27's CPU bound is met on a physical machine and cannot be measured on virtual machines.
- The comparison's conclusions rest on round 4 on the physical machine: H2 holds; H1 fails in 1 of group A's 5 runs (the OOM in the chamber killed the orphans as well); H3 is refuted; H4 holds but untested. Groups B, C and L left no orphans and never reached the chamber's limit in 15 runs.
- Round 5 (`results/h4/`) tests H4: interference takes 75% of the chamber, arriving while workers run. Group B's safety net ended 1 lease per run, C's 2, always after a warning, with no orphans and the chamber's limit never reached; C's envelopes are tighter, so ending one does not free enough to get back above the reserve line, and H4 is refuted for C. At 50% interference the admission freeze alone was enough and no lease was ended. The first run of group C found and fixed the defect of `memory.oom.group` on lease directories, through which a hit of the guard ended a whole lead tree.
- Two follow-up experiments are done: DeepSeek as the planner (group L, 24% completion on average, below B and C); and the real-model check with DeepSeek as lead and workers (H1 and H2 hold likewise; round 5 added one run of A for its per-second data). Round 5's informed group L gave the same envelopes as group C and still completed only 22% on average: the bottleneck is dispatch speed, not envelopes.
- New capacity guard: `memory.max` on the parent cgroup of all leases stops bursts faster than the safety net's sampling; attribution of 413 now uses each level's own `oom` count.
