# Subagent Controller：单机 subagent 资源管理设计

Nov 4, 2026 · @Karl · 2026-11-17 按 v1 实现修订：凡与最初设计不一致或实现中改定的地方，正文已改为实际做法，改动处标注“（实现）”，汇总见文末“实现记录”。

Subagent Controller 是单机上的 subagent 资源控制器，由 Go 编写的 daemon `subactld` 和 CLI `subactl` 组成；配套的演示 harness `mini-agent` 用 Go 编写。v1 的核心是两条承诺：维护一个不超卖的配额账本；保证一个 acgroup 结束时，其中的进程全部终止。在此之上，subactl 是创建 agent 的唯一入口：人在自己的 shell 里用 `subactl run -it` 启动 lead agent，agent 在 bash 工具里用 `subactl spawn` 派生子 agent，两者构成同一棵 agent 树；subactl 也是 planner 与子 agent 之间唯一的控制面通道。分配多少、何时扩容、终止谁，这些策略都属于 planner；subactld 只负责校验、执行和报告。名字里的 Controller 指控制工具（同 systemctl 的 ctl），不是 K8s 意义上主动 reconcile 的 controller。

## 背景与定位

触发问题：在单机上做多 agent 编排时，subagent 常常经由异常路径终止，比如被外部 kill、进程崩溃或上游服务中断。它拉起的重资源子进程（如浏览器）不会被回收。与此同时，主流 agent 对 subagent 的约束只有"数量上限"，没有"资源感知"（见下文的现状调研）。两者叠加，最终会耗尽整机内存。这类问题在主流 coding agent 中反复出现，社区目前只有事后清理的 reaper 脚本。集群侧的 agent 编排（如 Google 的 Scion / AX / Agent Substrate）面向容器和 Pod，场景与单机进程树不同，只作参考，不拿来对比。

**现状调研：主流 agent 用数量上限代替资源感知**（2026-11 调研。Codex、OpenCode、Pi 基于源码，分别为 openai/codex@b016cd1、sst/opencode@d432a06、earendil-works/pi@c3cc916；Claude Code 未开源，基于官方文档）：

|  | 并发上限 | 嵌套深度 | 派发前是否看内存/CPU 余量 | 进程清理 |
| --- | --- | --- | --- | --- |
| Claude Code | 每个 session 默认 20 个同时运行（`CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS`，v2.1.217 起）；ultracode 豁免；恢复已结束的子 agent 不检查上限 | 默认 3 层 | 否，文档未提及 | 文档承认后台子 agent 可能让后台 Bash 命令在它的回合结束后继续运行 |
| Codex | v1 默认 6 个线程；v2 默认每个 session 4 个（含主线程） | v1 默认 1 层 | 否 | 命令用 setsid 起进程组并整组终止；父进程死亡时发 SIGTERM 仅在 Linux 上生效，且只作用于直接子进程；v2 会卸载闲置的子 agent，是四家中唯一的闲置回收 |
| OpenCode | 未发现并发计数上限；子 agent 总是在后台运行，调用立即返回 | 默认 1 层（`experimental.subagent_depth`） | 否 | 命令进程组整组终止，SIGTERM 后 3 秒补 SIGKILL |
| Pi | 核心有意不提供子 agent；官方示例扩展最多 8 个任务、同时 4 个 | 示例未限制 | 否 | bash 工具整组终止；但示例扩展只杀子 pi 进程本身，它的命令进程组独立（detached），子 pi 被硬杀后这些命令会变成孤儿 |

结论：

- **无上限的扩出已经不是主流现状。** 各家都有某种上限；Claude Code 的并发上限是 2026 年 7 月才加上的，理由是避免一条消息扩出无上限的后台 agent，说明此前这是真实发生过的问题。
- **数量上限不是资源感知。** 上限是写死的常数，从嵌入式设备到顶配工作站一视同仁：20 个子 agent 在 128GB 的工作站上可能很保守，在 8GB 的笔记本或开发板上则足以压垮机器。上限也不区分子 agent 在做什么：20 个各自编译或开 Chrome 的子 agent，与 20 个只读文件的子 agent 在上限看来是一样的。
- **上限按 session 计算，彼此不可见。** 多个 session、多个终端窗口、不同 agent 混用时，机器上的总量没有人管。
- **上限是由 agent 自己执行的软约束。** 可以调高，也有豁免和漏洞（ultracode、恢复子 agent 不检查上限）。
- **进程清理只覆盖自己知道的那一层进程组。** 子 agent 被硬杀后的孙进程、非 Linux 平台、经 setsid 或 nohup 脱离的进程，都不在清理范围内。

这就是 subactl 存在的价值：它不与这些数量上限竞争，而是补上它们缺的两样东西：按本机实际容量、以实测用量计量的跨 session 全局账本，以及按进程归属保证的整组回收。

来源：[Claude Code 子 agent 文档](https://code.claude.com/docs/en/sub-agents)；[Codex config/mod.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/config/mod.rs)、[Codex process\_group.rs](https://github.com/openai/codex/blob/main/codex-rs/utils/pty/src/process_group.rs)、[Codex residency.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/agent/control/residency.rs)；[OpenCode subagent.ts](https://github.com/sst/opencode/blob/dev/packages/core/src/tool/plugin/subagent.ts)；[Pi README](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/README.md)、[Pi subagent 示例](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/examples/extensions/subagent/index.ts)。

v1 要解决六个问题：

1. subagent 异常终止后，它的子孙进程残留。
2. 单个 subagent 占用过多资源。
3. subagent 的总占用失控，压垮整机。
4. planner 看不到各个 subagent 的实际占用，无从决策。
5. subagent 完成任务或无人管理后仍然存活，持续占用资源（闲置泄漏）。
6. planner 只能硬杀子 agent，无法让它先保存进度再退出。

设计原则：

- **策略与机制分离。** 分配多少、何时扩容、终止谁，由 planner 决定；subactld 只校验、执行、报告，自己不做任何策略判断（唯一例外是下文的兜底终止）。
- **安全性来自静态不变式，而不是反应速度。** 配额不超卖，加上每个 acgroup 的硬限，就能保证 planner 变慢或离线时机器仍然安全。
- **强制执行不依赖 agent 配合。** 边界要设在 agent 越不过去的地方。真实的 agent 可能主动跳出外部施加的边界，一个典型例子见 [anthropics/claude-code#60488](https://github.com/anthropics/claude-code/issues/60488)。
- **计量看实际占用。** 不看 VSZ（Virtual Set Size，进程映射的全部虚拟地址空间）；进程按 acgroup 归属统计，不靠 PPID 拼树。
- **每个机制都必须对应上面六个问题中的一个。** 对应不上的机制不进 v1。
- **平台差异如实标注。** 不静默降级；做不到的保证写进非目标，不写成 hard。

目标（v1）：

1. 交付 Go 编写的 daemon `subactld` 与 CLI `subactl`（含 `subactl top`、`subactl trace`）。契约只有 `api/openapi.yaml`，v1 只提供一个用标准库手写的公开 Go client 包，由 CI 对照契约校验一致性。整个项目只用 Go 标准库和官方扩展库 golang.org/x/sys、golang.org/x/term，不引入第三方依赖（实现：YAML 用自写的严格子集解析器 `yamlite`，实验数据的 Parquet 文件由手写的最小 writer 生成）。Linux（cgroup v2）是 v1 的主平台；macOS 做到 monitor 级别并已交付（实现）；Windows 推迟，不在 v1 内。cgroup v1 不在 v1 范围内。
2. 管理的资源维度：内存硬限、CPU、进程数、wall time、输出上限。
3. 提供不超卖的配额账本，并支持对运行中的 subagent 调整配额。
4. acgroup 结束时整组回收。结束的原因可以是主进程退出、父节点退出（包括 lead 的终端被关闭）或外部中止。
5. 维护 agent 树：lead 与子 agent 统一由 subactl 创建；子树权限、级联回收、优雅终止协议、闲置检测；控制消息经 subactl 双向投递；多个 lead 之间在资源和控制面上隔离。
6. 可观测：用量可以查询和订阅；提供 Prometheus 指标和 per-lease 时间线追溯；终止时给出统一的终止码；全程写审计日志。
7. 交付演示 harness mini-agent：Go 编写，作为 `examples/mini-agent` 下的独立 module，自行实现 agent 循环、四个工具和行模式 TUI，不依赖第三方库；配 scripted 与 real 两个大脑，在 radix-ui/primitives 的 React 19 升级任务上完成三组对照实验。real 大脑讲 Anthropic Messages API，可接 Anthropic 本身以及 DeepSeek、Z.AI 的 Anthropic 兼容端点（实现）。

非目标（v1）：

- 超卖，以及自动压力处理（施压、暂停、自主选择 victim）。
- 多机部署与 Kubernetes。
- 修复 agent harness 自身的内存泄漏。subactld 只限制它的爆炸半径。
- 文件系统与网络沙箱。因此，agent 通过 IPC 请第三方代为启动进程造成的逃逸不在 v1 防护范围内，例如 `systemd-run --user`、tmux server、docker daemon、Windows 的 WMI 或计划任务：新进程不是 agent 的后代，通常也不继承它的环境变量。同一目录下的多个 agent 同时修改同一文件也不在防护范围内（实现中用真实模型观察到过，三个子 agent 同时改了同一个共享文件）。
- 磁盘配额。不设输出上限的 lease，其输出会无限制地写进 scratch 目录。
- subactld 自身崩溃后保持 lease 继续运行。v1 采用 fail-closed，见"核心模型"。
- cgroup v1、暂停与恢复。
- LLM 额度管理。额度耗尽按外部中止处理，终止码为 520 EXTERNAL\_ABORT。
- Windows（Job Object backend）。原计划 v1.1，现推迟，未定期（实现）。
- 在同一进程内隔离单个 subagent。subagent 必须以独立进程运行。
- 第三方 harness 及其内部自行派生的 subagent。
- 其余推迟的机制见文末的"v2 预留"一节。

## 术语与跨平台映射（规范）

核心层只使用本节定义的抽象术语；cgroup、Job Object、kqueue 这类平台机制只允许出现在 backend 中。每个 backend 必须为每项能力声明强制等级，任何平台差异都要显式暴露，不得静默降级。

**术语定义**：

| 术语 | 定义 |
| --- | --- |
| acgroup（agent control group） | 一组进程的资源边界。子孙进程自动归属于它；由 backend 创建和销毁。命名类比 Linux 的 cgroup，但它是跨平台抽象：在 Linux 上由 cgroup v2 实现，在 Windows 上由 Job Object 实现，并不等于 cgroup |
| acgroup 树 | acgroup 按 lease 层级嵌套，子 acgroup 的限额不超过父 acgroup。它是 agent 主从关系的唯一映射；OS 的父子进程关系不承担归属职责 |
| lease | 一个 agent 节点在 subactld 中的登记与配额凭证。每个 lease 对应一个 acgroup、一个主进程和一个终止码；状态为 admitted → running → reaping → reaped |
| envelope | 申请 lease 时提交的各维度上限：memory、memory\_self、cpu、pids、wall、output |
| lead | 一棵 agent 树的入口 agent，即没有 parent 的 top-level lease，由人用 `subactl run -it` 启动。不叫 root，以免与 Unix 的 root 用户混淆 |
| planner | 任何创建子 agent 的节点，包括 lead。负责语义与计划层面的决策 |
| holder | 一个 lease 存活所依赖的进程：嵌套 lease 的 holder 是父 lease 的主进程，lead 的 holder 是 `subactl run -it` 的 CLI 进程。holder 一死，其名下的 lease 级联回收 |
| operator | 不属于任何 acgroup 的调用方，即人或人运行的工具，按 uid 授权 |
| 归属（membership） | 判断某个进程属于哪个 acgroup 的唯一真相源，由 backend 提供。PPID 和进程组只作辅助。归属同时是调用 subactld 时的身份依据 |
| 有效容量 | 可供所有顶层 lease 分配的内存总量：min(配置上限, MemTotal − 预留线 − 外部占用)。外部占用指不由 subactld 管理的进程的内存；预留线是给系统留的余量，可用内存跌破它时触发兜底终止。预留线默认取 max(MemTotal × 15%, 8GiB)，但不超过 MemTotal 的一半（实现） |
| 内存口径（mem\_basis） | 内存用量的度量方式。三个平台口径不同，所有用量数据都必须带上口径标签：working\_set、commit、phys\_footprint |
| 内存硬限（mem.hard） | 超过后整个 acgroup 被终止。各平台达到这一语义的方式不同，见下表 |
| 整组终止（kill\_tree） | 终止 acgroup 内的全部进程，并验证剩余进程数为 0 |
| 终止码 | 每个 lease 结束时产出且只产出一个的结束原因编码，按 HTTP 风格分为 2xx 正常、4xx 超出自身配额或被主动终止、5xx 环境原因，见"终止码与审计" |
| harness adapter | 嵌在 agent harness 里的一小段代码，在工具调用与换轮时把 inbox 里的控制消息注入对话，并上报进度信标。没有 adapter 的 agent 只能用信号通信 |
| inbox | subactld 为每个 lease 维护的控制消息队列，只接受 subactld 自己和祖先 planner 的消息 |
| 进度信标 | adapter 在每个 hook 上主动上报的状态（working / awaiting\_input / done）、时间戳和工具名，用于闲置检测与 trace。没有信标不等于进程死亡 |
| fail-closed | subactld 崩溃或重启时不接管运行中的 lease，启动时把已有 acgroup 一律整组终止（500）；停机期间内核硬限照常生效 |
| 强制等级 | 分四档：hard 由内核保证；soft 尽力而为，可能存在时间窗口；monitor\_only 是事后检测再处置，有秒级延迟；unsupported 表示不支持 |

**各平台的实现**（格式为"机制 / 强制等级"）：

| 能力 | Linux（cgroup v2） | Windows（Job Object） | macOS 原生 | macOS + Linux VM |
| --- | --- | --- | --- | --- |
| acgroup | cgroup 目录；限额设在 lease 目录上，进程放在其叶子 `_self/` 中（见核心模型） | Job Object（Windows 8 起支持嵌套） | 进程组 + subactld 归属表 | VM 内的 cgroup |
| 归属 | 子进程自动继承 / hard | 子进程自动进入 job / hard | 每次采样时扫描进程表，按父子链和进程组登记；成员表落盘，供重启后的 fail-closed 清理 / soft（NOTE\_FORK 不带子进程 pid，NOTE\_TRACK 不可用） | hard |
| 内存口径 | working\_set：memory.current − inactive\_file | commit：私有提交内存 | phys\_footprint 之和 | working\_set |
| 内存硬限 | memory.max + memory.oom.group / hard | job memory limit（限的是 commit）。超限时只是分配失败、不终止进程，subactld 收到 JOB\_OBJECT\_MSG\_JOB\_MEMORY\_LIMIT 后调用 TerminateJobObject / hard | 每 200 毫秒检查一次（实现），超限即整组终止，记 413 / monitor\_only | hard |
| CPU | cpu.max / hard | CPU rate control / hard | monitor\_only | hard |
| 进程数 | pids.max / hard | active process limit / hard | monitor\_only | hard |
| 整组终止 | cgroup.kill / hard | TerminateJobObject / hard | 遍历归属表逐个 kill 并验证 / soft | hard |
| 父节点死亡检测 | pidfd / hard | 等待进程句柄 / hard | kqueue NOTE\_EXIT / soft | 跨 VM 心跳 / soft |
| 防逃逸（经 cgroupfs 或 breakaway 脱离） | cgroup namespace + 内核的迁移可达性检查 + no\_new\_privs（实现）/ hard | 禁止 breakaway / hard | unsupported。实现：按环境标记事后检测也做不到，macOS 的 `kern.procargs2` 对普通用户只给出其他进程的参数、不给环境变量（daemon 在 macOS 上以普通用户运行） | hard |
| 调用方身份 | SO\_PEERCRED → 所在 acgroup；6.5+ 另取 SO\_PEERPIDFD，查完归属后确认调用方仍存活 / hard | GetNamedPipeClientProcessId + IsProcessInJob / hard | LOCAL\_PEERPID → 归属表 / soft | hard |
| 宿主可用内存 | `/proc/meminfo` 的 MemAvailable；在受限的外层 cgroup 里可改用该 cgroup 的 memory.max 与用量（实现） | 可用物理内存 | `kern.memorystatus_level`，即内核内存压力机制认为可用的比例（实现） | VM 内的 meminfo |
| 交付阶段 | v1 | 推迟（实现） | v1（monitor，实现） | v2 |

**规则**：

1. 核心章节（核心模型、准入、API、终止码、planner 分工）只能引用本节的术语。平台机制名只出现在 backend 和本节中。
2. backend 启动时上报自己的能力矩阵，通过 `GET /v1/host` 对外暴露。acquire 中的 `require` 按强制等级匹配，达不到就返回 `enforcement_unavailable`。
3. 所有用量数据都随附 `mem_basis`。不同口径的数值不能直接相加或比较。
4. 某项能力缺失或等级较低时：内存硬限只有 monitor\_only（macOS 原生）时，不超卖只能近似保证，需要相应调高预留线，并由兜底终止补位；kill\_tree 只有 soft 时，每次终止后必须重新扫描归属表，确认没有残留。
5. 表中"防逃逸 / hard"只覆盖经 cgroupfs 或 breakaway 脱离。请第三方代为启动进程的逃逸不在范围内，见非目标。
6. Windows 的已知差异：commit 口径会把已提交但尚未使用的内存计算在内，数值通常高于 working set，配置限额时需要留出余量。由于 v1 采用 fail-closed，job 默认启用 KILL\_ON\_JOB\_CLOSE：subactld 崩溃时整组终止，与其他平台的语义一致。

## 核心模型：不变式、Lease 与 acgroup

每个 subagent 运行在自己的 acgroup 里，acgroup 通过申请 lease 得到。下面三条不变式就是 subactld 对外承诺的全部内容，其余机制都是为了实现它们。

**三条不变式**：

1. **I-A 归属与生命周期**：subactld 启动的每个进程（连同其子孙进程）恰好属于一个 acgroup。acgroup 结束时，其中的进程全部终止，并验证剩余进程数为 0。
2. **I-B 不超卖**：所有存活的顶层 acgroup 的内存硬限之和，不超过有效容量；嵌套 acgroup 的硬限包含在父级之内，不重复计入。有效容量 = min(配置上限, MemTotal − 预留 − 外部占用)，其中外部占用 = MemTotal − MemAvailable − Σ acgroup 当前 working\_set，即不由 subactld 管理的进程的占用，subactld 每秒重算。不能直接用 MemAvailable 计算：它已经扣掉了 acgroup 自己的占用，subagent 用得越多容量就越小，形成循环。CPU 和进程数允许超分，因为超分只会让进程变慢，不会让机器崩溃。
3. **I-C 可观测**：每个 acgroup 的用量、限额和状态随时可查。每一次状态变化都会产生事件，并写入审计日志。

subactld 自身的实现约束：

- reservation 在所有终止路径上都会释放，不会泄漏。
- **fail-closed**：subactld 崩溃或重启时，不尝试接管运行中的 lease。启动时把 backend 上报的全部 acgroup 一律整组终止。停机期间内核硬限照常生效，机器仍然安全，只是级联回收推迟到重启之后。这样账本只需保存在内存中（审计日志另行落盘），也不必解决"重启后不再是主进程的父进程、拿不到退出状态"的问题。重启不中断作为 v2 选项，见"v2 预留"。
  - 实现：新 daemon 对上一个 daemon 的 lease 一无所知，所以这些 acgroup 只被整组终止并记日志，不产生终止记录。daemon 被正常停止（SIGTERM）时，先把所有 lease 以 500 SUBACTL\_INTERNAL 回收再退出。
  - 实现：在 systemd 下，lease 子树位于服务自己的 cgroup 里（见下条），daemon 一死，systemd 当即清理整个服务 cgroup，lease 连同其中进程随之结束，比等到重启更早。VM 实测：`kill -9` 后一秒内重启，无残留。
- subactld 自身设置 oom\_score\_adj = −1000，由 systemd 以 Restart=always 托管。实现：仓库提供 `deploy/subactld.service`（`Restart=always`、`OOMScoreAdjust=-1000`、`Delegate=yes`）。daemon 在委派给它的 cgroup 下建 `leases/` 放所有 lease，并先把自己移进 `_daemon/` 叶子：cgroup v2 不允许持有进程的 cgroup 向下放控制器。

**Lease 与 envelope**：planner 为每个 subagent 申请一个 lease，申请时提交 envelope，即各维度的上限。subactld 批准后创建对应的 acgroup，并在其中启动 subagent。

| 维度 | 字段 | 计入不超卖 | 超限后的结果 |
| --- | --- | --- | --- |
| 内存硬限 | memory | 是（仅顶层） | 整组终止，终止码 413 |
| 自留内存 | memory\_self | 否（包含在 memory 之内） | 嵌套时为父节点自身进程保留的部分，子 lease 不能占用 |
| CPU | cpu | 否（允许超分） | 进程变慢，不终止 |
| 进程数 | pids | 否 | 无法再创建新进程，用于防止 fork bomb |
| 运行时长 | wall | 否 | 整组终止，终止码 408。每秒检查一次，最多超出 1 秒 |
| 输出量 | output | 否 | 整组终止，终止码 414。实现：agent 的 stdout/stderr 是管道，由 subactld 读出后写入 scratch 目录下的文件并计数；超限后 subactld 停止读取，写入方被管道阻塞，文件大小恰好等于上限，并在下一次检查（1 秒内）整组终止。这些输出不计入 lease 的内存。lead 写终端时，按中继的字节数计 |

- **进程由 subactld 代为启动**：planner 不直接 fork subagent，而是由 subactld 通过 pre-exec shim 把 subagent 启动在对应的 acgroup 里。subactld 是主进程的父进程，直接 wait 就能拿到退出状态。OS 的父子进程关系不承担归属职责。
- **主进程的退出如何归因**（实现）：退出码 0 记 200；非零退出或被信号终止记 520，退出码和信号写进 evidence。被信号终止时先看内存：若该 lease 或某个祖先的 acgroup 的自身硬限被撞（`memory.events.local` 的 `oom`，它只记在被撞限的那一层），撞限的那一层记 413，被它带走的子树记 502；若都没有、而容量护栏被撞（见兜底终止），记 503。`oom_group_kill` 不能用来判断：实测它记在被整组杀掉的那一层，即使撞的是上一层的限。
- **嵌套**：subagent 再派生 subagent 时，子 acgroup 嵌套在父 acgroup 之内，子的用量计入父的配额。cgroup v2 不允许一个已向子级启用 controller 的 cgroup 自己直接持有进程（no internal processes 规则），所以每个 lease 是一个目录：限额设在 `<lease>/` 上，lease 自己的进程放在叶子 `<lease>/_self/`，子 lease 放在 `<lease>/<child>/`。父级超限时内核在父级 OOM，memory.oom.group 会把整棵子树一起终止（413，子树级联）。
- **父节点即 holder**：lease 的 holder 就是它的父节点。顶层 lease 的父节点是 启动它的 subactl run -it CLI 进程（即 lead 的 holder），嵌套 lease 的父节点是父 lease 的主进程。父节点一旦死亡（pidfd 检测），它名下的全部 lease 立即级联回收，终止码 502。单机上 pidfd 是精确的，所以 v1 不设 TTL 心跳，也不支持父节点重启后重新接管。实现：holder 是创建 lead 的那个 operator 进程，不论是否带 `-it`；`subactl run` 收到挂断信号时直接退出，由 daemon 按 502 回收（早先版本会先主动 kill，导致关终端记成 499）。macOS 用 kqueue 的 NOTE\_EXIT。
- **主进程退出即回收**：lease 的主进程一旦退出，不论原因，lease 都立即进入 reaping，子孙进程一个不留。

**状态机**：admitted → running → reaping → reaped，共四个状态。v1 没有排队状态、自动降级状态，也没有暂停状态。每次迁移都产生事件。终止原因只用终止码表达（见"终止码与审计"），不另设 exit\_reason 枚举。

## 准入与配额

subactld 对每个配额请求只回答两种结果：批准，或者拒绝并附上原因。它不排队，也不替 planner 做选择；是否等待、换一种做法，还是先释放别的 subagent，都由 planner 决定。

**准入检查**（acquire 时）：

1. 顶层 lease：已分配的顶层内存硬限之和加上本次请求，不超过有效容量。
2. 嵌套 lease：`Σ 兄弟.memory + 本次请求 + 父.memory_self ≤ 父.memory`。
3. backend 能达到请求中 `require` 声明的强制等级。
4. 当前不处于容量缩水状态（见下文）。

拒绝时的 reason 取以下枚举值：`mem_budget`、`parent_budget`、`enforcement_unavailable`、`parent_not_running`、`capacity_shrunk`；调整配额时另有 `below_usage` 和 `below_allocated`（实现，见下）。所有申请都可带 `dry_run`，只做检查不预留。

**运行中调整配额**（resize）：

- 提高：只能使用尚未分配的余量（嵌套时为父节点的余量）。余量不够时拒绝，reason 为 `mem_budget` 或 `parent_budget`；planner 需要先终止或缩小其他 acgroup。
- 降低：新值不得低于"当前用量 + 余量"。余量默认取当前用量的 10% 与 64MB 中的较大者。低于这个值时拒绝，reason 为 `below_usage`，以免调整本身直接把进程杀掉。
- 降低到 `memory_self` 加上存活子 lease 的硬限之和以下时，同样拒绝，reason 为 `below_allocated`（实现）。
- 节点不能调整自己的配额，返回 403 `self_resize_forbidden`（实现）；只能经 resize-request 向父节点申请。
- 生效方式：Linux 直接修改 memory.max；Windows 修改 job 的内存上限；macOS 更新 monitor 的阈值。

**NEAR\_LIMIT 预警**：某个 acgroup 的内存用量达到自身硬限的 90%（可配置）时，subactld 推送 `NEAR_LIMIT` 事件，附带当前用量和增长速率。planner 可以选择三种处理：提高配额、任由它触限终止，或者主动终止它。subactld 本身不采取任何动作，除非 planner 事先声明了 `grow_to` 规则（见"声明式策略"）。实现：同一次越线只推送一次，用量回落到 85% 以下才重新武装；按 `grow_to` 扩容后，以新上限重新武装。事件同时作为控制消息投入该 lease 自己的 inbox。

**容量缩水与兜底终止**：有效容量随外部占用变化，分两个阈值处理：

- **容量缩水（只拒绝，不终止）**：外部占用增长，导致已分配的顶层硬限之和超过有效容量，即 I-B 暂时不成立。subactld 推送 `CAPACITY_SHRUNK`，此后新的 acquire 和提高配额一律拒绝（`capacity_shrunk`），直到恢复。已分配不等于已使用，所以此时不终止任何 acgroup。
- **兜底终止（subactld 唯一的自主动作）**：宿主 MemAvailable 实际跌破预留线，并连续 3 次采样如此（滞回，避免抖动）。先推送 `CAPACITY_SHRUNK`，给 planner 一个短窗口（默认 5 秒）自行释放；窗口结束仍不足时，每次终止一个 acgroup，等它回收完成、内存释放后再重新评估，直到可用内存回到预留线以上。终止码为 503 SAFETY\_NET。实现：每终止一个之后，滞回计数清零，下一个至少要再等连续 3 次新的低于预留线的读数。两类 `CAPACITY_SHRUNK` 用 `reason` 区分：`allocated_exceeds_capacity`（只拒绝）与 `below_reserve`（随后兜底终止）。
  - **容量护栏**（实现）：采样式的兜底终止至少要 3 秒才会动手，挡不住更快的暴涨。实验中（L 组第 5 次）外部干扰占着 3.6GiB 时，几个早已获准的 worker 同时开出数百个进程，两秒内从 0.3GiB 涨到 8.3GiB，整个爆炸舱撞上自己的上限，内核清空了整个舱。因此 cgroup2 backend 给所有 lease 共同的父 cgroup 设 `memory.max`，每秒更新为 MemTotal − 外部占用 − 预留线的一半：比兜底终止的触发线低半条预留线，所以有时间时仍由兜底终止先预警、再挑选；只有快过采样的暴涨才由内核在 lease 之间挑一个整组终止，而不是让宿主 OOM。父 cgroup 不设 `memory.oom.group`，每次只终止一个 lease。这样终止的 lease 记 503，evidence 中 `detected_by` 为 `kernel`。集成测试中，同样的暴涨在 63–98 毫秒内被终止，宿主的上限没有被触及；关掉护栏时宿主上限被撞。
- **终止顺序**：planner 事先提交过 `kill_order` 提示时按它执行；否则默认先终止叶子节点，同为叶子时按当前用量从大到小，用最少的终止次数释放内存。实现：先选当前用量最大的 lead 树；树内若有 lease 带 `kill_order`，取数值最小者；否则取用量最大的叶子；同用量按 id 排序以保证可复现。
- 5 秒窗口对 LLM planner 基本来不及响应，实际只对代码实现的 planner 和事先声明的 `kill_order` 有意义。兜底终止是一条确定的规则，不涉及判断。正常运行时它不会触发；一旦触发，就说明预留线或外部负载需要重新评估。

## 计量与可观测

planner 需要的所有数据，都来自 subactld 从 backend 读到的实测值，subagent 不汇报自己的用量。每条用量数据都带有 `mem_basis` 标签，不同口径的数值不能相加，也不能直接比较。

| 指标 | Linux | Windows | macOS | 采样 |
| --- | --- | --- | --- | --- |
| acgroup 内存 | `working_set`：v2 为 `memory.current` − `inactive_file` | `commit` | `phys_footprint` 之和 | 每秒一次 |
| 触限 / OOM | `memory.events` | job 通知 | 进程退出码与信号 | 事件驱动 |
| CPU | `cpu.stat` | job 记账信息 | `proc_pid_rusage` | 每秒一次 |
| 进程数 | `pids.current` | job 中的活跃进程数 | 归属表计数 | 每秒一次 |
| 宿主可用内存 | `MemAvailable`，或外层 cgroup 的上限与用量（实现） | 可用物理内存 | `kern.memorystatus_level`（实现） | 每秒一次 |

- **每个 acgroup 暴露的字段**：用量、限额、峰值、增长速率、CPU、进程数、状态、存活时长，以及 `mem_basis`。
- **宿主暴露的字段**：有效容量、已分配额度、外部占用（非 subactld 管理的进程）、预留线，以及 backend 的能力矩阵。
- **事件**：状态变化、`NEAR_LIMIT`、`CAPACITY_SHRUNK`、带终止码的终止事件、`escape`。
- **精度要求**：与内核统计的误差不超过 10%。实现：Linux 上与各进程 VmRSS 之和相差 −8.5% 至 −9.3%（working\_set 不含进程间共享页的重复计数）；macOS 上 phys\_footprint 与系统 `footprint` 工具相差约 5%。macOS 的有效容量按内核内存压力百分比计算，以整数百分比变化，且在内存压缩之后，所以变化幅度小于其他程序实际分配的内存。

**建议配额**（CLI 与公开的 Go client  提供，结果是确定性的）：`subactl suggest(kind)` 读取审计日志中同一 kind 的历史峰值，返回 p95 × 1.2 作为初始配额建议，同时返回样本数。样本不足时，返回默认值并加以标注。planner 可以直接采纳，也可以覆盖。这是"可学习"中的数值部分；需要判断的部分归 planner，见"planner 与 subactl 的分工"一节。

实现：只统计正常结束（2xx）的 lease。被内存硬限终止的 lease，其峰值就是上限本身；被提前终止的，峰值只是它当时的进度。把它们算进去，建议值会追着上限走：实验中 C 组在一轮内把 worker 的建议从 2GiB 推高到 9.6GiB，直到没有 worker 装得进 lead 的配额。样本少于 5 个时返回默认值 2GiB；p95 用最近秩法；每个 kind 最多保留 1000 个峰值；只用与 backend 同一 mem\_basis 的样本；daemon 启动时从审计日志重建。CLI 的 `subactl suggest KIND` 只向 stdout 打印字节数，便于 `--memory "$(subactl suggest worker)"`。

**metrics 与追溯**：

- **Prometheus `GET /metrics`**：有效容量、已分配额度、外部占用、各终止码计数、兜底终止次数、按 reason 统计的准入拒绝次数、各 kind 的用量分布（histogram）。按 Prometheus 文本格式手写输出，不依赖 client\_golang。label 只用 kind、depth、code，不用 lease id 或 lead id：两者都持续创建销毁，会让序列数爆炸。单个 lease 的数据走下面的时间线。
- **per-lease 时间线**：subactld 为每个存活 lease 在内存里保留 1 秒粒度的 ring buffer（默认 10 分钟），同时降采样到 5 秒，与审计日志一起落盘为 JSONL。`subactl trace <lease>` 重建完整时间线：spawn → 内存与 CPU 曲线 → 事件 → 注入的控制消息 → 工具调用信标 → 终止码与证据。实现：已结束 lease 的 1 秒粒度时间线在内存里再保留最近 1024 个，更早的从审计日志按 5 秒粒度重建；每个 lease 最多保留 500 个事件；`--tree` 把整棵子树（含已结束的 lease，从内存或审计日志中找回）作为 `children` 一并返回。trace 另附 `spikes`：把内存增长达到 64MiB 的区间归到当时那次工具调用的信标（实现）。
- **工具调用叠加**：进度信标带上工具名与序号（不带参数，以防注入和泄露），trace 把它们叠加在用量曲线上，直接回答"哪一次 bash 调用让内存涨了 4GB"。这是分析 agent 行为最有价值的一项。
- **`subactl top`**：类似 htop 的 TUI，显示各 lead 的 agent 树、每个节点的用量与配额进度条、状态（working / awaiting\_input / done）和最近事件。它是 API 的普通客户端，与具体 harness 无关。实现：进度条到 90% 时改用另一种字符；长时间无信标的节点标出静默时长；`--once` 打印一帧后退出。为此 lease 对象增加了 `activity` 字段（最后一次信标）。
- **自报数据**：LLM token 用量 subactld 测不到，只能由 adapter 上报，记录时标为 `self_reported`，与实测数据分开。
- **可观测性故障不影响强制执行**：采样、导出和落盘在独立的 goroutine 中运行，缓冲区有界，背压时丢弃数据并计数，不阻塞 enforcement。
- OpenTelemetry 导出（lease 树正好对应 span 树）放到 v1.1，作为可选 exporter。

## Enforcement backend 与防逃逸

backend 只负责五件事：创建隔离单元、在 exec 之前把进程放进去、采样读数、杀掉整组进程、销毁单元；另外提供两个查询：由 pid 查所在 acgroup（用于身份认证），以及列出现存 acgroup（用于启动时的 fail-closed 清理）。backend 必须针对每个维度回报自己的强制等级，planner 可以对最低等级提出要求。

```
type Backend interface {
    Capabilities() Caps // 每项能力的 EnforcementLevel: hard|soft|monitor_only|unsupported
    Create(ctx context.Context, lease LeaseID, parent ACGroupRef, env Envelope) (ACGroup, error)
    Spawn(ctx context.Context, g ACGroup, cmd Cmd) (Proc, error) // pre-exec placement；Proc 可 Wait 得到退出状态
    Sample(ctx context.Context, g ACGroup) (Usage, error)        // 用量带 mem_basis
    SetLimits(ctx context.Context, g ACGroup, env Envelope) error
    KillTree(ctx context.Context, g ACGroup) (KillReport, error)
    Destroy(ctx context.Context, g ACGroup) error
    Identify(ctx context.Context, pid int) (ACGroupRef, error)   // 调用方身份：pid → 所在 acgroup
    List(ctx context.Context) ([]ACGroupRef, error)              // 启动时 fail-closed 清理
}
```

实现：接口另有 `Name()` 与 `MemBasis()`。backend 能做到的额外能力用可选接口声明，daemon 按有无来启用：`HolderWatcher`（精确检测 holder 死亡：Linux pidfd、macOS kqueue）、`EscapeScanner`（逃逸扫描）、`OutputMeter`（输出字节数）、`Scratcher`（scratch 目录）、`TTYProc`（lead 的 pty）。没有 `HolderWatcher` 的 backend 不会得到更弱的替代品。

| backend | 平台 | memory | cpu / pids | 整组 kill | v1 范围 |
| --- | --- | --- | --- | --- | --- |
| cgroup2 | Linux unified，subactld 以 root 运行 | hard：memory.max + memory.oom.group | hard | cgroup.kill | 是 |
| fake | 任意 | 内存模拟 | 内存模拟 | 模拟 | 是（用于测试 ledger 和准入） |
| darwin-monitor | macOS | monitor\_only：采样 phys\_footprint，超限 kill | monitor\_only | 扫描归属表逐个 kill，再验证 | 是（实现）：`subactld --backend darwin`，macOS 上的默认值，可由普通用户运行 |
| jobobject | Windows | hard：job memory limit（commit 口径），超限通知后 TerminateJobObject | hard | TerminateJobObject | 推迟（实现） |
| systemd | Linux 非 root，user manager + Delegate=yes | 同 cgroup2 | hard | 同 cgroup2 | v2：没有 cgroup namespace 和独立 uid，防逃逸降为 soft |
| cgroup1 | Linux hybrid / legacy | hard（max），没有 oom.group，需 subactld 补杀 | hard | 先冻结再遍历 | v2 |
| linux-vm | macOS（Lima / OrbStack / Apple container） | 委托给 VM 内的 cgroup2 | hard | 同 cgroup2 | v2 |

**部署要求**：v1 的 cgroup2 backend 要求 Linux ≥ 5.14（cgroup.kill 自 5.14 起提供，clone3 的 CLONE\_INTO\_CGROUP 自 5.7 起提供），在 Linux 6.5+ 上额外使用 SO\_PEERPIDFD。实现：memory 与 pids 控制器必须能委派下来，否则启动失败；cpu 控制器委派不下来时，CPU 如实报告为 unsupported。socket 的属组由 `--socket-group` 指定（默认 `subactl`）。subactld 以 root 运行（system service），因为创建 cgroup namespace 需要 CAP\_SYS\_ADMIN，切换 agent 的 uid 需要 setuid 能力。**agent 本身从不以 root 运行**：lead 默认使用发起 `subactl run -it` 的 operator 的 uid（由 SO\_PEERCRED 取得），子 agent 继承父节点的 uid；启用 per-lead uid 时改用 uid 池分配的独立 uid。operator 是 root 时必须显式指定一个非 root uid，否则拒绝启动。原因是 root 身份的 agent 可以 setns 回初始 cgroup namespace、直接改写 cgroup 文件，所有隔离都会失效。非 root 模式见上表 systemd 一行。

**pre-exec placement**：subactld 不直接 exec 目标命令。它用 Go 的 `SysProcAttr.UseCgroupFD`（底层是 `clone3(CLONE_INTO_CGROUP)`，需要 Linux 5.7+ 与 Go 1.20+）把 shim 直接创建在 `<lease>/_self/` 里。shim 是 subactld 自身的 re-exec 入口（`subactld __exec`），不是单独的二进制。shim 依次完成以下步骤，然后 exec agent，保证第一条指令执行前就已经在限制之内：

1. 进入新的 cgroup namespace（unshare），ns 根即 `<lease>/_self/`。
2. 设置 oom\_score\_adj，并在配置要求时切换到 agent 的运行 uid（见部署要求）。以特权写入 oom\_score\_adj 会同时设定下限，agent 没有 CAP\_SYS\_RESOURCE 时无法再调低。下限必须高于 −1000：oom\_score\_adj 为 −1000 的进程不会被 memory.oom.group 杀掉。除此之外，这一项只影响全局 OOM；acgroup 内的 OOM 由 memory.oom.group 处理。实现取 200。
3. 设置环境变量 `SUBACTL_LEASE_ID` 与 `SUBACTL_SOCK`；把 stdout/stderr 重定向到 lease 的 scratch 目录下的文件。实现：另设 `SUBACTL_SCRATCH`；stdout/stderr 是通往 subactld 的管道，由 subactld 写入文件（见 envelope 表）。
4. 关闭多余的 fd。
5. （实现）保留 agent 用户的全部附加组（其中有能连上 daemon socket 的组，否则 agent 无法派生子 agent），然后设置 `no_new_privs`：此后该 lease 内的任何进程都不能再借 setuid 程序（如 sudo）提权，且子孙进程继承、无法撤销。这是在 VM 上用真实模型发现的：模型遇到权限错误后自己改用 sudo，而在有免密 sudo 的机器上这会让 agent 成为 root。

**防逃逸**：

威胁模型与覆盖情况：

- agent 把自己写进别的 cgroup（真实案例：[anthropics/claude-code#60488](https://github.com/anthropics/claude-code/issues/60488)）。由 cgroup namespace 挡住，hard：cgroup v2 要求迁移的源 cgroup 和目标 cgroup 都能从写入者的 cgroup namespace 中看到，否则返回 ENOENT。agent 以普通 uid 运行、对 cgroup 目录没有写权限，是第二道屏障。
- double-fork 加 setsid。在 Linux 和 Windows 上不构成逃逸：cgroup 与 job 的归属随 fork 继承，与进程组无关。只对 macOS 原生有意义，由定时扫描归属表覆盖，soft。
- 调低自己的 oom\_score\_adj，让内核去杀别人。由 shim 设定的下限挡住（前提是 agent 没有 CAP\_SYS\_RESOURCE），且只影响全局 OOM。
- （实现）借 sudo 等 setuid 程序提权为 root，再跳出 cgroup namespace。由 no\_new\_privs 挡住，hard。macOS 上没有对应机制，与"防逃逸：unsupported"的声明一致。
- 通过 IPC 请第三方代为启动进程（`systemd-run --user`、tmux server、docker、WMI、计划任务）。新进程不是 agent 的后代，通常也不继承环境变量。不在 v1 范围内，需要沙箱，见非目标。

对策：

1. **cgroup namespace**：agent 看到的 cgroup 根就是它自己的 `_self/`。即使宿主的 cgroupfs 挂载仍然可见，内核的包含检查也会拒绝它迁移到 ns 根之外。
2. **权限隔离**：cgroup 目录的属主是 root。可选让 agent 以独立 uid 运行，减少它对其他进程的可见性（/proc 读取、发信号）。
3. **逃逸扫描（soft 启发式）**：subactld 每 5 秒扫描一次 lease 树之外、环境中带 `SUBACTL_LEASE_ID` 的进程（读 `/proc/<pid>/environ`，需要 root）。`env -i` 即可绕过，所以它只用于发现无意的泄漏，不作为安全边界。发现后记录 escape 事件并终止，终止码 451。

归属的唯一真相源是 cgroup 成员关系，PPID 和进程组只作辅助。

**最低等级要求**：acquire 时可以声明 `require: {memory: hard}`。当前 backend 达不到时返回 `enforcement_unavailable`，绝不静默降级。

## Daemon API 与模块边界

系统由两个进程边界组成：`subactld`（Go daemon）和 `subactl`（Go CLI）。所有调用方，包括 LLM 在 bash 里调用的 CLI、代码 planner 使用的生成 client、`subactl top`，都只依赖 `api/openapi.yaml` 这一个契约，subactld 不感知调用方是谁。之所以必须是独立的 daemon，有两个原因：任何 agent 或编排器崩溃后仍要有进程负责回收；多个 lead 并存时需要一个全局账本。

&#91;embedded content: subactl 架构 · 2 类调用方、subactld 的 7 个模块、多 lead 的 acgroup 树\]

planner 通过 CLI 或公开的 Go client 发送请求并订阅事件。subactld 的内部模块只负责记账、校验、采样和执行；subagent 只存在于 acgroup 树中，嵌套的 acgroup 计入父级的配额。

```
subactl/                         # module: github.com/zydo/subactl
  api/                           # 线上类型（Go）与 openapi.yaml（唯一契约，嵌入二进制）
  client/                        # 公开的 Go client，标准库 net/http 手写，测试对照 openapi.yaml 校验
  yamlite/                       # YAML 严格子集解析器；公开，因为 mini-agent 也要读 scenario.yaml
  cmd/subactld/                  # daemon（含 __exec 入口，即 pre-exec shim）
  cmd/subactl/                   # CLI，命令见下
  internal/ledger/               # 内存中的配额账本、lease 树
  internal/daemon/               # lease 生命周期、HTTP 层、观测与控制面（inbox、信标、闲置）、启动时 fail-closed 清理
  internal/capacity/             # 有效容量、宿主读数、兜底终止的滞回与选择规则
  internal/telemetry/            # 事件总线、ring buffer
  internal/metrics/              # Prometheus 文本格式的 /metrics
  internal/audit/                # JSONL 审计、suggest()
  internal/pty/                  # pty 分配与 attach 帧格式
  internal/backend/              # 接口与 fake；cgroup2/、darwinmon/
  internal/contract/             # 读取 openapi.yaml，供一致性测试使用
  internal/lint/                 # 依赖规则检查（以测试形式运行）
  deploy/subactld.service        # systemd unit
  skill/subactl.md               # 教 LLM 使用 subactl 的 skill，与具体 harness 无关
  examples/mini-agent/           # module: github.com/zydo/subactl/examples/mini-agent（独立 go.mod）
    cmd/mini-agent/
    cmd/bench/                   # 对照实验与报告（run / report / extract / hog）
    agent/                       # agent 循环、steer 队列、hook、adapter、运行目录
    llm/                         # 大脑接口；scripted 与 Messages API 两个实现
    tools/                       # read、write、edit、bash
    tui/                         # 行模式输出
    scenarios/                   # 剧本、scenario.yaml、toy 与 radix 两个任务的脚本
    bench/                       # 爆炸舱、指标、报告、Parquet writer；chamber/ 下是 Docker 运行脚本
  go.work                        # 开发时联接两个 module
```

（实现：原计划的 internal/telemetry 落盘与 internal/backend/jobobject 未按原样出现，前者并入 audit，后者推迟。）

依赖只能单向：`examples/mini-agent → client → api ← subactld`（实现：mini-agent 另外依赖公开的 `yamlite`；`internal/lint` 的测试会拒绝任何违反这些规则的 import）。mini-agent 是独立的 Go module，工具链保证它无法导入 subactl 的 `internal` 包，只能使用公开的 `client` 包和 CLI；它需要的能力如果只能从内部拿到，就说明公开契约有缺口。两个 module 放在同一个仓库，开发时用 `go.work` 联接，一次提交即可同时修改；mini-agent 有了独立用户之后再拆仓库。整个项目只依赖 Go 标准库和官方扩展库 `golang.org/x/sys`、`golang.org/x/term`，不引入第三方库。传输层是 HTTP/JSON，Linux 和 macOS 上走 Unix socket（权限 0660，属组 subactl；agent 以独立 uid 运行时需加入该组），Windows 上走 named pipe。

**身份认证**：不发 token。subactld 从连接取得对端 pid，再由 backend 查出它所在的 acgroup，这个 acgroup 就是调用方的节点身份。取对端 pid 的方式：Linux 用 SO\_PEERCRED，6.5+ 用 SO\_PEERPIDFD 消除 pid 复用竞争；Windows 用 GetNamedPipeClientProcessId；macOS 用 LOCAL\_PEERPID。agent 的工具子进程与 agent 同属一个 acgroup，身份相同；子 lease 内的进程身份是子节点。不用 token 的原因：环境变量里的 token 可以被同 uid 进程从 `/proc/<pid>/environ` 读走，而 cgroup 归属在 v2 上无法伪造。不属于任何 acgroup 的进程是 operator（人），按 uid 授权，见"lead 与多 lead 隔离"。实现：身份在每个连接建立时确定一次；Linux 6.5+ 上用 SO\_PEERPIDFD 拿到对端的 pidfd，查完归属后确认对端仍然存活，从而排除 pid 被复用给另一个进程的情况。

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| GET | /v1/host | 有效容量、已分配额度、外部占用、预留线、能力矩阵 |
| GET | /v1/leases/{id}/available | 本节点子树的剩余预算与建议配额；可用 `?kind=` 指定要包含的 kind（实现） |
| POST | /v1/leases | 申请配额：参数为 envelope、parent、require、tty，以及可选的 kill\_order 与声明式策略。没有 parent 时创建 lead，发起请求的 operator 进程成为 holder。201 表示批准，409 表示拒绝并附 reason |
| POST | /v1/leases/{id}/spawn | 在 acgroup 内启动进程 |
| GET | /v1/leases/{id}/attach | lead 的 pty 中继（升级为双向流），由 subactl run -it 使用。实现：`Upgrade: subactl-pty`；客户端发送带类型与长度的帧（输入、窗口大小），daemon 回送原始终端输出；attach 之前的输出缓存最多 64KB，首次 attach 时补发；同一时刻只允许一个客户端，且只能由拥有该 lead 的 operator 连接 |
| PATCH | /v1/leases/{id} | 调整配额，规则见"准入与配额" |
| POST | /v1/leases/{id}/resize-request | 子节点向父节点申请扩容，以事件送达父节点 |
| POST | /v1/leases/{id}/terminate | 优雅终止（通知 → 等待 → SIGTERM → kill\_tree），参数为 reason、grace、code（released 记 204，killed 记 499）、recursive |
| POST | /v1/leases/{id}/kill | 立即整组终止，记 499。可选请求体 `{"dry_run": true}`（实现） |
| POST | /v1/leases/{id}/notify | 祖先 planner 经 subactld 向子孙节点投递控制消息 |
| POST | /v1/leases/{id}/progress | adapter 上报进度信标与状态（working / awaiting\_input / done） |
| POST | /v1/leases/{id}/wait-children | 等待子树全部结束，返回各自的终止码。实现：可选 `self`（连同自身一起等，`spawn --wait` 与 `run` 用它）和 `timeout_seconds` |
| GET | /v1/leases/{id}、/v1/leases?lead= | 单个 lease 或整棵 lease 树的状态和用量 |
| GET | /v1/leases/{id}/trace | 该 lease 的时间线：用量序列、事件、注入的控制消息、工具调用信标、终止码 |
| GET | /v1/leases/{id}/inbox | 本节点的 inbox（SSE），供 harness adapter 订阅 |
| GET | /v1/events | SSE 事件流，范围是调用方的子树，或 operator 有权查看的 lead |
| GET | /metrics | Prometheus 指标 |

所有 `{id}` 都必须在调用方自己的子树内（含自身），或属于 operator 有权操作的 lead，否则返回 403 `out_of_subtree`。规则见"planner 与 subactl 的分工"。实现：`{id}` 可写作 `self` 指调用方自己的 lease；大小一律以字节为整数传输；错误体为 `{error, reason, message, field, expected, example}`，校验失败时带出错字段、期望值和示例；未知字段使整个请求被拒绝。

```bash
# 人在自己的 shell（bash / zsh / fish 均可）里启动 lead
subactl run -it --memory 24G -- mini-agent
# 更常见：直接运行 mini-agent，它发现自己不在 lease 内时会自动 exec 上面这条命令
mini-agent

# lead 或任一 planner 在 bash 工具里派生子 agent，并同步等待结果
subactl available
subactl spawn --wait --kind worker --memory "$(subactl suggest worker)" --wall 30m \
  -- mini-agent --task tasks/react-dialog.md
```

- **自包装**：mini-agent 启动时检查 `SUBACTL_LEASE_ID`。没有，就 `exec subactl run -it -- mini-agent …` 成为 lead；有，就 `exec subactl spawn --wait -- mini-agent …` 成为当前节点的子节点。LLM 即使在 bash 里直接写 `mini-agent --task X &`，也会被收编进 agent 树，不会绕开 subactl。`--wait` 阻塞到子节点结束，返回终止码和结果路径，与 Pi 中 bash 同步调用自身的语义一致。
  - 实现：lead 只在 stdin 和 stdout 都是终端时才加 `-it`。子节点不 exec，而是留一个等待进程运行 `subactl spawn --wait`，原因有二：申请因 `parent_budget`、`mem_budget` 或 `capacity_shrunk` 被拒时，隔 2–4 秒重试（scripted 场景下直到剧本结束）；以及让退出码表达任务的成败（看子节点写下的结果，而不是 lease 的终止码），使 `test || mini-agent --task fix` 这样的写法成立。真实模型的任务是自由文本，其子节点的 kind 一律记为 `worker`。
- **lead 的 pty**：daemon 派生的进程拿不到用户终端，所以 lead 的 TUI 由 subactld 分配 pty；`subactl run -it` 在用户终端里切到 raw mode，中继输入输出并转发窗口大小变化（同 `docker run -it`；pty 基于 golang.org/x/sys 自行分配，不引入第三方库）。子 agent 是 headless 的，不分配 pty，输出写到 scratch 目录下的文件。CLI 不能自己迁入 cgroup 再 exec：它没有特权，既做不了 cgroup 迁移，也建不了 cgroup namespace。实现：Linux 与 macOS 都已实现；不带 `-it` 的 `subactl run` 也会把 lead 的输出边写边转给自己的 stdout/stderr（输出在 scratch 里，随 lease 结束删除，否则用户什么都看不到），终止记录写到 stderr。
- **shell 无关**：subactl 是普通可执行文件，不依赖 shell 函数、alias 或 source，任何 shell 都能调用；补全脚本为 bash、zsh、fish 各生成一份（实现：`subactl completion bash|zsh|fish`）。agent 一侧的工具固定为 bash 语义（找不到 bash 时退回 `/bin/sh`），可以配置，但不跟随用户的 `$SHELL`：LLM 写的命令以 bash 语义为主，zsh 在 word splitting、glob 无匹配时报错、数组下标等方面的差异会让命令出错；固定 shell 也保证实验可复现。
- `subactl run --memory 8G -- <第三方 agent 命令>`（不带 `-it`）：把未经修改的第三方 agent 作为一个退化的叶子节点整体纳入管理。它没有 adapter，只能用信号通信，只有硬限、wall 和整组回收兜底。实现：保留。在 VM 上分别包装了内存泄漏、挂起、输出失控、留下后台进程的程序，各得 413、408、414、200，均无残留进程。

**CLI 命令**（实现）：`host`、`run`、`spawn`、`available`、`ls`、`get`、`resize`、`terminate`、`kill`、`wait-children`、`suggest`、`trace`、`audit`、`events`、`notify`、`inbox`、`beacon`、`done`、`resize-request`、`metrics`、`top`、`completion`。退出码：0 表示成功或 lease 以 2xx 结束；1 表示 daemon 拒绝或出错，错误以 JSON 写到 stderr；2 表示用法错误；3 表示等到的 lease 以非 2xx 结束；130 表示被中断。与 daemon 的连接中途断开时，CLI 说明 daemon 已停止、它的 lease 都已结束。

## 终止码与审计

每个 lease 结束时，subactld 产出且只产出一个终止码，同时附上证据。终止码按"问题出在哪"分类，类似 HTTP 状态码。subactld 只陈述事实；重试、换做法、升级给人处理，这些都是 planner 的策略，subactld 不给建议。

| 类别 | 含义 | 终止码 |
| --- | --- | --- |
| 2xx 正常 | 正常结束，或被 planner 优雅释放 | 200 COMPLETED、204 RELEASED |
| 4xx 超出自身配额、违规或被主动终止 | 任务超出了自己的 envelope，或被 planner 终止；原样重试大概率仍会失败 | 413 OOM\_LEASE\_LIMIT、408 WALL\_EXCEEDED、414 OUTPUT\_EXCEEDED、451 ESCAPE\_DETECTED、499 KILLED（planner 主动终止）、410 IDLE\_REAPED（完成后闲置或停滞超过截止时间后回收） |
| 5xx 环境原因 | 不是任务本身的问题 | 503 SAFETY\_NET、502 PARENT\_DIED（父节点或 lead 退出导致级联回收）、520 EXTERNAL\_ABORT、504 REAP\_INCOMPLETE、500 SUBACTL\_INTERNAL（含 subactld 重启时的 fail-closed 清理） |

**结构化终止记录**（随事件推送，也可以通过 `GET /v1/leases/{id}` 查询）：

```json
{
  "code": 413, "name": "OOM_LEASE_LIMIT",
  "lease": "ls_7f3a", "kind": "renderer", "parent": "ls_01c2",
  "dimension": "memory", "mem_basis": "working_set",
  "peak": 3242719232, "limit": 3221225472, "enforcement": "hard",
  "evidence": {"oom_kill": 1, "top_procs": ["chrome (renderer) x11"], "reaped_pids": 14},
  "message": "renderer 超出内存硬限 3GiB，已整组终止；峰值主要来自 11 个 Chrome renderer。"
}
```

实现：大小一律是字节整数；另有 `at`（结束时间）。message 由模板生成，为英文；evidence 中有 `top_procs`（占内存最多的 3 个进程：pid、名称、内存；Linux 为常驻集，macOS 为 phys\_footprint）、`reaped_pids`（回收的进程数）、`exit_code`、`signal`、`step`（优雅终止停在哪一步）等。内核按硬限整组杀掉进程之后已无从列出，所以 lease 用量达到上限的 75% 后、或按当前增速两次采样内就会撞限时，每次采样都记一份，结束时若进程已不在，就用最近 10 秒内的那一份。实际收不干净（kill 后仍有进程，或 cgroup 删不掉）时记 504，原本的终止码记在 evidence 的 `intended_code`。

**归因必须准确**。下面四种情况，planner 的正确反应完全不同；subactld 依据触限事件、兜底终止记录、退出信号以及信号是否由 subactld 发出来区分它们：

- 在自身硬限内 OOM：413。说明配额给小了，或者任务本身太大。
- 被兜底终止：503。环境原因，与这个任务本身无关。
- 主进程被外部 kill（信号不是 subactld 发出的，例如 harness 因 LLM 额度耗尽硬杀了 subagent，或者 agent 自己崩溃）：520。subactld 不判断外部原因，只记录退出信号和时间。实现：主进程以非零退出码退出（没有信号）也记 520，退出码写进 evidence；只有退出码 0 记 200。
- 主进程正常退出，但留下了子孙进程：子孙进程一律回收；终止码按主进程的退出状态给出，evidence 中列出被回收的残留进程。

**审计日志**：只追加的 JSONL 文件，路径为 `$SUBACTL_STATE/audit/YYYY-MM-DD.jsonl`（实现：状态目录由 `--state` 指定，默认 `/var/lib/subactl`；写入在独立 goroutine 中进行，缓冲 4096 条，写满或写失败时丢弃并计数）。每次状态迁移、配额调整、兜底终止都写一条记录，内容包括：lease 树路径、kind、envelope 及其变化、用量峰值、终止码与证据、调用方身份。审计日志有两个用途：`subactl audit --lease <id>` 用它重建单个 lease 的完整时间线；`suggest()` 用它计算历史分布。

## planner 与 subactl 的分工

任何要创建子 agent 的节点都是 planner：lead 是，负责派生更深层 agent 的子 agent 也是。planner 负责语义层面和计划层面的低频决策，由事件触发；这里的事件不只是异常，创建子 agent 本身就是一种正常事件。subactl 负责实时的、数值型的、确定性的执行，不依赖 LLM。LLM planner 在 bash 工具里调用 subactl CLI，用法写在配套 skill 里。skill 只教 subactl 本身：spawn、available、terminate、wait-children、done 的用法，如何确定 envelope，各终止码的含义。这是预训练中没有的知识，控制在几百 token 以内。代码实现的 planner 用公开的 Go client 包。

|  | planner（agent 树中的任一节点） | subactl（subactld） |
| --- | --- | --- |
| 频率 | 低频，事件触发 | 实时，高频 |
| 决策性质 | 语义与计划：要不要创建、分配多少、失败后换什么做法、要不要告诉用户 | 确定性执行：校验、记账、强制、报告 |
| 自主动作 | 在自己子树内的一切操作 | 只执行已声明的规则：触限终止、级联终止、兜底终止、闲置回收、planner 预先声明的策略 |
| 不做的事 | 直接 fork 子进程、越过自己的子树操作 | 自己做取舍：自动扩容、自动重试、自己挑选终止谁、给出启发式建议 |

**agent 树与 lead**

- subactl 是创建子 agent 的唯一接口，并负责维护完整的 agent 树：主子关系，以及编排支持嵌套时的子孙关系。
- **lead** 是一棵 agent 树的入口，即没有 parent 的 top-level lease，由人用 `subactl run -it` 启动（或直接运行 mini-agent，由自包装完成）。不叫 root，以免与 Unix 的 root 用户混淆。
- lead 和其他节点一样进 acgroup，自身有硬限并计入账本，harness 自己的泄漏也会被关住；"外部占用"因此只剩非 agent 进程。lead 的 holder 是 `subactl run -it` 这个 CLI 进程：用户关掉终端，CLI 退出，整棵树级联回收（502）。detach 与 reattach 放到 v2。
- 任何节点（包括 lead）退出时，不论正常还是异常，subactld 都会级联终止它的整棵子树，终止码 502。

**lead 与多 lead 隔离**：同一台机器上可以同时运行多个 lead，共用一个 subactld。

| 维度 | 是否隔离 | 机制 |
| --- | --- | --- |
| 内存 | 是，hard | lead 的 envelope 就是整棵树预留的额度，子孙都嵌套在其中，准入后其他 lead 无法挤占。竞争只发生在准入和扩容：先到先得，容量不足时返回 409 |
| CPU | 按比例 | 每个 lead 的 acgroup 默认配相等的 cpu.weight：空闲时可以借用，争抢时各拿一份。v1 不控制 IO，可选 io.weight。实现：在物理机上测得达标，见验收标准 F27 |
| 控制面 | 是 | 身份按 acgroup 认定，一棵 lead 树里的节点无法查询、终止或通知另一棵树里的节点（403） |
| operator（人） | 按 uid | 不属于任何 acgroup 的进程是 operator。同一 uid 可以查看并终止自己启动的 lead 树，`subactl top` 默认显示本 uid 的全部 lead；只有 root 用户或 subactl-admin 组能操作全部 lead。实现：lead 的归属记在发起它的 operator 名下（`owner_uid`），与 agent 实际运行的 uid 分开；root 必须用 `--uid` 指定一个非 root 用户 |
| OS 层面 | 默认否 | 不同 lead 的 agent 默认以同一 uid 运行，可以绕过 API 直接 kill 对方进程、读对方的 scratch 目录。可选 per-lead uid 模式：每棵 lead 树从 uid 池分配一个独立 uid。默认关闭，因为文件系统沙箱是非目标，多 uid 会带来 worktree 的读写权限问题 |
| 兜底终止 | 规则隔离 | kill\_order 提示只在提交它的 planner 子树内部生效。跨 lead 时先选当前用量最大的 lead，再在它内部按它自己的提示和默认规则挑选 |
| 故障域 | 否 | 只有一个 subactld，它崩溃时 fail-closed 会回收全部 lead。v1 接受这一点，v2 由 per-lease supervisor 解决 |

- **lead 的默认 envelope**：`subactl run -it` 未指定 `--memory` 时，取 min(有效容量 × 50%, 剩余容量)，并在 TUI 状态栏显示；需要更多时显式指定。默认值不能是"剩余全部"，否则第一个 lead 会吃掉全部容量。
- **有意共享的部分**：`suggest()` 的历史数据按 kind 跨 lead 共享，数据越多越准；审计日志共用文件，经 API 读取时按权限过滤。

**权限：只能操作自己的子树**

- 调用方身份由连接对端进程所在的 acgroup 决定（见"Daemon API"一节的身份认证），不使用 token。
- 一个节点只能对自己的子树执行查询、创建、调整和终止，不能碰兄弟节点和祖先节点。
- 子节点不能给自己扩容，只能向父节点申请（resize-request）。申请只含结构化字段（目标值、当前用量），以事件形式送到父节点的 planner，由它决定是否批准。

**声明式策略**：planner 创建子 agent 时可以附带规则，由 subactld 在运行时确定性执行。这样秒级响应不经过 LLM，决策权又仍在 planner 手里。v1 的规则是封闭集合，只有两项；没有声明的情况一律不作为。重试属于 planner，subactld 不提供重试规则。

```yaml
on_near_limit: {grow_to: 6GiB}  # 收到 NEAR_LIMIT 时在父级余量内扩到该值；余量不足则不扩
kill_order: 2                   # 兜底终止时的顺序提示，数字小的先终止
```

**创建子 agent 的流程**

1. planner 调用 `subactl available` 查询可用资源。返回的是自己子树的剩余预算，而不是整台机器的原始数据：剩余内存额度、剩余 CPU、存活子节点数、宿主有效容量，以及各 kind 的 `suggest()` 建议值。具体颗粒度待定，全部带 mem\_basis。
2. planner 决定是否立刻创建，以及分配多少。
3. planner 调用 acquire。查询结果只是参考：acquire 是原子的检查加预留，余量已被其他 planner 用掉时返回 409。

**通信与终止协议**

subactld 需要和 planner 及其子 agent 通信，包括向子 agent 当前的对话插入控制消息。subactld 不能直接写入 LLM 的上下文，所以通信分成两层：subactld 为每个 lease 维护一个 inbox；由 agent 的 harness adapter 把 inbox 里的消息注入对话。v1 只有两个投递通道：

| 投递通道 | 时机 | 适用场景 |
| --- | --- | --- |
| harness hook adapter | agent 下一次工具调用前或下一轮对话开始时，以系统消息的形式注入 | 在 subactl 之上构建、支持 hook 的 harness。v1 提供一个参考 adapter，即 mini-agent 内建的 adapter |
| 仅信号 | 即时，但没有语义 | 没有 adapter 的 agent，如 subactl run 包装的第三方 agent |

消息只在 agent 下一次行动时才能被看到，agent 正在生成或卡住时可能永远看不到，所以优雅终止必须带超时：

1. **通知**：向目标 agent 的 inbox 投递 TERMINATE\_REQUESTED，附带原因和截止时间。agent 应保存进度、终止自己的子树并退出。实现：带 `recursive` 时投递给整棵子树中所有运行中的 lease。
2. **等待**：默认 30 秒，可由 planner 指定。
3. **SIGTERM**：未退出则向主进程发 SIGTERM，再等 10 秒。
4. **整组终止**：仍未退出则对整个 acgroup、连同子树执行 kill\_tree，并验证进程数为 0。

终止码由 terminate 的 code 参数决定（204 或 499），由规则触发的仍按原终止码。evidence 中注明最终是在哪一步结束的。

**消息的可信度**：注入对话的通道本身就是 prompt injection 的入口，所以要严格限定：

- inbox 只接受两类来源：subactld 自己生成的控制消息，以及目标节点的祖先 planner 经 subactld 转发的消息。兄弟节点和子节点都不能向它发送。实现：拥有该 lead 树的 operator 也可以发送，来源标为 `operator`；节点不能给自己发；inbox 只有 lease 自己能读；每个 inbox 保留最近 256 条。
- 每条消息都带来源标签（subactld 或哪个祖先 lease），由 adapter 以固定模板注入，不允许消息内容伪造来源。
- 控制消息的类型是封闭枚举（TERMINATE\_REQUESTED、NEAR\_LIMIT、RESIZED、CAPACITY\_SHRUNK）。自由文本只能作为来自祖先 planner 的 reason 字段出现。实现：reason 去掉控制字符、换行折成空格、截到 500 字节；adapter 以 `[subactl control message from <来源>] <类型> ...` 的固定模板注入，reason 只以引号括起的形式出现。
- 向上送给 planner 的事件全部由 subactld 根据结构化数据生成。子节点提交的唯一字符串是 done 的结果路径：必须位于该子节点的 scratch 目录内，只允许白名单字符；事件中只给路径，不内联文件内容。

**唯一管道：只管控制面（方案 A）**

- planner 与 subagent 之间的控制面通信必须经过 subactl，且只能经过 subactl。控制面包括：生命周期、资源、状态、进度信标和控制消息。
- 数据面（任务描述、结果文件）不经过 subactl，只以引用形式传递。例如 subagent 把结果写到自己 lease 的 scratch 目录，再通过 done 把路径交给 planner。审计日志因此能看到"谁在什么时候交付了什么"，而 subactl 不搬运内容本身，也不承担持久化和背压。

**双向插话**：subactl 既能向下给 subagent 插话，也能向上给 planner 插话（planner 不一定是 lead）。向上的事件包括：子 agent 触限、退出、申请扩容、停滞、完成后闲置。adapter 只在 planner 换轮时注入，所以天然是批量的：每轮注入一次摘要，同一 (lease, 事件类型) 合并为一条，不另设限流引擎。实现：事件类型为 `TERMINATED`、`NEAR_LIMIT`、`RESIZED`、`RESIZE_REQUESTED`、`DONE`、`IDLE_DONE`、`STALLED`、`CAPACITY_SHRUNK`、`ESCAPE`，另有只进 trace 不广播的 `BEACON`；合并后的摘要形如 `ls_xxx NEAR_LIMIT (x100)`。

**意见、截止与兜底**：LLM 负责优雅地遵循 subactl 的意见，但不能保证绝对遵循，所以 subactl 发出的每条意见都必须同时带上截止时间和到期后的确定性兜底动作。subactl 只发下表中的意见，不发出基于启发式判断的建议。

| 意见 | 发给 | 截止 | 兜底 |
| --- | --- | --- | --- |
| 请终止自己 | subagent | grace（默认 30 秒） | SIGTERM，再 kill\_tree |
| 即将触限 | subagent 与 planner | 用量到达硬限 | 内核整组终止（413） |
| 宿主容量不足，请释放 | planner | 5 秒（仅在 MemAvailable 跌破预留线时） | 兜底终止（503） |
| 子 agent 完成后闲置 | planner | max\_idle\_done（默认 60 秒） | 回收（410） |
| 子 agent 停滞 | planner | max\_idle（默认 30 分钟） | 回收（410） |

实现：停滞在静默 10 分钟时通知 planner 一次，30 分钟时回收；完成后闲置在报告 done 后立即通知一次，60 秒时回收。只有发过信标的 lease 才参与闲置检测；新的信标重新计时。

**进度信标与闲置检测**

hook 式 adapter 只在工具调用、换轮等时刻运行，无法响应轮询。所以 v1 不做轮询式 heartbeat，改为 adapter 在每个 hook 上主动上报进度信标（`POST /progress`），内容为状态（working / awaiting\_input / done）、时间戳和累计轮数。上报不经过 LLM，不消耗 token。

- subactl 平时不打扰 planner，只在两种情况下向上推送：**完成后闲置**（状态是 done，进程却还活着）；**停滞**（状态是 working，但连续 N 分钟没有新信标）。
- 没有信标不等于进程死亡：LLM 长时间生成或一次 20 分钟的编译都不会产生信标。进程真正退出由 pidfd 和主进程退出精确检测，所以 v1 没有"心跳中断即回收"。停滞只通知 planner；planner 一直不处理时，到 max\_idle 截止后才回收。
- planner 收到推送后自行决定：直接结束、经 notify 询问，或者再等一会。
- **done**：subagent 结束任务时，由 adapter 调用 `subactl done`（可附结果路径）。"完成却没人回收"因此会立即被发现。
- **awaiting\_input**：等待人类输入是合理的闲置，在这个状态下暂停闲置计时。
- 没有 adapter 的 agent 没有信标，只能依靠 wall time 上限兜底。

**孤儿与泄漏**

- 孤儿：在 agent 树模型里，结构上不会产生孤儿，因为父节点一旦退出，子树就级联回收。剩下的三种孤儿来源都有对应机制：进程逃逸（防逃逸，覆盖范围见前文）、主进程退出后残留的子孙进程（主进程退出即回收）、subactld 自身崩溃（重启时 fail-closed 清理）。
- 父节点还活着但没停掉子节点：由上面的闲置检测覆盖。
- 非进程资源：每个 lease 分配一个 scratch 目录。v1 不设大小配额，磁盘不在六个问题之内。子 lease 回收后，其 scratch 目录保留到父 lease 结束时一并删除，保证 planner 能在子节点被回收之后取走结果；lead 的 scratch 随 lead 结束删除。端口、socket 等资源随整组终止释放。

**结束流程**：一个 planner 合理的结束方式是先调用 `subactl terminate --recursive` 或 `subactl wait-children`，确认子 agent 全部结束后再退出。这是优雅路径，正确性不依赖它：节点异常退出时，subactld 仍会级联回收它的子树。

**校验与错误**：参数或命令不合法时整条拒绝，不做部分执行。错误信息结构化，写明出错的字段、期望值和一个正确示例，方便 LLM 自我修正。所有变更类命令都支持 `--dry-run`。

**适用范围**：subactl 只适用于在它之上构建的 agent harness。第三方 harness（包括它内部自己派生的 subagent）不经过 subactl，不在其管理范围内。`subactl run` 可以把第三方 agent 作为一个退化的叶子节点整体包起来。

**已定**（实现）：`available` 返回剩余内存额度、`memory_self`、已分给子节点的额度、存活子节点数、宿主有效容量和各 kind 的建议值，都带 mem\_basis，不含 CPU；`max_idle` 30 分钟、停滞通知 10 分钟、`max_idle_done` 60 秒；`subactl run` 的第三方退化模式保留。

## Demo：mini-agent 与演示任务

mini-agent 是在 subactl 之上构建的最小 multi-agent harness，用来演示和压测 subactl。它借鉴 Pi 的极简设计：只给模型四个工具（read、write、edit、bash），系统提示保持在 1000 token 以内；派生子 agent 不靠专门的工具，而是由 bash 调用 subactl。

|  | Pi | mini-agent |
| --- | --- | --- |
| 人启动入口 agent | 在 shell 中运行 `pi` | 在 shell 中运行 `mini-agent`，自动包装为 `subactl run -it`，成为 lead |
| 派生子 agent | `bash: pi -p "..."` | `bash: subactl spawn --wait -- mini-agent --task ...`，或直接写 `mini-agent --task ...`，由自包装收编 |
| 子 agent 的资源边界 | 无 | 各自的 acgroup，嵌套在父节点之内 |

**实现**：

- **Go 编写，独立 module**：放在 `examples/mini-agent`，只依赖 subactl 公开的 `client` 包和 CLI。不依赖任何第三方库，以下部分全部自行实现，合计约 1000–1500 行。
- **agent 循环**（约 300–500 行）：消息列表 → 调 LLM → 解析 tool call → 顺序执行 → 追加结果 → 循环。提供事件回调、steer 队列（inbox 消息在当前轮的工具调用结束后注入）、工具调用前的 hook（收到 TERMINATE\_REQUESTED 后不再执行新工具）和中止信号。
- **LLM 接入**（约 200–300 行）：大脑接口只有一个流式调用。scripted 大脑直接实现该接口；real 大脑用 `net/http` 对接 Anthropic Messages API，自行解析 SSE，并拼接分片到达的 tool call JSON。实现：`--provider` 选端点，`anthropic`（原生，默认 claude-opus-5-5，开启服务端 refusal fallback 与工具输入流式）、`deepseek`（`https://api.deepseek.com/anthropic`，默认 deepseek-v4-pro）、`zai`（`https://api.z.ai/api/anthropic`，Bearer 认证，默认 glm-5.3）；`--model`、`--base-url` 可覆盖。助手消息按服务商原样回放（含带签名的 thinking 块）。429、402、rate\_limit 与 billing 错误归为额度耗尽；prompt 过长归为上下文溢出；refusal 单独报错；流中途的 error 事件与断流都报错。被 max\_tokens 截断在工具调用中途时不执行该工具。兼容端点都实测过；原生端点只用录制的流做过回归测试。
- **四个工具**（约 200 行）：read、write、edit（精确匹配替换并校验唯一性，匹配失败时给出可读的报错，方便模型自我修正）、bash（固定 bash 语义，带超时与输出上限）。实现：bash 的输出写进文件而不是管道（否则命令留下的后台进程会让调用一直挂着），模型看到首尾截断后的部分和完整文件的路径；超时整组杀掉命令的进程组；在 subactl 下，子 agent 的命令输出同时转写到自己的 stdout，从而受 lease 输出上限约束。
- **行模式 TUI**（约 150 行）：逐行追加输出，底部一行输入，用 `golang.org/x/term` 切换 raw mode。实现：每条消息完整后整条输出，不逐字流式；输入是普通的按行读取，raw mode 只在 `subactl run -it` 一侧使用。不做全屏多窗格和差分渲染，也就避开了中文字符宽度与输入法的问题。查看某个节点的 transcript 用 `subactl trace` 或直接读文件；资源观测交给 `subactl top`，演示时两者在 tmux 中分屏。
- **adapter 内建**：在循环的工具调用与换轮 hook 上读取 inbox、注入控制消息，并上报进度信标（带工具名与序号）。
- **上下文溢出**：不实现 compaction。工具输出设硬上限，超出部分截断，并告诉模型完整内容所在的文件；对话超出上下文窗口时，worker 以非零退出码结束，并在 done 中注明 context\_overflow，由 planner 决定是否拆小任务后重新派发。
- **Ctrl-C**：lead 在 pty 中运行。第一次按下中断当前这一轮，第二次退出 lead，整棵树随之回收。实现：被中断那一轮里尚未得到结果的工具调用，统一补上“被用户中断”的结果，对话因此能继续；在提示符处按 Ctrl-C 直接退出。真实模型遇到服务商额度上限时，lead 只请求一次，报告后回到提示符，不重试。

**两个大脑**，工具层、harness 和 adapter 完全共用：

- **scripted**：带 seed 的剧本直接实现大脑接口。结果确定、可复现，不花 token，也不需要 HTTP server，用于对照实验。剧本在指定时间点返回额度耗尽错误，用来确定性地注入一次外部意外中止。
- **real**：用标准库 `net/http` 对接 Anthropic Messages API，用于展示，会消耗额度。网络路径（SSE 断流、限流错误）只在这个大脑下被覆盖。

**演示任务：radix-ui/primitives 全仓库升级到 React 19**

- **目标仓库与基线**：[radix-ui/primitives](https://github.com/radix-ui/primitives)，基线锁定为 commit `af1e3b41cb5c49d7b8c073d2aec34d5074c6dd17`（2024-06-03，"Publish release candidate"），即上游 PR [#2934 React 19 compatibility](https://github.com/radix-ui/primitives/pull/2934) 的父 commit。
- **基线状态**（已从仓库历史核实）：当时用的是 yarn berry workspaces，还不是现在的 pnpm；Node 18；`packages/react/` 下 54 个包、41 个 stories 文件、14 个 jest 测试文件。类型检查是根目录一次性的 `tsc --noEmit`，测试用 jest，组件预览用 Storybook 7（webpack5，端口 9009），端到端测试用 Cypress 8；react 依赖为 ^18.0.0。
- **参考答案**由三处上游改动组成：#2934（2024-06-10，代码层面的 React 19 兼容，共改 105 个文件，其中约 45 个是组件源文件）、#2952（2024-06-19，把 React 19 RC 加入 peer dependencies）、#3283（2024-12-12，修复 React 19 下的 composeRefs）。演示任务的目标是升级到 React 19 正式版，这三处改动只作参考，不要求逐行一致。
- **completion 判定**：换上 @types/react 19 后根目录 tsc 通过；该包的 jest 测试通过；该包的 stories 在 React 19 下渲染时控制台无报错，截图与 React 18 基线相比没有回归。截图用 Playwright 驱动 Chrome，它属于任务环境里的工具，不是 subactl 或 mini-agent 的依赖。
  - 实现：目标改为“运行时 React 19、类型仍为 @types/react 18”：上游参考修复在 @types/react 19 下编译不过，而 scripted 大脑不会自己写代码。判定为该包无 tsc 错误、jest 通过、stories 渲染无控制台报错（热更新轮询的 404 不算）；与基线截图的差异只记录，不作为失败条件。
  - 实现：Storybook 7 按版本号选择 React 根 API，只认 18，需要在 webpack 配置里把 `@storybook/react-dom-shim` 指向 react-18 版本才能在 React 19 下渲染。
  - 实现：任务的包列表取上游 #2934 在 `packages/react/` 下改动过的包，共 54 个；worker 在自己的 worktree 里套用上游对该包的改动，fixer 在第二层套用该包的改动、第三层套用全部包的改动。
- **流程**：lead 读取包列表，按包扇出 worker；每个 worker 在自己的 git worktree 中修改该包的依赖与源码，运行根目录 tsc 和该包的 jest，再起 Storybook 用 Chrome 截图验证该包的 stories；测试失败时再派生子 worker 定位问题，形成递归。
- **资源压力来自任务本身**：基线的类型检查是根目录级的，每个 worker 都要跑一遍全仓库 tsc；Storybook 7 的 webpack 构建很吃内存；多个 worker 同时起 Storybook 会抢 9009 端口，agent 要自己换端口，起了之后也容易忘记关。这些正是要观察的行为。实测：根目录 tsc 只要约 6 秒，压力主要来自 Storybook 加 Chrome，每个 worker 约 1.7GiB。

注意区分两套工具链：radix 是被 agent 改造的 TypeScript/React 项目，yarn、tsc、jest、Storybook、Playwright 都属于任务环境，由 agent 在 bash 工具里调用，也正是它们产生了要观察的资源压力。subactl 与 mini-agent 自身只用 Go，不依赖这些工具。

**贪婪行为**：下表中的行为在 scripted 剧本中刻意触发，在 real 大脑下也可能自然出现。每一种都对应一个已知的真实故障。

| # | 贪婪行为 | 在演示任务中的形态 | 对应的真实故障 |
| --- | --- | --- | --- |
| G1 | 过度派发 | 每个包一个 worker，并发扇出最多 32，失败时递归派生，深度 3 | [anthropics/claude-code#74001](https://github.com/anthropics/claude-code/issues/74001)：5–7 个后台 agent，每个约 6.5GB |
| G2 | 起浏览器和服务不关 | worker 用 Playwright 起 Chrome 截图后不关闭；为"先看看效果"起 storybook dev server 后不停 | [anthropics/claude-code#15861](https://github.com/anthropics/claude-code/issues/15861)、[anthropics/claude-code#50783](https://github.com/anthropics/claude-code/issues/50783) |
| G3 | 上下文膨胀 | agent 进程内的 transcript 缓冲每一步增长 X MB，从不释放（scripted 模拟） | [anthropics/claude-code#33350](https://github.com/anthropics/claude-code/issues/33350)、[anthropics/claude-code#74001](https://github.com/anthropics/claude-code/issues/74001) |
| G4 | 异常路径不清理 | 运行中途 scripted 大脑返回额度耗尽错误，without 模式下 harness 只 SIGKILL subagent 本身 | 背景中的触发问题、[anthropics/claude-code#83412](https://github.com/anthropics/claude-code/issues/83412) |
| G5 | 输出失控 | 一个 worker 卡在 yarn 的交互式提示或 watch 模式里，无限写 stdout | [anthropics/claude-code#47771](https://github.com/anthropics/claude-code/issues/47771) |
| G6 | 无界并行与搜索 | tsc -b 和测试按核数的 4 倍并行，同时做无界文件遍历 | [anthropics/claude-code#91523](https://github.com/anthropics/claude-code/issues/91523)、[anthropics/claude-code#86238](https://github.com/anthropics/claude-code/issues/86238) |

实现：G2 的浏览器由 agent 以独立进程启动后不再管理；G5 按约 5MB/s 的速率写，以便在没有输出上限时也能观察；G6 只作用于由 seed 决定的四分之一 worker：若所有 worker 都这样，四十个并发的类型检查需要的内存超过整个舱，任何配额都装不下。

**剧本参数**（`scenarios/*.yaml`）：seed、扇出上限、递归深度、每步泄漏量 X、故障注入时间点，以及每种贪婪行为的开关。实现：另有 `fail_rate`、`max_packages`、外部干扰的时间与比例、`greedy.unbounded_share`，以及在 subactl 下使用的 `envelopes`（各 kind 的固定配额）、`grow_factor` 和 `output_limit`。radix 场景的固定配额为 lead 9GiB、worker 3500MiB、fixer 1GiB、runaway 256MiB。

**爆炸舱**：without 组的设计目标就是把机器压垮，所以每次运行都放在一个外层隔离单元里，用一个独立的 VM 或带硬上限的外层 cgroup（例如 16G）。实现：采用外层 cgroup，上限 12GiB，建在特权 Docker 容器里；B、C 两组的 subactld 在舱内运行，用 `--capacity-cgroup` 以舱的上限和用量为“整机”，预留线取舱的 15%。看门狗每 5 秒在舱内启动一个探测进程，60 秒无响应即判 meltdown。B、C 两组优先用 VM：subactld 的有效容量以 MemTotal 和 MemAvailable 为基准，放在外层 cgroup 里时这两个值反映的是整台宿主，而不是舱的上限。如果只能用外层 cgroup，subactld 需配置为以该 cgroup 的 memory.max 和 memory.current 作为容量基准。这样既保护开发机，又能在舱内采到完整数据。外层再配一个看门狗：舱内持续 60 秒无响应就强制终止，并把这次运行记为 meltdown。

**两种接入方式**：without 模式下 mini-agent 关闭自包装，子 agent 由 harness 直接作为子进程启动，并刻意复刻真实缺陷：收到 limit 错误时只对 subagent 的 PID 发 SIGKILL，不杀进程组，孙进程（Chrome、dev server）因此变成孤儿。with 模式即正常的 subactl 接入。两种模式之间只差一个启动开关；如果需要改的不止这一处，说明边界设计失败了。实现：开关是 `--mode with|without`；with 模式下另外挂上 adapter。harness 的“只杀 PID”在两种模式下是同一段代码，只是在 with 模式下，被杀的主进程会让 subactld 回收整个 acgroup。

## 对照实验

三组实验使用同一份剧本、同一个 seed、同一台硬件，每组至少跑 5 次。比较的维度包括：会不会炸机、有没有残留进程、任务完成度、损失的工作量，以及资源利用率。

| 组 | 配置 | 要回答的问题 |
| --- | --- | --- |
| A without | 编排器直接 spawn subagent，没有任何限制 | 问题能否稳定复现 |
| B 静态配额 | 接入 subactld；每类 subagent 用固定配额，所有轮次都相同 | 只靠不超卖和硬限，能否保证安全 |
| C 自适应配额 | 接入 subactld；初始配额取 `suggest()` 的建议值，收到 `NEAR_LIMIT` 时按规则扩容，并在多轮之间累积历史数据 | 根据实测调整配额，能否在保证安全的同时提高利用率 |

C 组的 planner 用确定性规则实现（代码 planner），以保证可复现。由 LLM 担任 planner 的版本作为后续变体。实现：已作为 L 组完成，结果见下文“实验结果”。

实现：B 组每次运行从空的历史开始；C 组五次运行共用一个审计目录，建议值随运行累积。C 组的“规则”就是 worker 用 `subactl suggest` 的值作初始配额（无历史时用 B 组的固定配额），并声明增长到 1.5 倍的 `grow_to`。

**故障注入时间表**（每次运行都相同）：

- t = 2 分钟：G1 扇出开始。
- t = 5 分钟：外部意外中止。scripted 大脑返回额度耗尽错误，harness 随即 SIGKILL 正在运行的 subagent（只杀 subagent 本身，不杀进程组）。
- t = 7 分钟：外部干扰。一个非 agent 进程在 30 秒内占用有效容量的 30%，持续 4 分钟。
- t = 9 分钟：G5 输出失控。
- t = 15 分钟：剧本结束，之后再观察 60 秒。

| 指标 | 定义 |
| --- | --- |
| host\_peak\_ws | 爆炸舱的 working set 峰值（GB） |
| host\_oom | 外层 OOM 次数，以及是否触发看门狗判定为 meltdown。实现：取舱自身上限被触及的次数（舱的 `memory.events.local` 的 `oom`）；舱内各 lease 因自身上限被杀不计入，另以 oom\_kills 记录 |
| orphans\_after | 结束 60 秒后，带实验标记的残留进程数 |
| leaked\_ws\_after | 这些残留进程占用的内存（GB） |
| completion | 完成的子任务比例 |
| work\_lost | 因 413 或 503 被终止、需要重做的子任务数。实现：A 组没有终止码，取额度耗尽时被 harness 杀掉的 subagent 数 |
| util\_mean | 平均利用率：各 acgroup 用量之和 ÷ 有效容量。实现：以舱的上限为分母，三组可比 |
| safety\_net\_count | 兜底终止的次数 |
| makespan | 剧本总耗时（分钟） |

**可证伪的假设**（结论按数据写；被否证的假设如实报告）：

1. **H1**：G4 之后，A 组的 orphans\_after > 0；B、C 两组恒为 0。
2. **H2**：A 组在 5 次运行中至少出现 1 次外层 OOM 或 meltdown；B、C 两组的 host\_oom 恒为 0。
3. **H3**：C 组的 util\_mean 比 B 组高至少 15 个百分点，并且 work\_lost 不高于 B 组。
4. **H4**：外部干扰期间，B、C 两组每次运行的 safety\_net\_count ≤ 1，并且 planner 在兜底终止发生之前就收到了 `CAPACITY_SHRUNK`。

**实验结果**（实现，2026-11-17，第四轮，物理机 x86-64，12GiB 爆炸舱，每组 5 次；报告与数据见 `examples/mini-agent/bench/results/`）：

| | 判定 | 依据 |
| --- | --- | --- |
| H1 | 否证（A 组 1 次不满足） | A 组残留进程 38、55、0、12、116；B、C、L 三组 15 次全为 0。A 组第 3 次舱内 OOM killer 杀掉 2540 个进程，孤儿也在其中，计数时已无残留 |
| H2 | 成立 | A 组每次都触及舱的上限；B、C、L 三组从未触及 |
| H3 | 否证 | C 组利用率比 B 组高 4.3 个百分点（要求 15）；丢失的工作 C 组 21.4、B 组 15.2 |
| H4 | 成立，但未受检验 | B、C 两组兜底终止从未触发：干扰占舱的 30%，可用内存始终没有跌破预留线 |

完成率：A 组 4%，B 组 41%，C 组 53%，L 组 24%。C 组的 worker 建议配额五次都稳定在约 2.9GiB（B 组固定 3.5GiB），不再随 413 上涨；更紧的配额让更多 worker 同时运行、完成更多，但贪婪的 worker 撞限也更多，所以丢失的工作更多。H1 的判定规则在实验前确定，不因结果更改。

**由模型担任 planner（L 组，实现）**：lead 换成 DeepSeek，由它决定每个 worker 的配额和同时运行的数量，其下的 worker 与 fixer 仍是与 B、C 组相同的 scripted 大脑。它保证了安全（零残留、从未触及舱的上限），但完成率 11%–39%、平均 24%，低于 B 组的固定配额，更低于 C 组的规则：它给同类 worker 的配额在不同运行之间、同一运行之内都不一致，多数小于贪婪 worker 所需，撞限更多；给得少时完成的也少。C 组那条不需要判断的规则（历史峰值的 p95 × 1.2）更好。这只是一个模型、一份提示、五次运行的结果。

**真实模型复核（实现）**：lead 和 worker 都由 DeepSeek 担任，真正去改代码，8 个包，A、B 两组各 2 次。A 组两次都完成 8/8，但留下 153 和 84 个进程（storybook、esbuild，最多占 4.25GiB），舱的上限被触及数千到数万次；B 组两次都完成 8/8，零残留，从未触及上限。scripted 大脑得到的 H1、H2 在真实模型下同样成立；而且真实模型不需要剧本指示，自己就会留下 dev server 不关。

**容量护栏**：L 组最初的 5 次中，第 5 次在外部干扰期间两秒内暴涨，舱的上限被触及一次（见“兜底终止”下的容量护栏）。加入护栏后 L 组重跑 5 次；B、C 两组以及 L 组其余运行中，舱的用量从未超过“总量 − 预留线”，护栏不会动作，所以没有重跑。

此前三轮：第一轮作废（任务环境缺陷）；第二轮在 Docker Desktop 上进行，发现了建议配额随 413 一路上涨的缺陷；第三轮在物理机上进行，结论与本轮相同，但 G5 的 runaway worker 被当成自由文本任务、拿到默认的 1GiB 而不是剧本规定的 256MiB，使 C 组每次最后 6 分钟少跑一个 worker，修复后 B、C 两组重跑为第四轮，A 组沿用第三轮（没有配额，不受影响）。第三轮第一次尝试也作废：那台 Ubuntu 26.04 主机限制非特权 user namespace，截图检查自行启动的 Chromium 因没有沙箱可用而退出；改为与 Playwright 一样关闭沙箱。

**产出**：`bench/report` 自动生成三组对比图表和汇总表，原始时间序列以 parquet 格式保存，保证可以复现。实现：`bench report` 生成 `report.md`（四个假设的判定与依据、按组均值、逐次运行表）、`memory.svg`、`metrics.svg` 和 `series.parquet`；假设的判定规则写在代码里，与数据相悖时判为否证。

## 验收标准

功能标准 F1–F33 需要在 Linux cgroup v2 环境下全部通过；Windows 和 macOS 按各自能力矩阵声明的等级验收。实验标准 E1–E3 必须达成。每一条都要有对应的自动化测试或实验脚本。

实现状态（2026-11-17）：`[x]` 已达成，`[~]` 部分达成（缺口写在括注里）。F1–F33 全部达成。集成测试在四个 Linux 环境上通过：Docker Desktop（arm64）、GitHub Actions、VirtualBox VM（Ubuntu 26.04）、一台物理机（x86-64，以特权 Docker 运行）；macOS backend 的测试在本机与 GitHub Actions 的 macOS 作业上通过。

**功能**：

- [x] F1 父节点死亡回收：持有 lease 的 planner 被 SIGKILL 后，3 个各自带 Chrome 的 acgroup 在 5 秒内进程数归零，终止码 502，宿主上不残留任何带 SUBACTL\_LEASE\_ID 的进程。（实现：集成测试：holder 被 SIGKILL 后 7–13ms 内整棵树回收，记 502）
- [x] F2 主进程退出与外部中止：父节点仍存活时，绕过 subactld 直接对 subagent 主进程发 SIGKILL，它的子孙进程在 5 秒内全部被回收，终止码为 520；主进程正常退出但留下子孙进程时，子孙进程同样全部被回收，evidence 中列出这些进程。（实现：集成测试，含 setsid、nohup 脱离进程组的子孙进程）
- [x] F3 防逃逸：acgroup 内的进程尝试经 cgroupfs 迁移到自己的 acgroup 之外时失败；环境变量未被清空、却处在 lease 树之外的进程在 5 秒内被扫描发现并终止，记录 451。请第三方代为启动的逃逸不在验收范围内。（实现：集成测试：迁出被内核拒绝；扫描在 0.5 秒内发现并终止，记 451）
- [x] F4 内存隔离：某个 acgroup 泄漏到硬限时，只有它被终止（413），其他 acgroup 不受影响。（实现：集成测试，含父级与嵌套 lease 各自撞限的归因）
- [x] F5 不超卖：任意时刻已分配的顶层内存硬限之和都不超过有效容量；有效容量不随 acgroup 自身用量变化；超出时 acquire 立即返回 409 mem\_budget。（实现：单元测试（模拟宿主读数）与随机操作的性质测试）
- [x] F6 配额调整与嵌套准入：提高配额超出余量时被拒绝；降低到"用量 + 余量"以下时被拒绝（below\_usage）；合法的调整在 1 秒内生效；嵌套 acquire 超出 `父.memory − 父.memory_self − Σ 兄弟.memory` 时返回 409 parent\_budget。（实现：单元测试）
- [x] F7 NEAR\_LIMIT：用量达到硬限的 90% 后，2 秒内推送事件。（实现：真实内核：分配后约 1 秒推送）
- [x] F8 容量缩水与兜底终止：外部进程使已分配额度超过有效容量时，1 秒内推送 CAPACITY\_SHRUNK，新的 acquire 被拒（capacity\_shrunk），不终止任何 acgroup；外部进程继续增长使 MemAvailable 跌破预留线后，窗口结束时按终止顺序逐个终止，直到可用内存恢复，终止码为 503，且没有多杀。（实现：真实内存：以 640MiB 的外层 cgroup 作整机，预警后按 5 秒窗口恰好终止最大的叶子）
- [x] F9 计量准确：用量与内核统计（按平台的 mem\_basis）的误差不超过 10%，采样延迟不超过 2 秒。（实现：Linux 与各进程 VmRSS 之和相差 −8.5% 至 −9.3%）
- [x] F10 终止码归因：逐一注入 413、408、414、451、499、503、502、520、410 对应的故障，每个终止的 lease 都恰好产出一个与注入一致的终止码，准确率 100%。（实现：真实内核上逐一注入 11 种故障（含 200、204），11 个终止码全部正确，连续三次）
- [x] F11 输出上限：输出超限后整组终止（414），输出文件大小不超过上限加 1 秒的写入量。（实现：实现后为精确上限：文件大小恰等于上限，输出不计入 lease 内存）
- [x] F12 fail-closed 重启：对 subactld 执行 kill -9 后，停机期间各 acgroup 的硬限仍然生效；重启后 5 秒内，重启前存在的全部 acgroup 进程数归零，终止码 500。（实现：集成测试；systemd 下 daemon 被 kill -9 后一秒内重启，无残留）
- [x] F13 等级如实：macOS monitor 和 Windows backend 都报告真实的强制等级；当 backend 达不到 require 声明的等级时，返回 enforcement\_unavailable。（实现：Linux 与 macOS backend 都如实报告；Windows 推迟）
- [x] F14 嵌套：子 acgroup 的用量计入父 acgroup 的配额；父 acgroup 触限或终止时，整棵子树全部回收；父节点自身进程位于 `_self/` 叶子，创建嵌套 lease 时不出现 EBUSY。（实现：集成测试）
- [x] F15 lead 级联：已登记的 lead 被 SIGKILL 后，其整棵子树在 5 秒内进程数归零。（实现：同 F1；另在 VM 上断开 SSH 会话实测）
- [x] F16 子树权限：子节点进程去终止兄弟节点、给自己扩容、向兄弟节点发送消息，全部返回 403，且没有任何副作用；读取其他节点的环境变量无法冒充该节点。（实现：单元测试；身份只取自连接对端所在的 cgroup，不读任何环境变量）
- [x] F17 优雅终止：支持 adapter 的 agent 在 grace 内自行退出；忽略消息的 agent 在 grace + 10 秒内被整组终止；evidence 标明结束在哪一步。（实现：单元测试（模拟时间）：自行退出、SIGTERM、grace+10 秒整组终止三条路径）
- [x] F18 声明式策略：on\_near\_limit.grow\_to 在有余量时 2 秒内生效，没有余量时不扩容；kill\_order 改变兜底终止顺序；没有声明的情况 subactld 不作为。（实现：单元测试）
- [x] F19 校验：参数非法的命令整条被拒绝、没有副作用，返回的错误包含字段、期望值和示例。（实现：单元测试，覆盖 HTTP 层与所有变更类命令的 --dry-run）
- [x] F20 闲置检测：分别注入完成后闲置和停滞两种情况，每种都在阈值到达后 10 秒内向 planner 推送且只推送一次；planner 不处理时在对应截止后回收，终止码为 410；awaiting\_input 状态不计入闲置；一次 20 分钟无信标的长工具调用不会被回收。（实现：单元测试（模拟时间），含 20 分钟无信标的长调用不被回收）
- [x] F21 信标不消耗 LLM：进度信标全程由 adapter 上报，subagent 的 LLM 调用次数不因此增加。（实现：信标由 adapter 在 hook 中直接调用 API，不经过模型。测试：同一段脚本化运行，带与不带 adapter 都是 4 次模型调用，带 adapter 时另有 6 个信标）
- [x] F22 向上合并：planner 的一轮之内注入 100 个同类事件，下一轮只收到一条合并后的摘要。（实现：adapter 单元测试：100 条同类事件合并为一行）
- [x] F23 意见兜底："意见、截止与兜底"表中的每一行，在 LLM 不响应时都会在截止后执行对应的兜底动作。（实现：由 F4、F8、F17、F20 的测试共同覆盖）
- [x] F24 scratch 生命周期：子 lease 回收后其 scratch 目录仍可被父节点读取；父 lease 结束后一并删除；done 提交 scratch 目录之外的路径时被拒绝。（实现：集成测试）

* [x] F25 lead 与自包装：直接运行 `mini-agent` 时自动成为 lead，TUI 在用户终端中正常交互（包括窗口缩放）；关闭终端后整棵树在 5 秒内回收（502）；agent 在 bash 中直接运行 `mini-agent --task X &` 时，该进程成为当前节点的子节点。（实现：VM 上：with 模式下 lead 自动获得终端，窗口大小随动；断开 SSH 后整棵树 1 秒内回收，记 502；真实模型在 bash 里调用 mini-agent 派生的子节点进入了 agent 树）
* [x] F26 shell 无关：分别在 bash、zsh、fish 中启动 lead，行为一致；agent 工具使用的 shell 不受用户 `$SHELL` 影响。（实现：CI 在 Linux 与 macOS 上安装 zsh 和 fish，测试要求三个 shell 都在，缺一个即失败；agent 的 bash 工具固定用 bash，与 `$SHELL` 无关）
* [x] F27 多 lead 隔离：两个 lead 并发运行，其中一个扇出到自身配额上限时，另一个 lead 的 completion 不下降，在该 lead 的 CPU 需求不超过一半核数、且测试负载以 CPU 为主时（v1 不控制 IO），p95 任务延迟上升不超过 20%；跨 lead 的查询、终止、通知全部返回 403。（实现：物理机 16 个同构核上 p95 上升 1%–2%（四次），大小核混用 9%；虚拟机上的虚拟 CPU 不独立，无法测量。跨 lead 控制面隔离通过）
* [x] F28 operator 权限：同 uid 的 operator 能查看并终止自己的 lead 树，不能操作其他 uid 的 lead；subactl-admin 组可以操作全部。（实现：集成测试经真实 socket：另一个用户（nobody）看不到、也终止不了别人的 lead，加入 subactl-admin 组后两者都能做；测试结束后系统的组恢复原状）
* [x] F29 跨 lead 兜底：兜底终止从当前用量最大的 lead 中选择终止对象；一个 lead 设置的 kill\_order 不影响其他 lead 的节点。（实现：单元测试）
* [x] F30 metrics：`/metrics` 暴露所列指标；连续创建 10000 个 lease 后，序列数不随 lease 数增长。（实现：单元测试：一万个 lease 后序列数不变）
* [x] F31 trace：对任一已结束的 lease，`subactl trace` 能重建时间线，并把一次注入的内存突增定位到具体的工具调用信标。（实现：trace 含 1 秒样本、事件、注入的控制消息和带工具名的信标，并可从审计日志重建；另有 `spikes`：以相邻两个信标之间为一次工具调用，列出内存增长达到 64MiB 的调用，按增长从大到小。真实内核上 256MiB 的分配三次都归到了发生它的那次调用）
* [x] F32 观测故障隔离：让时间序列落盘目录变为只读或写满后，NEAR\_LIMIT 推送和 kill\_tree 的延迟不受影响，丢弃计数增加。（实现：单元测试：审计目录只读时 NEAR_LIMIT 与 kill_tree 的延迟不变，丢弃数增加）

- [x] F33 运行身份：lead 与子 agent 的进程 uid 等于发起者的 uid，没有任何 agent 以 root 运行；operator 是 root 且未指定非 root uid 时，`subactl run -it` 被拒绝。（实现：集成测试；另验证了 no_new_privs 下 sudo 被拒）

**实验**：

- [~] **E1 基线可复现**：A 组 5 次运行全部满足 H1 和 H2 中关于 A 组的部分。如果做不到，先把剧本调整得更"贪婪"，再做其他组的对比。（实现：H2 的部分 5 次全部满足；H1 的部分 4 次满足。不满足的那次不是不够贪婪，而是过于贪婪：舱内 OOM 把孤儿一并杀掉，所以没有按此条去加重剧本）
- [x] **E2 安全**：B、C 两组 5 次运行全部满足 orphans\_after = 0、host\_oom = 0。（实现：10 次全部满足）
- [x] **E3 结论**：对 H3 和 H4 给出"成立"或"否证"的结论；报告和图表都由脚本生成，可以复现。（实现：H3 否证，H4 成立但未受检验；`bench report --in <目录>` 由提交的数据重新生成报告）

实验状态：以物理机上的第三轮为准，结果见"对照实验"一节。

## 风险、开放问题与里程碑

不超卖带来的主要代价是利用率偏低：subagent 大多数时候用不满自己的配额。这个代价靠自适应配额（C 组）来缓解，能缓解到什么程度由实验验证；如果仍然不够，超卖作为 v2 的选项再引入。

| 风险 | 影响 | 缓解 |
| --- | --- | --- |
| 不超卖导致利用率低 | 同时能跑的 subagent 变少 | 用 suggest() 收紧初始配额，NEAR\_LIMIT 时扩容；用实验 H3 量化效果 |
| 外部负载突然增长 | I-B 暂时不成立 | 容量缩水时先拒绝新请求；MemAvailable 跌破预留线才兜底终止，CAPACITY\_SHRUNK 先给 planner 一个窗口 |
| agent 主动逃逸（[anthropics/claude-code#60488](https://github.com/anthropics/claude-code/issues/60488)） | 硬限失效 | cgroup namespace、独立 uid、逃逸扫描，见 F3 |
| 各平台口径不一致（Windows 的 commit 口径通常偏大） | 同一组配额在不同平台上的表现不同 | 所有数据都带 mem\_basis；配额建议按平台分别统计 |
| macOS 只能做到 monitor 级别 | 硬限不可靠 | 如实标注等级，调高预留线；需要硬限时走 Linux VM（v2）。实现：预留线为 25%，超限检查每 200 毫秒一次，见开放问题 1 |
| 模拟与现实有偏差 | 实验结论难以外推 | 演示任务用真实仓库，真实的 yarn、tsc 和 Chrome；scripted 的结论再用 real 大脑抽样复核，并用 subactl run 包装第三方 agent 验证 |
| 自研 harness 的边界情况 | 流式解析、edit 匹配等细节出错，影响实验稳定性 | 范围只限 demo 所需（一家 provider、顺序执行工具、行模式 TUI）；对实验结论起决定作用的是 scripted 大脑，它不经过网络和解析路径；real 大脑的 SSE 解析用录制的响应做回归测试 |
| 单一 subactld 是共享故障域 | daemon 崩溃时全部 lead 被回收 | fail-closed 保证不泄漏；v2 引入 per-lease supervisor |

**开放问题**：

1. ~~预留线的默认值~~ 已定（实现）：max(15%, 8GiB)，但不超过 MemTotal 的一半，以免小内存机器没有容量。macOS 上提高为 max(25%, 8GiB)，同样不超过一半：没有内核硬限，lease 会超出自己的上限，超出量约等于两次检查之间分配的内存。实测每秒检查一次时，以 100MiB/s 分配的 lease 超出 33MiB，以 2GiB/s 分配的超出 1GiB；所以 macOS 上的超限检查同时改为每 200 毫秒一次，超出量降到 8MiB 和 284MiB。多出的预留线用来容纳几个 lease 同时超出。
2. ~~`max_idle`、`max_idle_done`、停滞阈值 N 的默认值~~ 已定（实现）：30 分钟、60 秒、10 分钟。
3. ~~`subactl run` 的第三方退化模式是否保留~~ 已定（实现）：保留。
4. 主流 harness（Claude Code、Codex、Pi 等）的 adapter 如何实现。v1 只交付 mini-agent 内建的 adapter。仍未决。

**里程碑**（按依赖顺序；基线放在最前面，是为了先证明问题能够复现）：

1. M0 契约与骨架：OpenAPI、公开的 Go client 包、fake backend、账本单元测试。
2. M1 基线：mini-agent（scripted 大脑、without 模式）、agent 循环与四个工具、radix 任务与剧本、爆炸舱、指标采集。达成 E1。
3. M2 Linux 强制核心：cgroup2 backend、pre-exec shim、`_self` 叶子布局与嵌套、整组回收、防逃逸、fail-closed 重启。达成 F1–F4、F12–F14。
4. M3 配额与可观测：不超卖准入、配额调整、NEAR\_LIMIT、容量缩水与兜底终止、计量、终止码、审计、suggest()、`/metrics`、时间线与 `subactl trace`。达成 F5–F11、F30–F32。
5. M4 agent 树与通信：lead（pty 与自包装）、基于归属的身份认证与子树权限、operator 权限、多 lead 隔离、inbox、优雅终止、声明式策略、进度信标与闲置检测、`subactl top`。达成 F15–F29、F33。
6. M5 实验：B、C 两组的对照实验与报告；real 大脑演示。达成 E2、E3。
7. M6 跨平台：Windows Job Object backend（v1.1），以及 macOS monitor backend。

实现状态：M0–M5 已完成；M6 只完成 macOS monitor backend，Windows 推迟。实际顺序与上表一致，M4 之后又按验证中发现的问题补做了 daemon 的 systemd 托管、no\_new\_privs、输出经由 daemon 转写等。

## v2 预留

以下机制在讨论中出现过，但 v1 的六个问题不需要它们，或者成本与收益不相称，所以移出了 v1。只有当实验或实际使用证明 v1 不够时，才重新引入。

| 机制 | 移出 v1 的理由 | 什么情况下重新引入 |
| --- | --- | --- |
| 超卖 + 自动压力处理（施压、暂停、自主选择 victim、压力阶梯） | 一旦超卖，subactld 就必须自主做策略判断，变成第二个决策者，系统不可预测 | H3 证明自适应配额下利用率仍然不够时 |
| browser pool | Chrome 已经关在 subagent 的 acgroup 里，硬限兜得住；pool 只是优化，不影响正确性 | 浏览器开销成为利用率的主要瓶颈时 |
| 场景化资源类别（独占的 display、GPU 槽位或显存额度） | v1 场景中没有这类故障 | 需要编排 computer use 或 GPU 任务时 |
| cgroup v1 backend | 需要逐个 controller 维护层级，没有 memory.oom.group、cgroup.kill 和 namespace 包含检查，工作量接近一个独立 backend；systemd 258 已彻底移除 cgroup v1 支持，主流发行版的新版本只用 v2 | 目标用户的主力环境仍是 cgroup v1 时 |
| 非 root 部署（systemd user manager + Delegate=yes） | 没有 cgroup namespace 和独立 uid，agent 可以在委派子树内迁移，防逃逸降为 soft | 无法以 root 部署 subactld 时 |
| 暂停 / 恢复 | 对应不到 v1 的六个问题；暂停期间内存也不释放 | 需要在不丢进度的前提下临时让出 CPU 时 |
| 重启不中断（per-lease supervisor） | 需要 conmon 式的常驻监护进程持有输出、记录退出状态；v1 用 fail-closed 代替 | daemon 重启造成的工作损失不可接受时，或需要消除多 lead 的共享故障域时 |
| lead 的 detach 与 reattach | 需要 pty 在没有客户端时保持，并支持重新连接；v1 关闭终端即回收 | 需要长时间无人值守地运行 lead 时 |
| subactl-mcp、MCP 通知通道、stdin 通道 | LLM planner 有 shell，CLI 已经足够；多数 MCP 客户端不会把服务端通知注入模型上下文；向交互式 harness 的 stdin 写入在生成中途没有确定语义 | 目标 harness 只能通过 MCP 接入时 |
| 语义状态查询 | 本质是 planner 与子 agent 的对话，属于数据面；向上的自由文本还是注入入口 | 进度信标不足以支撑 planner 决策时 |
| OpenTelemetry 导出 | 时间线与 Prometheus 已覆盖 v1 的分析需求 | 需要接入已有的可观测平台时（计划 v1.1） |
| Python client | v1 的调用方只有 CLI 和 Go 编写的 harness | 有 Python 编写的编排器需要接入时，从 openapi.yaml 生成 |
| scratch 与输出的硬配额 | 需要 per-lease 文件系统或 project quota；磁盘不在 v1 的六个问题之内。实现：stdout/stderr 已经由 daemon 转写并按 `output` 精确截断；agent 自己写到磁盘上的文件仍无配额 | 磁盘耗尽成为实际故障时 |
| 配置 skill（probe / init / tune） | 容量探测由 subactld 启动时自动完成，配额建议由 suggest() 提供 | 部署形态足够多样、需要引导式配置时 |
| macOS 的 Linux VM 路径 | 工作量大；v1 先如实标注为 monitor\_only | 主力机需要 hard 内存隔离时 |

**参考**：

**缩写对照**：

| 缩写 | 全称 | 在本文中的含义 |
| --- | --- | --- |
| acgroup | agent control group | 本文的核心抽象：一组进程的资源边界，子孙进程自动归属。命名类比 cgroup，但跨平台，在 Linux 上由 cgroup 实现、在 Windows 上由 Job Object 实现，详见术语一节 |
| cgroup | control group | Linux 内核把进程分组并施加资源限额的机制，分 v1 和 v2 两代；本文 v1 只支持 v2 |
| cgroupns | cgroup namespace | 让进程把某个 cgroup 当作自己的根；v2 上还限制它把进程迁移到这个根之外 |
| pidfd | process file descriptor | 指向某个进程的文件描述符（Linux 5.3+），可以没有 pid 复用竞争地监视进程退出 |
| pty | pseudo-terminal | 伪终端；让没有真实终端的进程获得一个可交互的终端，由另一端中继输入输出 |
| SO\_PEERCRED | socket option: peer credentials | Unix socket 上读取对端进程 pid、uid、gid 的选项；SO\_PEERPIDFD（Linux 6.5+）直接返回对端的 pidfd |
| TUI | Text-based User Interface | 在终端里运行的文本界面 |
| OTLP | OpenTelemetry Protocol | OpenTelemetry 的数据传输协议，v1.1 的可选导出格式 |
| CI | Continuous Integration | 持续集成 |
| JSONL | JSON Lines | 每行一个 JSON 对象的日志格式 |
| MCP | Model Context Protocol | agent 调用外部工具的协议 |
| OOM | Out Of Memory | 内存耗尽；内核的 OOM killer 会杀掉进程以释放内存 |
| SSE | Server-Sent Events | 基于 HTTP 的服务端单向事件推送 |
| VM | Virtual Machine | 虚拟机 |
| VSZ | Virtual Set Size | 进程映射的全部虚拟地址空间，含未实际占用的预留部分；数值往往远大于实际内存占用，不能作为资源决策依据 |
| working set | — | Linux 上的内存口径：cgroup 内存用量减去可回收的 inactive file cache。Windows 和 macOS 的口径见术语一节 |

说明：文中引用的 Claude Code issue，只用作真实 agent 场景里的典型故障案例，证明这些问题确实存在。它们不是设计依据，Claude Code 也不是参考实现；subactld 的行为只由本文定义。

- [Claude Code #74001：CliGovernor 计量偏小、不驱逐](https://github.com/anthropics/claude-code/issues/74001)
- [Claude Code #60488：逃逸 systemd scope](https://github.com/anthropics/claude-code/issues/60488)
- [Claude Code #83412：subagent 撞额度后被硬杀](https://github.com/anthropics/claude-code/issues/83412)
- [Claude Code #74006：滚动窗口与后台 subagent](https://github.com/anthropics/claude-code/issues/74006)
- [Claude Code #50783：agent-browser 的 Chrome 成为孤儿](https://github.com/anthropics/claude-code/issues/50783)
- [Claude Code #47827：subagent 进程残留](https://github.com/anthropics/claude-code/issues/47827)
- [OpenClaw #136882：资源感知执行提案](https://github.com/openclaw/openclaw/issues/136882)
- [AgentCgroup（arXiv 2602.09345）](https://arxiv.org/abs/2602.09345)
- [Google Scion settings 示例](https://github.com/GoogleCloudPlatform/scion/blob/main/settings.yaml.example)
- [About Agent Substrate on GKE](https://docs.cloud.google.com/kubernetes-engine/ai-ml/about-agent-substrate)
- [cc-reaper](https://github.com/theQuert/cc-reaper)

## 实现记录（2026-11-17）

正文中标为“（实现）”的地方已经写明实际做法。按类别汇总如下，便于对照最初的设计。

**范围**
- macOS monitor backend 已交付；Windows 推迟，不在 v1 内。
- YAML 用自写的严格子集解析器，Parquet 用手写的最小 writer，以保持零第三方依赖。

**API 与 CLI**
- 大小以字节整数传输；`{id}` 可写作 `self`；错误体带出错字段、期望值和示例；未知字段使整个请求被拒绝。
- 新增拒绝原因 `below_allocated`，新增 403 `self_resize_forbidden`。
- `wait-children` 增加 `self` 与 `timeout_seconds`；`trace` 可带 `?tree=true`；`available` 可用 `?kind=`；`kill`、`terminate`、`notify`、`spawn` 都支持 `dry_run`。
- lease 对象增加 `scratch` 与 `activity` 字段，用量增加 `oom_events`。
- attach 用 `Upgrade: subactl-pty` 和自定义帧格式；attach 之前的终端输出缓存 64KB。
- 不带 `-it` 的 `subactl run` 实时显示 lead 的输出，终止记录写 stderr。

**终止码与归因**
- 非零退出记 520（退出码 0 才记 200）；内存撞限按 `oom_group_kill` 归到撞限的那一层。
- evidence 的 `top_procs` 在用量达到 75% 或即将撞限时预先采集，因为内核杀掉进程后已无从列出。
- fail-closed 清理的 acgroup 不产生终止记录；daemon 被正常停止时以 500 回收全部 lease；回收不干净记 504。

**安全与部署**
- shim 保留用户的附加组，并设置 no\_new\_privs，挡住借 sudo 提权（真实模型实际这样做过）。
- 身份每个连接确定一次，6.5+ 内核用 pidfd 排除 pid 复用。
- systemd unit：lease 位于服务委派的 cgroup 下，daemon 自己移入 `_daemon/` 叶子；daemon 一死 systemd 即清理全部 lease。

**配额与观测**
- 输出改为经由 daemon 的管道转写，超限即停，不计入 lease 内存；原设计的“文件大小每秒检查”会让输出洪流先撑满内存、被误判为 413。
- 建议配额只统计 2xx 结束的 lease，否则会随 413 一路上涨。
- NEAR\_LIMIT 每次越线只推送一次，回落到 85% 以下才重新武装。
- 兜底终止每终止一个 lease 后重新要求连续 3 次低读数；树内按 `kill_order`、再按最大叶子选择。
- macOS 的有效容量来自 `kern.memorystatus_level`；预留线默认 25%（Linux 为 15%），超限检查每 200 毫秒一次，其余仍每秒一次。

**mini-agent 与实验**
- 子节点的自包装不 exec，而是留一个等待进程，用于在配额不足时重试，并让退出码表达任务成败。
- real 大脑支持 Anthropic 与 DeepSeek、Z.AI 的兼容端点；遇到服务商额度上限不重试。
- radix 任务的目标改为“运行时 React 19、类型 18”；Storybook 7 需要 react-dom-shim 别名；截图差异只记录、不判失败。
- G6 只作用于四分之一的 worker；爆炸舱是特权 Docker 容器里的外层 cgroup。
- 实验指标的实际定义见“对照实验”一节；F27 的 CPU 指标在物理机上达成，在虚拟机上无法测量。
- 对照实验的结论以物理机上的第四轮为准：H2 成立；H1 在 A 组 5 次中有 1 次不满足（舱内 OOM 连孤儿一起杀掉）；H3 否证；H4 成立但未受检验。B、C、L 三组 15 次运行零孤儿、零次触及舱的上限。
- 后续两项实验已完成：由 DeepSeek 担任 planner（L 组，完成率平均 24%，低于 B、C）；由 DeepSeek 担任 lead 和 worker 的真实模型复核（H1、H2 同样成立）。
- 新增容量护栏：所有 lease 共同的父 cgroup 设 `memory.max`，挡住快过兜底终止采样的暴涨；413 的归因改看各层自己的 `oom` 计数。

