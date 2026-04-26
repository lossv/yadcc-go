# 移植进度

本文档记录 `yadcc-go` 的当前移植状态。原始 C++ 项目位于工作区平级目录 `../yadcc/`，本项目是独立 Go 项目。

## 当前状态

- 当前阶段：阶段 1 闭环已打通，阶段 2 缓存基础能力已就绪。
- 当前日期：2026-04-26。
- 当前结论：wrapper -> daemon -> scheduler -> worker 核心链路已全部实现，可运行基本分布式编译闭环。缓存（L1 内存）已接入 daemon 路径。

## 阶段进度

| 阶段 | 目标 | 状态 |
| --- | --- | --- |
| 阶段 0 | 初始化 Go 工程、入口、基础文档、git 仓库 | 完成 |
| 阶段 1 | Linux MVP：wrapper -> daemon -> scheduler -> worker -> 产物返回 | **基本完成** |
| 阶段 2 | 缓存、Bloom Filter、运行中任务复用 | 进行中（L1内存缓存已接入，L2磁盘/外部缓存服务待接入） |
| 阶段 3 | 调度增强、lease、keep-alive、生产化指标 | 未开始 |
| 阶段 4 | 平台抽象收敛，准备 macOS/Windows | 未开始 |
| 阶段 5 | macOS clang 支持 | 未开始 |
| 阶段 6 | Windows clang-cl/MSVC 支持 | 未开始 |

## 已完成

- 创建 `yadcc-go/`，与原始 `yadcc/` 平级。
- 初始化 `yadcc-go/` 为独立 git 仓库。
- 新增 `go.mod`。
- 建立 `cmd/yadcc`、`cmd/yadcc-daemon`、`cmd/yadcc-scheduler`、`cmd/yadcc-cache` 入口目录。
- 建立 `internal/client`、`internal/locald`、`internal/remoted`、`internal/scheduler`、`internal/cache`、`internal/platform`、`internal/protocol` 基础包目录。
- 新增项目 README、`.gitignore` 和本进度文档。
- 新增 `Makefile`，提供 `make fmt` 和 `make test`。
- 新增初始 proto 草案：`common.proto`、`scheduler.proto`、`daemon.proto`、`cache.proto`。
- `cmd/yadcc` 已具备本地 passthrough wrapper 雏形。
- `cmd/yadcc-daemon` 已具备 `/healthz` 和 `/local/get_version` HTTP 接口。
- `cmd/yadcc-scheduler` 已具备 `/healthz` 和 `/scheduler/state` HTTP 接口。
- `cmd/yadcc-cache` 已具备 `/healthz` 和 `/cache/stats` HTTP 接口。
- `internal/protocol` 已实现 multi-chunk 编解码基础函数及单元测试。
- 新增 `internal/compiler`，实现 gcc/clang 参数解析、输出文件推导、语言推导、可分发任务判断，附单元测试。
- 新增 `internal/cache` 核心能力：版本化 cache key、MemoryStore、DiskStore、Bloom Filter，附完整单元测试。
- `cmd/yadcc-cache` 支持 `-engine=memory|disk` 和 `-disk-dir`，HTTP 提供 `/cache/entry` 读写、`/cache/stats`、`/cache/bloom`。
- 新增 `internal/taskgroup`，支持相同任务 digest 的运行中任务合并等待，附单元测试。
- **新增 `internal/compiler/preprocess.go`**：实现本地预处理，优先 `-E -fdirectives-only`，失败回落 `-E`，结果返回内存。
- **更新 `internal/client/client.go`**：接入 `compiler.IsDistributable()` 决策，可分发任务先预处理再提交 daemon，daemon 不可用或失败时自动回落本地编译。
- **更新 `internal/locald/server.go`**：实现 `/local/submit_task` 接口，接入 L1 内存缓存、taskgroup 任务去重、远端 worker 提交（通过 scheduler acquire_worker）、本地回落编译（semaphore 限速）。
- **实现 `internal/scheduler/server.go`**：完整 worker 注册（`/scheduler/register`）、心跳（`/scheduler/heartbeat`）、任务分配（`/scheduler/acquire_worker`，least-loaded 策略）、释放（`/scheduler/release_worker`）、过期 worker 自动驱逐。
- **实现 `internal/remoted/server.go`**：完整远端编译 worker，接收预处理源码，本地运行编译器，返回 .o，启动时向 scheduler 注册，后台定期心跳。
- **更新 `cmd/yadcc-daemon/main.go`**：支持 `-mode=local`（wrapper-facing daemon）和 `-mode=remote`（worker）两种模式。

## 当前实现范围

### 核心链路（阶段 1 闭环已通）

```
wrapper (cmd/yadcc)
  -> 参数解析 (internal/compiler.IsDistributable)
  -> 本地预处理 (internal/compiler.Preprocess, -E -fdirectives-only 或 -E)
  -> HTTP POST /local/submit_task
     -> local daemon (internal/locald)
        -> L1 cache 查询 (internal/cache.MemoryStore)
        -> 向 scheduler 申请 worker (GET /scheduler/acquire_worker)
        -> HTTP POST /remote/compile 到 remoted
           -> remoted (internal/remoted): 写临时文件，运行真实编译器，返回 .o
        -> L1 cache 写入
        -> 本地回落（semaphore 限速）
  -> wrapper 收到 .o，写入输出路径
  -> 失败时直接 passthrough 本地编译
```

### scheduler
- worker 注册、心跳更新、过期驱逐（30s 超时）。
- least-loaded 策略选择 worker。
- `/scheduler/state` 返回真实 workers/running_tasks 统计。

### remoted
- 接收预处理源码，生成临时 .i/.ii 文件。
- 运行真实编译器，生成临时 .o，读回并返回。
- 启动时注册 scheduler，后台 10s 心跳。

## 待完成

- 将 `internal/cache` L2 DiskStore 接入 daemon 路径（当前为纯内存）。
- 将外部 `yadcc-cache` 服务接入 daemon（当前仅内存缓存）。
- 实现 Bloom Filter 预过滤（减少 cache miss 时的网络查询）。
- 设计并固化新版 protobuf IDL，生成 Go 代码，替换当前 HTTP/JSON。
- 将当前标准库 `sha256` cache key hash 升级为 BLAKE3。
- 添加 scheduler 任务 lease/keep-alive（当前无超时自动回收）。
- 添加 locald/scheduler/remoted 的 metrics（Prometheus）。
- 添加集成测试（本地单机 fake worker 编译 C 文件）。
- 实现 compiler digest（用于 EnvironmentDesc 和 cache key）。

## 验证状态

- `go version`：`go1.24.4 darwin/arm64`。
- `gofmt`：已执行。
- `go build ./...`：已通过。
- `go test ./...`：已通过。
