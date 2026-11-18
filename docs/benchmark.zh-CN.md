[English](benchmark.md) | 简体中文

# 基准测试

subactl 是对着一个自己的基准测试开发的：一个小型 multi-agent harness `mini-agent`，接一项真实的工程任务，分别在用和不用 subactl 的情况下，放进一个封闭的“爆炸舱”里运行并测量。本文说明任务是什么、harness 如何表现、一次运行如何设置和测量，以及各轮运行得出了什么。代码在 [`examples/mini-agent`](../examples/mini-agent)；测量数据、报告和图表在 [`examples/mini-agent/bench/results`](../examples/mini-agent/bench/results)。

## 任务：把一个真实的 UI 库升级到 React 19

agent 要把广泛使用的 React 组件库 [radix-ui/primitives](https://github.com/radix-ui/primitives) 升级到 React 19。

- **起点。** 仓库位于 commit `af1e3b4`，即上游 pull request [#2934](https://github.com/radix-ui/primitives/pull/2934)（“React 19 compatibility”）的父 commit：Node 18 上的 yarn workspace，`packages/react/` 下有 54 个包，类型检查是全仓库一次的 `tsc`，测试用 jest，组件 stories 用 Storybook 7。
- **工作内容。** lead agent 列出这些包，每个包派一个 worker。每个 worker 有自己的 git worktree，把该包升级到 React 19，运行类型检查和该包的测试，再起一个 Storybook dev server，用无头 Chromium（经由 Playwright）渲染该包的 stories，确认它们仍能正常显示。检查失败时，worker 在自己下面派一个 fixer，fixer 还可以再派一个：最多三层。
- **完成的标准：** 该包没有类型错误，jest 测试通过，stories 渲染时控制台没有报错。
- **内存花在哪。** 类型检查很快（约 6 秒）；分量在 Storybook 的 webpack 构建加一个浏览器，每个 worker 约 1.7GiB。好几个 worker 会同时抢 Storybook 的 9009 端口，而“先起来看看效果”的 dev server 往往一直不关。这正是基准测试要关注的压力，它来自任务自身的工具，而不是模拟出来的。

任务环境（克隆的仓库、只安装一次再硬链接到各个 worktree 的依赖、Playwright、基线截图）由 [`scenarios/files/radix/prepare.sh`](../examples/mini-agent/scenarios/files/radix/prepare.sh) 一次性准备好。agent 调用的各个步骤是旁边的小脚本：`setup.sh`、`typecheck.sh`、`test.sh`、`preview.sh`、`fix.sh`、`cleanup.sh`。

## harness：mini-agent

`mini-agent` 是用 Go 写的精简 multi-agent harness，只用 subactl 的公开 client 和 CLI 构建。它给模型四个工具：`read`、`write`、`edit`、`bash`，没有专门派生子 agent 的工具：agent 在 bash 里运行 `mini-agent --task ...` 来启动子 agent。在 shell 里直接运行时，它成为 lead；在 subactl 下，每个子 agent 有自己的 lease，嵌套在父节点的 lease 里。

同一套循环、工具和 harness 后面有两个“大脑”：

- **scripted**：一份带种子的剧本，按场景表演。运行结果确定，不花钱，各组可以逐次对比。对照实验用它。
- **real**：任何通过 Anthropic Messages API 提供的模型，可以是服务商自己的端点，也可以是兼容端点（`--provider`、`--base-url`、`--model`；密钥从环境变量读取）。它自己读报错、改代码、跑检查。

`--mode without` 把 agent 作为普通子进程运行，主流 harness 就是这样做的；`--mode with` 让它们在 subactl 下运行。除此之外没有任何差别。

## 贪婪行为

剧本会打开六种 agent 在实际中会表现出的行为，每一种都取自某个真实 coding-agent harness 的故障报告。真实模型也会自发地表现出其中几种：在下面的真实模型运行中，worker 没人指示就把 dev server 留着不关。

| | 行为 | 在这个任务里的形态 |
| --- | --- | --- |
| G1 | 扇出过度 | 每个包一个 worker，最多同时 32 个，fixer 最深三层 |
| G2 | 起了服务和浏览器不关 | dev server 和 Chromium 启动后从不停止 |
| G3 | 上下文无限增长 | 每个 agent 的 transcript 缓冲每一步增长 8MB，从不释放 |
| G4 | 异常路径不清理 | 撞上用量额度时，harness 只杀每个 subagent 的 pid，不管它启动的东西 |
| G5 | 输出失控 | 一个 worker 卡在交互式提示上，无限往 stdout 写 |
| G6 | 无界并行 | 四分之一的 worker 以核数的四倍并行跑类型检查和测试，同时做无界的文件遍历 |

## 一次运行

每次运行都按同一个时间表进行，来自 [`scenarios/radix.yaml`](../examples/mini-agent/scenarios/radix.yaml)：

| 时间 | 事件 |
| --- | --- |
| 2 分钟 | 开始扇出（G1） |
| 5 分钟 | 模型服务商的用量额度耗尽：harness 只杀正在运行的 subagent 的 pid（G4） |
| 7 分钟 | 一个不是 agent 的进程占用机器的 30%，持续四分钟 |
| 9 分钟 | 开始输出失控（G5） |
| 15 分钟 | 场景结束；之后再观察舱 60 秒 |

**爆炸舱。** 不用 subactl 时，目的就是把机器压垮，所以每次运行都放在一个外层 cgroup 里，上限 12GiB，位于一个特权 Docker 容器中。“宿主内存耗尽”指的是舱撞上自己的上限。用 subactl 时，daemon 在舱内运行，把舱当作整台机器，预留线为 15%。

**分组。** 各组使用同一份剧本、同一个种子、同一台机器：

| 组 | 设置 | 要回答的问题 |
| --- | --- | --- |
| A | 不用 subactl | 问题能复现吗？ |
| B | subactl，固定配额（lead 9GiB、worker 3.5GiB、fixer 1GiB） | 不超卖加硬限能保证安全吗？ |
| C | subactl，配额取自 `subactl suggest`（历史峰值的 95 分位乘以 1.2），收到 NEAR_LIMIT 时扩容一半，历史跨运行累积 | 根据测量调整，能既安全又更好地利用机器吗？ |
| L | 同 C，但 lead 是一个真实模型，由它决定每个 worker 的配额和同时运行几个；worker 仍是 scripted | 模型规划内存能像规则一样好吗？ |

**测量什么。** 舱的内存峰值；舱撞上自己上限的次数；结束 60 秒后残留的进程及其占用的内存；完成的包所占比例；因被终止而丢失的工作；舱的平均使用率；兜底终止出手的次数；以及运行耗时。`bench report` 把测量结果生成报告、图表，以及逐秒序列的 Parquet 文件。

**假设**，在运行之前确定，由 `bench report` 中的代码判定：

1. **H1。** 撞上用量额度、harness 杀掉 subagent 之后，A 组会留下进程；B、C 两组从不会。
2. **H2。** A 组在五次运行中至少一次撞上舱的上限；B、C 两组从不会。
3. **H3。** C 组的机器平均使用率比 B 组至少高 15 个百分点，且丢失的工作不多于 B 组。
4. **H4。** 在外部干扰下，B、C 两组每次运行最多需要兜底终止一次，并且兜底终止出手之前 planner 都已收到警告。

## 结果

物理 x86-64 机器（24 核，60GB），Linux 7.0，12GiB 爆炸舱；机器的详细说明见 [testing.zh-CN.md](testing.zh-CN.md)。每一行是该组各次运行的均值或范围。基准测试只能在 Linux 上运行：它的舱是一个 cgroup。

### 第四轮：对照（每组五次）

| 组 | 完成的包 | 残留进程 | 撞上舱的上限 |
| --- | --- | --- | --- |
| A，不用 subactl | 4% | 38、55、0、12、116 | 每次都撞 |
| B，固定配额 | 41% | 每次都是 0 | 从未 |
| C，建议配额 | 53% | 每次都是 0 | 从未 |
| L，模型担任 planner | 24%（11% 到 39%） | 每次都是 0 | 从未 |

| | 判定 | 原因 |
| --- | --- | --- |
| H1 | 否证，A 组有一次不满足 | A 组第三次运行中，内核的 OOM killer 杀掉了 2540 个进程，孤儿也在其中，所以计数时已无残留；B、C、L 三组从未留下任何东西 |
| H2 | 成立 | |
| H3 | 否证 | C 组的使用率只比 B 组高 4.3 个百分点，不是 15，而且 C 组丢失的工作更多（21.4 对 15.2 个子任务）：更紧的配额让更多 worker 同时运行、完成更多，也让贪婪的 worker 更常撞上自己的上限 |
| H4 | 成立，但未受检验 | 干扰从未把 B、C 两组压到预留线以下 |

完成率用来比较各组，并不衡量任务本身。15 分钟内、每次两三个 worker，放不下全部 54 个包；而且有一个包 `slot` 在 React 19 下每次都失败。

### 第五轮：真正加压下的 H4，以及更知情的 planner

**H4** 重新做了一次，外部干扰占舱的 75%，并且在 worker 运行时到来，迫使兜底终止出手（[结果](../examples/mini-agent/bench/results/h4/README.md)）：

| 组 | 完成的包 | 每次运行兜底终止的次数 | 出手前已警告 | 残留进程 | 撞上舱的上限 |
| --- | --- | --- | --- | --- | --- |
| B | 39% 到 43% | 1 | 每次 | 0 | 从未 |
| C | 48% 到 59% | 2 | 每次 | 0 | 从未 |

H4 对 B 组成立，对 C 组被否证：C 组配额更紧，终止一个 worker 腾出的内存太少，于是还要再终止一个。在 50% 的干扰下根本不需要终止：机器一不够用，subactl 就停止接纳新 lease，正在运行的会自己结束。这一轮还发现并修复了一个 bug：内核在所有 lease 之上的护栏撞限时，会终止整个 lead 连同它的 worker，而不是只终止一个 worker。

**模型担任 planner，并被告知如何确定配额**（[结果](../examples/mini-agent/bench/results/L-informed/README.md)）：它随后给每个 worker 恰好分配建议配额，与 C 组一致，但完成率仍只有 22%（9% 到 39%）。限制在派发而不在配额：B、C 两组每次启动 80 到 95 个 worker，模型只启动 11 到 35 个，每回合一次 bash 调用，回合之间隔几秒到几分钟。它最好的那次，是它自己写了一个派发脚本并让脚本去跑。

### 由真实模型来干活

lead 和每个 worker 都由真实模型担任，自己动手改代码，处理其中八个包（[结果](../examples/mini-agent/bench/results/real-model/README.md)）：

| 组 | 运行次数 | 完成的包 | 残留进程 | 它们占用的内存 | 撞上舱的上限 |
| --- | --- | --- | --- | --- | --- |
| A，不用 subactl | 3 | 每次 8/8 | 153、84、141 | 4.25、1.66、2.94GiB | 3938、78563、9432 次 |
| B，用 subactl | 2 | 每次 8/8 | 0 | | 从未 |

模型自己把 Storybook 和 esbuild 留着不关，并没有剧本叫它这样做。H1 和 H2 在真实模型下与在剧本下一样成立。

### 这些结果不能说明什么

- 舱是一个 cgroup，不是一台独立的机器。
- 一个任务，一台机器，每组五次（真实模型两三次）；方向一致，数值不是精确估计。
- 模型担任 planner 的结果来自一个模型、一份提示。
- 每一轮怎么跑的、出过什么问题、重做了什么，以及完整的表格，见[结果说明](../examples/mini-agent/bench/results/README.md)（英文）和设计文档的[对照实验](design.zh-CN.md#对照实验)一节。

## 运行

```sh
make build

# toy 场景不需要任何工具链，在哪都能跑。
bin/mini-agent --mode without --scenario examples/mini-agent/scenarios/toy.yaml

# radix 任务，某一组跑五次，放在上限 12G 的 Docker 舱里。
# 需要 Docker 和网络；第一次运行会准备任务环境，需要几分钟。
examples/mini-agent/bench/chamber/run.sh A examples/mini-agent/scenarios/radix.yaml 5 /tmp/bench/A
examples/mini-agent/bench/chamber/run.sh B examples/mini-agent/scenarios/radix.yaml 5 /tmp/bench/B
examples/mini-agent/bench/chamber/run.sh C examples/mini-agent/scenarios/radix.yaml 5 /tmp/bench/C
bin/bench report --in /tmp/bench

# L 组，或任何一组用真实模型做 lead：通过 BENCH_ARGS 传入大脑，
# 密钥放在环境变量里。
BENCH_ARGS="--brain real" examples/mini-agent/bench/chamber/run.sh L \
  examples/mini-agent/scenarios/radix-llm-planner.yaml 5 /tmp/bench/L
```

其他场景有 `radix-real.yaml`（真实模型担任 lead 和 worker，八个包）、`radix-h4.yaml`（第五轮的干扰）、`radix-llm-planner-informed.yaml` 和 `radix-smoke.yaml`。
