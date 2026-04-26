# 移植进度

本文档记录 `yadcc-go` 的当前移植状态。原始 C++ 项目位于工作区平级目录 `../yadcc/`，本项目是独立 Go 项目。

## 当前状态

- 当前阶段：阶段 3 进行中（GetConfig/config_keeper、远程重试、动态容量、cgroup 内存感知 均已完成）。
- 当前日期：2026-04-26。
- 当前结论：wrapper -> daemon -> scheduler -> worker 核心链路已实现。P0/P1/P2 主要特性均已移植，构建和全部单元测试通过。

## 阶段进度

| 阶段 | 目标 | 状态 |
| --- | --- | --- |
| 阶段 0 | 初始化 Go 工程、入口、基础文档、git 仓库 | 完成 |
| 阶段 1 | Linux MVP：wrapper -> daemon -> scheduler -> worker -> 产物返回 | **基本完成** |
| 阶段 2 | 缓存、Bloom Filter、运行中任务复用 | 基本完成（COS/原版 cache 格式未移植） |
| 阶段 3 | 调度增强、lease、keep-alive、生产化指标 | 进行中（基础 grant/keep-alive/metrics 已有，生产级策略未完成） |
| 阶段 4 | 平台抽象收敛，准备 macOS/Windows | 未开始 |
| 阶段 5 | macOS clang 支持 | 未开始 |
| 阶段 6 | Windows clang-cl/MSVC 支持 | 未开始 |

## 已完成

- 创建 `yadcc-go/`，与原始 `yadcc/` 平级。
- 初始化 `yadcc-go/` 为独立 git 仓库。
- 新增 `go.mod`。
- 建立 `cmd/yadcc`、`cmd/yadcc-daemon`、`cmd/yadcc-scheduler`、`cmd/yadcc-cache` 入口目录。
- 建立 `internal/client`、`internal/daemon`、`internal/scheduler`、`internal/cache`、`internal/platform`、`internal/protocol` 基础包目录。
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
- **更新 `internal/daemon/server.go`**：统一实现 local daemon 和 servant 角色，提供 `/local/submit_task`，接入 L1/L2 缓存、Bloom Filter、taskgroup 去重、scheduler grant、远端 worker 提交、本地回落编译。
- **实现 `internal/scheduler/server.go`**：worker 心跳注册、任务分配、释放、keep-alive、过期 worker 驱逐、基础 HTTP debug/metrics。
- **实现 servant gRPC**：接收预处理源码，本地运行真实编译器，收集输出目录下所有产物并逐个做路径 patch，启动时向 scheduler 心跳注册。
- **更新 `cmd/yadcc-daemon/main.go`**：单进程同时承担 local daemon 和 servant 两个角色。
- **补齐 `EnvironmentDesc` 接线**：scheduler 匹配和 cache key 已使用 compiler digest/kind/version、host OS/arch、target、object format、sysroot、stdlib、ABI、cache format version。
- **补齐 compiler registry digest 到 path 映射**：远端 worker 按请求的 compiler digest 选择真实编译器，不再依赖重新扫描 PATH。
- **增强 C/C++ 参数解析**：支持常见 joined 参数（如 `-Ifoo`、`-DDEBUG=1`、`-ofoo.o`、`-std=c++17`、`--sysroot=...`），预处理阶段保留 `-MD/-MMD/-MF/-MT/-MQ` 以生成本地依赖文件。
- **保守处理多产物/非 object 场景**：`--coverage`、`-fprofile-arcs`、`-ftest-coverage`、`-E`、`-S`、`-fsyntax-only`、`-pipe` 等场景默认回退本地编译。
- **增强 scheduler grant 行为**：`prefetch_requests` 参与 grant 数量计算，过期 grant 会同步释放 worker load。
- **P0-1 路径规范化** (`internal/compiler/pathrewrite.go`)：`NormalizePreprocessed()` 在预处理输出中将主机绝对路径（srcDir、sysroot、系统 include）替换为规范占位符，使缓存 key 与构建机无关。
- **P0-2 Token 认证** (`internal/auth/token.go`)：`Verifier` user/servant token 白名单，空 = 开放模式；daemon gRPC/HTTP、scheduler Heartbeat/WaitForStartingTask 均已接入；cmd 新增 `--user_tokens`/`--servant_tokens`。
- **P0-3 Servant 异步写缓存** (`daemon/server.go`)：编译成功后异步（5 s 超时）写 L2 cache。
- **P1-1 DiskStore LRU 驱逐**：`NewDiskStoreWithLimit`，内存 LRU atime 排序驱逐，`--disk-max-gb` 标志。
- **P1-2 GetConfig RPC + config_keeper**：proto 新增 `GetConfig`；scheduler handler；daemon `configKeeperLoop` 每 60 s 拉取并动态更新 token + `servantSem`。
- **P1-3 cgroup v1/v2 感知内存** (`internal/sysinfo/sysinfo_linux.go`)：容器内存限制覆盖 `/proc/meminfo`。
- **P1-4 远程任务失败重试**：`tryRemote` 最多 3 次（`maxRemoteRetries=2`）。
- **P2-1 file_digest_cache** (`internal/compiler/digest_cache.go`)：`(path, mtime, size) → sha256` LRU，`buildCacheKey` 使用 `DigestCached()`。
- **P2-2 consistent hash 环** (`internal/consistent/hash.go`)：150 虚节点，SHA-256，线程安全，含单元测试。
- **P2-4 task quota** (`internal/quota/quota.go`)：wrapper 侧并发信号量，默认 8×NumCPU（上限 256）。
- **P2-5 /dev/shm 临时文件优先** (`internal/daemon/tempdir_linux.go`)：Linux 优先 `/dev/shm`，非 Linux 用 `os.TempDir()`。

## 当前实现范围

### 核心链路（阶段 1 闭环已通）

```
wrapper (cmd/yadcc)
  -> 参数解析 (internal/compiler.IsDistributable)
  -> 本地预处理 (internal/compiler.Preprocess, -E -fdirectives-only 或 -E)
  -> HTTP POST /local/submit_task
     -> local daemon (internal/daemon)
        -> L1 cache 查询 (internal/cache.MemoryStore)
        -> 向 scheduler 申请 worker grant (gRPC WaitForStartingTask)
        -> gRPC QueueCxxCompilationTask 到 servant
           -> servant (internal/daemon): 写临时文件，运行真实编译器，返回输出文件列表
        -> L1/L2 cache 写入
        -> 本地回落（semaphore 限速）
  -> wrapper 收到 .o，写入输出路径
  -> 失败时直接 passthrough 本地编译
```

### scheduler
- worker 通过 heartbeat 注册和更新状态，过期驱逐（30s 超时）。
- dedicated 优先、空闲 slot 优先、自回避、低内存过滤。
- grant 分配、prefetch、释放、keep-alive、运行中任务查询。
- `/scheduler/state` 返回真实 workers/running_tasks 统计。

### servant
- 接收预处理源码，生成临时 .i/.ii 文件。
- 按 compiler digest 选择本机真实编译器，生成临时输出目录，读回所有输出文件并返回。
- 启动后通过 heartbeat 注册 scheduler，后台 10s 心跳。

## 待完成

- `EnvironmentDesc` 已接线，但 sysroot/stdlib digest 仍是轻量启发式，需要更严格的 toolchain/SDK 指纹。
- 将当前标准库 `sha256` cache key hash 升级为 BLAKE3。
- COS cache engine 未移植。
- 原版 cache entry 二进制格式未兼容，Go 版与 C++ 版不能共享缓存。
- 原版 `libfakeroot` / `LD_PRELOAD` 路径修复未移植，当前仅做 object 内路径字节替换。
- 原版 `client/common`、`daemon/local`、`daemon/cloud` 的大量边角能力还没有等价移植。
- `--token` 目前不是完整鉴权机制，scheduler/cache 仍需网络层隔离。
- macOS/Windows 只具备少量编译/系统信息适配，未完成平台级支持。
- 增加更多集成测试和原版 fake compiler/golden case 回归。

## 验证状态

- `go version`：`go1.24.4 darwin/arm64`。
- `gofmt`：已执行。
- `go build ./...`：已通过。
- `go test ./...`：已通过。
