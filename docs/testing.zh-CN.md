[English](testing.md) | 简体中文

# 测试

subactl 支持 Linux 和 macOS；在其他任何系统上，它的包都会构建失败，报错为 `undefined: subactl_supports_only_linux_and_macos`。本文列出各测试套件、它们运行过的机器，以及每台机器能测和不能测的东西。基准测试见 [benchmark.zh-CN.md](benchmark.zh-CN.md)。

## 测试套件

| 套件 | 命令 | 需要 | 覆盖内容 |
| --- | --- | --- | --- |
| lint | `make lint` | Go | `gofmt`；针对本机、Linux 和 macOS 的 `go vet`；以及每个公开包在 Windows 和 FreeBSD 上都构建失败，且只在它的 `unsupported.go` 处失败 |
| 单元测试 | `make test` | Go | 依赖规则（标准库、`golang.org/x/sys` 和 `golang.org/x/term` 之外一律不许引入）；在 fake backend 上的账本、准入、lease 生命周期与终止码；API 与 `api/openapi.yaml` 的一致性；client 与 CLI；审计日志、建议配额与指标；mini-agent 的循环、工具和 LLM client（用录制的流）；在 macOS 上还有 `darwin` backend，使用真实进程 |
| 集成测试 | `make integration` | Linux、root、cgroup v2；部分测试需要 Node | 真实内核上的 cgroup v2 backend，测试以它们检查的验收标准命名：嵌套、整组回收、防逃逸、fail-closed 重启、内存隔离、兜底终止与容量护栏、运行时长与输出上限、终止码、按 cgroup 归属认定身份、多 lead 隔离与 CPU 公平性 |
| 基准测试 | `bench/chamber/run.sh` | Docker，首次运行需要网络 | 爆炸舱里的 radix 任务；见 [benchmark.zh-CN.md](benchmark.zh-CN.md) |

在 Linux 上，`make integration` 用 `sudo` 运行测试二进制；在其他系统上，在特权 Docker 容器里运行。机器缺少某个工具（Node、zsh、fish）的测试会被跳过，除非 `SUBACTL_REQUIRE_SHELLS` 列出了必须存在的 shell。

## Linux

| 机器 | 用途 | 说明 |
| --- | --- | --- |
| 物理 x86-64：Intel Core Ultra 9 285K（24 核：8 个性能核、16 个能效核），60GB，Ubuntu 26.04，Linux 7.0，cgroup v2 | 基准测试（第三到第五轮）；CPU 公平性（F27） | 唯一能测 F27 的机器：旁边一个 lead 占满所有 CPU 时，另一个安静的 lead 的 p95 任务耗时在 16 个同构核上上升 1% 到 2%，大小核混用时上升 9%，允许值为 20%。只在舱的特权容器里需要 root |
| 虚拟机（Apple Silicon 上的 VirtualBox）：Ubuntu 26.04 arm64，Linux 7.0，6 个 CPU，15GB，cgroup v2 | 开发期间的集成测试，包括针对基准测试发现的 bug 的回归测试 | 它的 CPU 不是独立的：完全不用 cgroup，任务也会慢得远超 F27 的允许值，所以这里不强制 F27 的上限（`SUBACTL_F27_STRICT=1` 会强制） |
| Apple Silicon 上的 Docker Desktop：linux/arm64，10 个 CPU | 在 Mac 上运行 `make integration`；基准测试第二轮 | 与虚拟机一样无法测 CPU |
| GitHub Actions `ubuntu-latest` | 每次推送：lint、要求 bash、zsh 和 fish 都存在的单元测试、构建，以及带 Node 的集成测试 | |

Linux backend 对内核的要求：cgroup v2，并把 memory 和 pids 控制器（要限制 CPU 时还有 cpu）委派给 daemon。在 Linux 6.5 及以上，daemon 还用 pidfd（`SO_PEERPIDFD`）认定调用方，排除 pid 复用；更早的内核退回 `SO_PEERCRED`。不支持 cgroup v1。

## macOS

| 机器 | 用途 | 说明 |
| --- | --- | --- |
| Mac mini，Apple M4 Pro（14 核），64GB，macOS 27 | 开发；`darwin` backend 用真实进程的测试；[教程](tutorial.zh-CN.md)的示例；测量 lease 在两次检查之间超出上限多少 | 每 200 毫秒检查一次时，以 100MiB/s 分配的 lease 超出上限 8MiB，以 2GiB/s 分配的超出 284MiB，所以 macOS 上的默认预留线是内存的 25%，而不是 15% |
| GitHub Actions `macos-latest` | 每次推送：要求 bash、zsh 和 fish 都存在的单元测试（含 `darwin` backend），以及构建 | |

在 macOS 上 subactl 只监控、不强制：没有 cgroup，backend 从进程表维护自己的归属表，采样每个进程的 physical footprint，终止超限的 lease。它如实报告每项能力（soft 或 monitor_only，防逃逸为 unsupported），要求硬性强制的请求会被拒绝。daemon 以普通用户运行。基准测试没有在 macOS 上运行过：它的舱是 Linux cgroup。
