[English](tutorial.md) | 简体中文

# 在 subactl 之上构建你自己的 multi-agent harness

本教程构建一个小型编排器：一个 lead 把任务分给 worker agent，在自己的内存允许的范围内同时跑尽可能多的 worker。它展示了自己的 harness 需要的各个部分：启动子 agent、确定它们的配额、收集结果、根据每个子 agent 的结束方式做出反应，以及与运行中的 agent 树通信。代码在 [`examples/orchestrator`](../examples/orchestrator)：lead 有 shell 脚本和 Go 两个版本，还有一个故意捣乱的 worker。[基准测试](benchmark.zh-CN.md)用的 harness [mini-agent](../examples/mini-agent) 是同样思路的完整版。

## subactl 为 harness 做了什么

- **每个 agent 都是一个 lease**：一组进程，带一个内存上限（它的 *envelope*）。子节点的 envelope 从父节点的里面划出来，所以 lead 的 envelope 框住了整棵树。
- **启动一个子 agent 只需一次调用**：`subactl spawn`，或 `client.Acquire` 加 `client.Spawn`。daemon 在 lease 里启动进程；它再启动的一切也都留在里面。
- **lease 结束时，其中的一切都结束**，不管它的进程是怎么启动的（`nohup`、`setsid`、`&`），它下面的 lease 也一样。你的 harness 不需要写清理代码；lead 死掉，整棵树随之而去。
- **每个 lease 结束时恰好产出一个终止码**，说明原因：200 完成，413 超出内存上限，408 超出运行时长，503 机器内存不足，520 崩溃或被外部杀掉，等等。
- **身份来自进程本身。** 从 lease 内部发起的调用就以这个 lease 的身份执行；不需要传递任何 token。一个 lease 只能操作自己的子树。

留给你的是：跑什么、给多少、失败了怎么办。subactl 只校验、执行和报告，从不替你做决定。

## 1. 启动 daemon

```sh
make build
export PATH=$PWD/bin:$PATH

# Linux：以 root 运行，或作为 systemd 服务运行（见 README）。
sudo bin/subactld --socket-group "$(id -gn)" &

# macOS：以你自己的用户运行。上限每 200 毫秒检查一次，而不是由内核强制；
# 见 README 的“平台”一节。
bin/subactld &

subactl host     # 容量、已分配的额度、backend 能强制执行什么
```

daemon 和 CLI 都通过 `SUBACTL_SOCK` 找到 socket；不用默认路径时，两边设成同一个路径。

## 2. 十五行 shell 写一个 lead

lead 就是用 `subactl run` 启动的任何程序。在它里面，`SUBACTL_LEASE_ID`、`SUBACTL_SOCK` 和 `SUBACTL_SCRATCH` 都已设好，每一次 `subactl` 调用都以这个 lease 的身份执行。[`fanout.sh`](../examples/orchestrator/fanout.sh) 的核心：

```bash
run_one() {
  local task=$1
  while :; do
    subactl spawn --wait --kind worker --memory "$(subactl suggest worker 2>/dev/null)" --wall 5m \
      -- "$here/worker.sh" "$task" >"$out/$task.json" 2>"$out/$task.err"
    local rc=$?
    if [ $rc -eq 1 ] && grep -qE '"reason": *"(parent_budget|mem_budget|capacity_shrunk)"' "$out/$task.err"; then
      sleep 1; continue          # 还没有余量：等一下再申请
    fi
    return $rc
  done
}
for task in "$@"; do run_one "$task" & done
wait
```

- `spawn --wait` 启动一个子 agent 并阻塞到它结束，打印它的终止记录（JSON）。子 agent 以 2xx 结束时退出码为 0，否则为 3；申请被拒绝时为 1。
- 因余量不足被拒绝是正常的：在某个运行中的子 agent 结束之前，lead 的 envelope 是满的。subactl 不排队；由 planner 等待并重新申请。
- `subactl suggest worker` 给出以往 worker 峰值的 95 分位乘以 1.2（样本不到五个时为 2GiB）。各类工作就这样从自己的历史里学习；`--kind` 给这一类命名。
- `--wall 5m` 终止跑得太久的子 agent（408）。`--output 100M` 会限制它的输出量（414）。

worker [`worker.sh`](../examples/orchestrator/worker.sh) 完成自己的任务，把结果写进自己的 scratch 目录并上报：

```bash
subactl beacon --state working --tool "$task"   # 进度信标；不消耗任何模型调用
...                                              # 干活
echo "..." > "$SUBACTL_SCRATCH/result.txt"
subactl done --result "$SUBACTL_SCRATCH/result.txt"
```

它的任务 `leak` 会无限分配内存，任务 `server` 会在后台留下一个“dev server”，就像 agent 常做的那样。运行：

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

lead 有 4GiB，每个 worker 默认 2GiB，所以同时跑两个。`leak` 撞上自己的上限被终止，没有影响其他 worker；`server` 留下的 dev server 随它的 lease 一起被终止。

## 3. 用 Go 写同一个 lead

[`main.go`](../examples/orchestrator/main.go) 通过 Go client 做同样的事，并加上对每种结束方式的处理策略：

```go
c := client.New(client.SocketFromEnv())   // 身份来自本进程

av, _ := c.Available(ctx, api.Self, "worker")            // 剩余余量和建议配额
ls, err := c.Acquire(ctx, api.AcquireRequest{            // 不填 Parent：本 lease 的子节点
    Kind:     "worker",
    Envelope: api.Envelope{Memory: av.Suggest["worker"].Memory, Wall: 300},
})
var refusal *api.Error
if errors.As(err, &refusal) && refusal.Reason == api.ReasonParentBudget { /* 等一下，再申请 */ }

c.Spawn(ctx, ls.ID, api.SpawnRequest{Argv: []string{worker, task}, Env: os.Environ(), Dir: cwd})
res, _ := c.WaitChildren(ctx, ls.ID, api.WaitChildrenRequest{Self: true})  // 它的终止记录
```

`runTask` 里的策略演示了分工中属于 planner 的那一半：超出上限的 worker（413）用两倍内存再试一次；被兜底终止的（503）稍后重试，因为出问题的是机器而不是任务；其余的就按它结束的样子保留。

```console
$ go build -o bin/orchestrator ./examples/orchestrator
$ subactl run --memory 4G -- bin/orchestrator -worker examples/orchestrator/worker.sh a b c leak server d
leak: 413 OOM_LEASE_LIMIT, peak 1339 MiB of 8 MiB
b: 200 COMPLETED, peak 2 MiB of 4 MiB
...
```

这次运行在 shell 版之后，所以 `suggest` 已有五个样本，这些 worker 的峰值约 2MiB，于是建议值从默认的 2GiB 变成了 4MiB。`leak` 那一行是在 macOS 上记录的：那里每 200 毫秒检查一次上限，一个每一步都把内存翻倍的进程，在被发现之前会远远越过一个很小的上限。在 Linux 上，内核会把它挡在上限处。

## 4. 子 agent 再启动子 agent

worker 也可以当 planner：在它的 lease 里运行 `subactl spawn`，启动的就是这个 worker 的子节点，配额从 worker 的 envelope 里划出。其他一切照旧。要为父节点自己的进程保留一部分 envelope，给它 `--memory-self`；它的子节点就只能用剩下的部分。

## 5. 与运行中的 agent 树通信

| 你想要 | planner 一侧 | 子 agent 一侧 |
| --- | --- | --- |
| 知道它是否活着、在做什么 | `subactl ls`、`subactl trace LEASE` | 每次工具调用前 `subactl beacon --tool NAME` |
| 拿到它的结果 | 从它的 `DONE` 事件读路径，或读它的 scratch 目录 | `subactl done --result FILE` |
| 让它停下 | `subactl terminate LEASE --grace 30s` | 从 inbox 读到 `TERMINATE_REQUESTED` 后保存、退出 |
| 给它更多内存 | `subactl resize LEASE --memory 6G` | `subactl resize-request --memory 6G` 向父节点申请 |
| 自动扩容 | `spawn --grow-to 6G`：收到 NEAR\_LIMIT 时扩一次，前提是有余量 | 无需操作 |

报告了 `done` 却还活着的子 agent，60 秒后被回收（410）；最后一个信标之后静默 30 分钟的也会被回收。`terminate` 先通知，再发 SIGTERM，最后强杀；终止记录会写明在哪一步结束。

**如果你的 agent 是 LLM**，在 agent 循环里加一个小 adapter，就像 mini-agent 在 [`agent/adapter.go`](../examples/mini-agent/agent/adapter.go) 里做的：

- 每次工具调用之前，`c.Progress(ctx, api.Self, api.ProgressRequest{State: "working", Tool: name, Seq: n})`；
- 订阅 `c.Inbox(ctx, api.Self, ...)`，在下一次工具调用或下一轮对话时，把收到的消息以固定模板作为系统消息注入，例如 `[subactl control message from <source>] <type> "<reason>"`；
- 收到 `TERMINATE_REQUESTED` 之后，不再执行新的工具；
- 任务完成时，`api.ProgressRequest{State: "done", Result: path}`。

只有 subactl 和这个 agent 的祖先能向它的 inbox 写消息，消息类型也是封闭的集合，所以这个通道不能被用来注入任意文本。

## 6. 让模型来规划

把 [subactl skill](../skills/subactl/SKILL.md) 给模型（`npx skills add zydo/subactl --skill subactl`）：一页关于派生、确定配额、结束以及终止码的说明。然后给它一个循环，而不是让它每回合派一个 worker。在基准测试里，一个手工规划、每回合一次 bash 调用的模型，每次只启动 11 到 35 个 worker，而一个简单的循环启动 80 到 95 个；即使配额相同，它完成的也不到循环的一半；它最好的一次，是它自己写了一个派发脚本。像 `fanout.sh` 这样、由模型带着自己的任务列表启动一次的脚本，就是这个循环。告诉模型：用立即返回的命令启动子 agent（`... >log 2>&1 &`），在之后的调用里查看它们，绝不在一次 bash 调用里等它们结束：一个会在超时时终止命令进程组的 harness（mini-agent 就是这样），会把在这个命令里启动的子 agent 一并终止，不论是不是后台启动的。

## 7. 观察

```sh
subactl top                 # 所有 lead 的树、用量与配额、状态、最近的事件
subactl ls                  # 你的子树，JSON 格式
subactl trace LEASE --tree  # 用量随时间的变化、事件、信标，以及 "spikes"：
                            # 内存增长达到 64MiB 的那些工具调用
subactl metrics             # Prometheus 文本
```

## 检查清单

- 用 `subactl run` 运行 lead，不要在它之外启动任何东西：用其他方式启动的进程不在树里，也不计入账本。
- 用 `subactl suggest KIND` 确定子 agent 的配额，并给每类工作一个自己的 kind。
- 把 `parent_budget`、`mem_budget` 和 `capacity_shrunk` 当作“等待”，绝不当作失败；不要在紧凑的循环里重试。
- 把结果写成 `$SUBACTL_SCRATCH` 下的文件；在父节点结束之前，它都能读取子节点的 scratch 目录。
- 按终止码决定怎么处理；终止码说明了问题出在谁那边（4xx 是任务的，5xx 是环境的）。
- subactl 不对文件和网络做沙箱，也拦不住由第三方代 agent 启动的进程（`systemd-run --user`、tmux server、docker）。见[设计文档](design.zh-CN.md)中的非目标。
