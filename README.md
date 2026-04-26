# yadcc-go

`yadcc-go` 是 [yadcc](https://github.com/Tencent/yadcc)（Yet Another Distributed C++ Compiler）的 Go 移植版本，实现了与原版相同的核心功能：将 C/C++ 编译任务分发到集群中的多台机器，同时在本地提供多级缓存，大幅缩短大型项目的构建时间。

> **当前状态**：Linux 基础分布式编译闭环已打通，L1/L2 缓存、Bloom Filter、scheduler grant/keep-alive 和基础 metrics 已接入；仍不是 C++ 原版的完整等价移植。与 C++ 原版使用不兼容的 wire protocol（使用 gRPC/HTTP 而非 Flare RPC），缓存 key 采用 SHA-256（原版用 BLAKE3），两者**不共享缓存**。

---

## 目录

- [架构概览](#架构概览)
- [快速开始](#快速开始)
- [构建](#构建)
- [组件详解](#组件详解)
  - [yadcc（编译器 wrapper）](#yadcc编译器-wrapper)
  - [yadcc-daemon](#yadcc-daemon)
  - [yadcc-scheduler](#yadcc-scheduler)
  - [yadcc-cache](#yadcc-cache)
- [端口与地址规范](#端口与地址规范)
- [环境变量](#环境变量)
- [部署方案](#部署方案)
  - [单机试用](#单机试用)
  - [小型集群](#小型集群)
  - [构建场（build farm）](#构建场build-farm)
- [与构建系统集成](#与构建系统集成)
  - [CMake](#cmake)
  - [Make / 裸编译命令](#make--裸编译命令)
  - [Bazel / Buck](#bazel--buck)
- [运维](#运维)
  - [健康检查](#健康检查)
  - [状态查询](#状态查询)
  - [Prometheus 监控](#prometheus-监控)
  - [日志](#日志)
- [缓存机制](#缓存机制)
  - [L1 内存缓存](#l1-内存缓存)
  - [L2 外部缓存（yadcc-cache）](#l2-外部缓存yadcc-cache)
  - [缓存 key 组成](#缓存-key-组成)
  - [不可缓存任务](#不可缓存任务)
- [安全注意事项](#安全注意事项)
- [性能调优](#性能调优)
- [已知限制](#已知限制)
- [开发](#开发)

---

## 架构概览

```
┌─────────────────────────────────────────────────────────────┐
│  开发机 / CI 节点                                            │
│                                                             │
│  cc foo.c  →  yadcc wrapper                                 │
│                    │  HTTP (loopback :8334)                  │
│                    ▼                                        │
│             yadcc-daemon  ──────────────────────────────┐   │
│             (本地 + servant)                             │   │
│               │  预处理  │  L1 内存缓存 (LRU 512 MiB)   │   │
│               │          │  L2 外部缓存查询              │   │
└───────────────┼──────────┴───────────────────────────────┘  │
                │ gRPC (:8336)                                │
                ▼                                             │
         yadcc-scheduler  ←── heartbeat ──  其他 daemon 节点  │
                │                                             │
                │ 分配 grant → servant gRPC (:8335)           │
                ▼                                             │
         远程 yadcc-daemon（servant 角色）                     │
                │ 编译完成 → 返回 object                       │
                │                                             │
         yadcc-cache (可选)  ←── put/get ─── daemon            │
         gRPC :8338 / HTTP :8339                              │
```

**角色说明：**

| 角色 | 进程 | 描述 |
|---|---|---|
| **wrapper** | `yadcc` | 替换 `cc`/`g++` 等，预处理后提交给本机 daemon |
| **local daemon** | `yadcc-daemon` | 接收 wrapper 请求，查缓存，向 scheduler 申请 grant，分发给远端 |
| **servant** | `yadcc-daemon`（同进程） | 接收其他 daemon 的编译任务，在本机执行 |
| **scheduler** | `yadcc-scheduler` | 集中管理 worker 注册、负载，分配 grant |
| **cache** | `yadcc-cache`（可选） | 持久化分布式编译缓存（L2） |

每个 `yadcc-daemon` 进程**同时扮演 local 和 servant 两个角色**，无需单独部署。

---

## 快速开始

```bash
# 1. 构建
cd yadcc-go
make build          # 产物在 ./bin/

# 2. 启动 scheduler（集群只需一个）
./scripts/start-scheduler.sh

# 3. 在每台参与节点上启动 daemon
YADCC_SCHEDULER_GRPC_ADDR=<scheduler_ip>:8336 ./scripts/start-daemon.sh

# 4. 配置编译器 wrapper（以 CMake 为例）
export CC="yadcc cc"
export CXX="yadcc c++"
cmake -B build && cmake --build build
```

---

## 构建

**依赖：**
- Go 1.24+
- `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc`（仅修改 proto 时需要）

```bash
# 编译所有二进制（输出到 bin/）
make build

# 运行单元测试
make test

# 运行集成测试（需要本机安装 gcc/clang）
go test -v -tags integration ./tests/integration/

# 重新生成 proto（修改 api/proto/ 后执行）
make proto-gen
```

生成的二进制：

| 二进制 | 说明 |
|---|---|
| `bin/yadcc` | 编译器 wrapper |
| `bin/yadcc-daemon` | 本地 daemon + servant |
| `bin/yadcc-scheduler` | 调度器 |
| `bin/yadcc-cache` | 缓存服务 |

---

## 组件详解

### yadcc（编译器 wrapper）

wrapper 是一个透明代理程序，作为 `cc`/`g++`/`clang++` 的替换品插入构建系统。

**调用方式：**

```bash
# 方式 1：直接调用，第一个参数是真实编译器
yadcc /usr/bin/gcc -c foo.c -o foo.o

# 方式 2：符号链接模式，wrapper 以编译器名字调用
ln -s /path/to/yadcc /usr/local/bin/gcc
gcc -c foo.c -o foo.o   # 自动查找系统 gcc 并代理

# 方式 3：通过 CMake toolchain
set(CMAKE_C_COMPILER "yadcc cc")
set(CMAKE_CXX_COMPILER "yadcc c++")
```

**工作流程：**

1. 解析参数，判断任务是否可分发（必须有 `-c`，语言为 C/C++，单输入文件）
2. 如果不可分发（链接、汇编、`-`读stdin 等），直接 `execv` 原编译器（零开销透传）
3. 本地预处理（`-E`）生成 `.i`/`.ii`
4. 通过 HTTP POST 到本机 daemon（`http://127.0.0.1:8334/local/submit_task`）
5. daemon 返回后，写入 `.o` 文件，打印 stdout/stderr，返回 exit code

**环境变量：**

| 变量 | 默认值 | 说明 |
|---|---|---|
| `YADCC_DAEMON_ADDR` | `http://127.0.0.1:8334` | daemon HTTP 地址，用于非标准端口或测试 |

**注意事项：**
- wrapper 不接受任何自身 flag，所有参数透传给编译器
- 当 daemon 不可达时，wrapper 自动回退到本地编译，**不会报错退出**
- `__TIME__`、`__DATE__`、`__TIMESTAMP__` 三个宏会使任务变为**不可缓存**（见[不可缓存任务](#不可缓存任务)）

---

### yadcc-daemon

每台参与节点运行一个 `yadcc-daemon`，**同时承担两个角色**：

- **local 角色**（HTTP `:8334`，仅 loopback）：接收本机 wrapper 的编译请求
- **servant 角色**（gRPC `:8335`，网络可访问）：执行其他节点分发过来的编译任务

**启动参数：**

```
yadcc-daemon [flags]
```

| Flag | 默认值 | 说明 |
|---|---|---|
| `--local_addr` | `127.0.0.1:8334` | wrapper 面向的 HTTP 监听地址。**必须是 loopback**，不应暴露到网络，否则任意用户可提交任务。 |
| `--servant_addr` | `0.0.0.0:8335` | 接受远端编译任务的 gRPC 监听地址。需要在防火墙中对集群内部开放。 |
| `--scheduler_uri` | `""`（空） | scheduler 的 gRPC 地址，格式 `host:port`，例如 `10.0.0.1:8336`。**留空则禁用分布式编译**，只走本地 + 缓存。 |
| `--cache_addr` | `""`（空） | yadcc-cache 的 gRPC 地址，格式 `host:port`。留空则只使用进程内 L1 缓存。 |
| `--token` | `yadcc` | 预留的共享 token 字段，当前仅随部分 RPC 传递，尚未形成完整认证机制。 |
| `--servant_priority` | `user` | 控制为远端任务分配多少 CPU。`user`（开发机）≈ 40% 逻辑 CPU；`dedicated`（构建场专用机）≈ 95% 逻辑 CPU。 |
| `--worker_id` | `hostname:port` | 该节点在 scheduler 中的唯一标识。默认自动生成为 `主机名:servant端口`，通常无需修改。 |

**关键行为说明：**

- **L1 缓存**：进程内 LRU 内存缓存，默认上限 **512 MiB**，LRU 淘汰。重启后清空。
- **L2 缓存**：连接到外部 `yadcc-cache`（若配置），命中直接返回不占用远端 CPU。
- **Bloom filter 预检**：daemon 后台每 60 秒从 `yadcc-cache` 拉取布隆过滤器，L2 查询前先过滤，避免无效 RPC。
- **Servant 并发限制**：servant 同时接受的远端任务数 = `capacity()`。`user` 模式下约为 CPU 数 × 0.40，`dedicated` 模式约为 CPU 数 × 0.95。超出容量时返回 `ResourceExhausted`，scheduler 会分配给其他节点。
- **Grant 管理**：daemon 每次分布式任务按当前 `EnvironmentDesc` 向 scheduler 申请 grant，并在等待远端编译期间定期 keep-alive。
- **自回退**：远端编译失败（网络错误、编译器版本不匹配等）时，自动在本机重新编译，**对 wrapper 透明**。
- **路径修复**：servant 编译时使用临时文件，返回 object 前会将临时路径替换为原始源文件路径，保证 debug info 和 `__FILE__` 正确。
- **内存上报**：heartbeat 中携带可用内存信息，scheduler 在机器可用内存低于 10 GiB 时不向其分配任务。

**启动示例：**

```bash
# 开发机（只做本地加速 + 贡献 40% CPU）
yadcc-daemon \
  --scheduler_uri=10.0.0.1:8336 \
  --cache_addr=10.0.0.2:8338

# 构建场专用节点（贡献 95% CPU）
yadcc-daemon \
  --servant_priority=dedicated \
  --scheduler_uri=10.0.0.1:8336 \
  --cache_addr=10.0.0.2:8338 \
  --token=my-secret-token
```

**HTTP API（本机 wrapper 接口，不对外暴露）：**

| 路径 | 方法 | 说明 |
|---|---|---|
| `/healthz` | GET | 健康检查，返回 `{"status":"ok"}` |
| `/local/get_version` | GET | 返回版本字符串 |
| `/local/submit_task` | POST | wrapper 提交编译任务（JSON body） |
| `/metrics` | GET | Prometheus metrics |

---

### yadcc-scheduler

集群中**只需部署一个** scheduler（高可用暂不支持），负责：

- 接受 daemon 节点的心跳（每 10 秒一次）
- 维护 worker 注册表（30 秒无心跳则剔除）
- 为 daemon 分配 worker grant
- 优先将任务分配给 **dedicated** 节点；同优先级内，选择**空闲 slot 最多**的节点
- 自回避：不将任务分配回发起请求的节点（除非别无选择）

**启动参数：**

```
yadcc-scheduler [flags]
```

| Flag | 默认值 | 说明 |
|---|---|---|
| `--addr` | `0.0.0.0:8336` | gRPC 监听地址。daemon 通过此地址注册和申请 grant。 |
| `--http-addr` | `0.0.0.0:8337` | HTTP debug 监听地址。提供 `/healthz`、`/scheduler/state`、`/metrics`。留空则不启动 HTTP。 |

**状态查询：**

```bash
curl http://scheduler-host:8337/scheduler/state
# 返回：{"version":"...","workers":12,"running_tasks":7}
```

**Worker 调度策略（`pickWorker`）：**

1. **内存门控**：跳过可用内存 < 10 GiB 的节点
2. **Dedicated 优先**：dedicated 节点优先于 user 节点
3. **负载均衡**：同优先级内，选 `(capacity - currentLoad)` 最大的节点
4. **自回避**：发起请求的节点放入最低优先级候选
5. **满载跳过**：`currentLoad >= capacity` 的节点不参与选择

---

### yadcc-cache

可选的分布式编译缓存服务，提供跨机器、跨构建的持久化缓存（L2）。

**启动参数：**

```
yadcc-cache [flags]
```

| Flag | 默认值 | 说明 |
|---|---|---|
| `--grpc-addr` | `0.0.0.0:8338` | gRPC 监听地址，daemon 通过此地址读写缓存。 |
| `--addr` | `0.0.0.0:8339` | HTTP 监听地址，提供 `/healthz`、`/cache/stats`、`/cache/entry`、`/cache/bloom`。 |
| `--engine` | `memory` | 缓存后端：`memory`（进程内，重启丢失）或 `disk`（持久化）。 |
| `--disk-dir` | `tmp/cache` | `--engine=disk` 时的缓存目录。建议使用绝对路径，并确保有足够空间。 |

**后端对比：**

| | `memory` | `disk` |
|---|---|---|
| 持久化 | 否（重启清空） | 是 |
| 容量限制 | 512 MiB（LRU） | 磁盘空间 |
| 推荐场景 | 测试、CI 单节点 | 长期运行、多节点共享 |
| 读写性能 | 极快 | 受磁盘 I/O 限制 |

**启动示例（磁盘模式）：**

```bash
mkdir -p /data/yadcc-cache
yadcc-cache \
  --engine=disk \
  --disk-dir=/data/yadcc-cache \
  --grpc-addr=0.0.0.0:8338 \
  --addr=0.0.0.0:8339
```

**缓存 entry 格式：**

缓存存储的不只是 object 文件，而是一个结构化 entry，包含：
- `exit_code`：编译器退出码
- `stdout`/`stderr`：编译器输出（警告信息）
- `object_file`：目标文件字节

这保证了缓存命中时**编译警告也会被重放**，行为与正常编译完全一致。

---

## 端口与地址规范

| 端口 | 协议 | 组件 | 用途 |
|---|---|---|---|
| **8334** | HTTP | daemon local | wrapper → daemon 提交任务（仅 loopback） |
| **8335** | gRPC | daemon servant | 接受远端编译任务（集群内部） |
| **8336** | gRPC | scheduler | daemon 心跳 + grant 申请 |
| **8337** | HTTP | scheduler | 健康检查 + 状态 + metrics |
| **8338** | gRPC | cache | daemon 读写缓存 |
| **8339** | HTTP | cache | 健康检查 + stats + metrics |

**防火墙建议：**

- `:8334` 只需 loopback，**禁止对外暴露**
- `:8335`、`:8336`、`:8338` 只对集群内部网段开放
- `:8337` 可对运维网段开放（Prometheus 拉取 scheduler metrics）；`:8339` 是 cache HTTP debug/API 端口

---

## 环境变量

| 变量 | 作用于 | 说明 |
|---|---|---|
| `YADCC_DAEMON_ADDR` | wrapper | 覆盖 daemon HTTP 地址，默认 `http://127.0.0.1:8334`。用于非标准端口或集成测试。 |
| `YADCC_SCHEDULER_GRPC_ADDR` | start-daemon.sh | 传给 daemon 的 `--scheduler_uri`，默认 `127.0.0.1:8336`。 |
| `YADCC_DAEMON_LOCAL_ADDR` | start-daemon.sh | daemon `--local_addr`，默认 `127.0.0.1:8334`。 |
| `YADCC_DAEMON_SERVANT_ADDR` | start-daemon.sh | daemon `--servant_addr`，默认 `0.0.0.0:8335`。 |
| `YADCC_DAEMON_PRIORITY` | start-daemon.sh | `user` 或 `dedicated`，默认 `user`。 |
| `YADCC_CACHE_GRPC_ADDR` | start-daemon.sh | daemon `--cache_addr`，默认空（不使用外部缓存）。 |
| `YADCC_TOKEN` | start-daemon.sh | 认证 token，默认 `yadcc`。 |
| `YADCC_BIN_DIR` | start-*.sh | 二进制所在目录，默认 `<repo>/bin`。 |
| `YADCC_LOG_DIR` | start-*.sh / stop-all.sh | 日志和 PID 文件目录，默认 `/tmp/yadcc-logs`。 |

---

## 部署方案

### 单机试用

单机也能从**本地 L1 缓存**获益（相同任务不重复编译）。scheduler 是可选的。

```bash
make build

# 只启动 daemon（无分布式，有 L1 缓存）
./bin/yadcc-daemon &

# 用 wrapper 编译
CC="./bin/yadcc cc" CXX="./bin/yadcc c++" make -j$(nproc)
```

### 小型集群

3～10 台机器，每台同时是 requester 和 servant。

```
节点 A（也运行 scheduler + cache）
节点 B、C...（只运行 daemon）
```

```bash
# 节点 A：启动 scheduler + cache + daemon
./bin/yadcc-scheduler --addr=0.0.0.0:8336 --http-addr=0.0.0.0:8337 &
./bin/yadcc-cache --engine=memory --grpc-addr=0.0.0.0:8338 &
./bin/yadcc-daemon \
  --scheduler_uri=节点A_IP:8336 \
  --cache_addr=节点A_IP:8338 &

# 节点 B、C...：只启动 daemon
./bin/yadcc-daemon \
  --scheduler_uri=节点A_IP:8336 \
  --cache_addr=节点A_IP:8338 &
```

### 构建场（build farm）

10 台以上，划分为"构建发起机"和"专用编译机"两类。

```bash
# 专用编译机（贡献 95% CPU，不参与普通编译）
YADCC_DAEMON_PRIORITY=dedicated \
YADCC_SCHEDULER_GRPC_ADDR=scheduler:8336 \
YADCC_CACHE_GRPC_ADDR=cache:8338 \
YADCC_TOKEN=build-secret \
  ./scripts/start-daemon.sh

# 开发机 / CI 节点（贡献 40% CPU，发起编译）
YADCC_SCHEDULER_GRPC_ADDR=scheduler:8336 \
YADCC_CACHE_GRPC_ADDR=cache:8338 \
YADCC_TOKEN=build-secret \
  ./scripts/start-daemon.sh
```

**推荐 cache 使用磁盘模式**，并挂载在高速 SSD 或 NVMe 上：

```bash
./bin/yadcc-cache \
  --engine=disk \
  --disk-dir=/nvme/yadcc-cache \
  --grpc-addr=0.0.0.0:8338
```

---

## 与构建系统集成

### CMake

**推荐方式**：通过 CMake toolchain 文件注入。

```cmake
# toolchain/yadcc.cmake
set(CMAKE_C_COMPILER   "yadcc" CACHE STRING "")
set(CMAKE_CXX_COMPILER "yadcc" CACHE STRING "")
# yadcc 以 'yadcc' 名字被调用时，会将 cc/c++ 作为第一个参数处理
```

或者通过环境变量：

```bash
CC="yadcc cc" CXX="yadcc c++" cmake -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j$(nproc)
```

或者将 wrapper 安装为符号链接（推荐，透明度最高）：

```bash
sudo ln -sf /path/to/bin/yadcc /usr/local/bin/gcc
sudo ln -sf /path/to/bin/yadcc /usr/local/bin/g++
sudo ln -sf /path/to/bin/yadcc /usr/local/bin/clang
sudo ln -sf /path/to/bin/yadcc /usr/local/bin/clang++
# 此后 cmake 自动探测并使用 wrapper，无需任何配置
```

### Make / 裸编译命令

```makefile
CC  = yadcc cc
CXX = yadcc c++

%.o: %.c
    $(CC) -c $< -o $@
```

### Bazel / Buck

通过 `--action_env` 将编译器路径指向 wrapper：

```bash
bazel build //... \
  --action_env=CC=yadcc \
  --action_env=CXX=yadcc
```

---

## 运维

### 健康检查

```bash
# daemon
curl http://127.0.0.1:8334/healthz
# {"status":"ok"}

# scheduler
curl http://scheduler-host:8337/healthz

# cache
curl http://cache-host:8339/healthz
```

### 状态查询

```bash
# scheduler 当前 worker 数和运行任务数
curl http://scheduler-host:8337/scheduler/state
# {"version":"devel","workers":8,"running_tasks":23}

# cache 统计
curl http://cache-host:8339/cache/stats
# {"entries":42137,"bytes":1234567890,"hits":380201,"misses":42137,"evicted":0}
```

### Prometheus 监控

daemon 和 scheduler 在 `/metrics` 暴露 Prometheus metrics。cache 目前维护内部指标并通过 daemon/scheduler 相关路径间接观测，HTTP cache 服务尚未暴露 `/metrics`。

**Scrape 配置示例：**

```yaml
scrape_configs:
  - job_name: yadcc_daemon
    static_configs:
      - targets: ['node1:8334', 'node2:8334']

  - job_name: yadcc_scheduler
    static_configs:
      - targets: ['scheduler:8337']

  - job_name: yadcc_cache
    static_configs:
      - targets: ['cache:8339']
```

**关键指标：**

| 指标 | 类型 | 说明 |
|---|---|---|
| `yadcc_daemon_tasks_total{outcome}` | Counter | 编译任务总数，按 outcome 分组：`cache_hit_l1`、`cache_hit_l2`、`remote`、`local`、`error` |
| `yadcc_daemon_remote_latency_seconds` | Histogram | 远端编译端到端延迟（含 grant 申请 + 网络传输 + 编译） |
| `yadcc_daemon_local_latency_seconds` | Histogram | 本地回退编译延迟 |
| `yadcc_daemon_servant_tasks_active` | Gauge | 当前本机正在执行的 servant 任务数 |
| `yadcc_scheduler_workers_active` | Gauge | 注册中的有效 worker 数量 |
| `yadcc_scheduler_grants_active` | Gauge | 当前未释放的 grant 数量（≈ 正在执行的分布式任务数） |
| `yadcc_scheduler_heartbeats_total` | Counter | 收到的 heartbeat 总数 |
| `yadcc_scheduler_wait_for_worker_seconds` | Histogram | WaitForStartingTask RPC 耗时 |
| `yadcc_cache_get_total{result}` | Counter | 缓存查询次数，`result=hit` 或 `miss` |
| `yadcc_cache_put_total` | Counter | 缓存写入次数 |
| `yadcc_cache_store_bytes` | Gauge | 缓存当前占用字节数 |
| `yadcc_cache_store_entries` | Gauge | 缓存当前条目数 |

**推荐告警：**

```yaml
# 分布式任务成功率低（可能 scheduler 不可达或 worker 不足）
- alert: YadccLowRemoteRate
  expr: |
    rate(yadcc_daemon_tasks_total{outcome="remote"}[5m]) /
    (rate(yadcc_daemon_tasks_total[5m]) + 0.001) < 0.5
  for: 10m

# Worker 全部下线
- alert: YadccNoWorkers
  expr: yadcc_scheduler_workers_active == 0
  for: 2m

# 缓存服务消失（getter RPC 全失败）
- alert: YadccCacheDown
  expr: rate(yadcc_cache_get_total[5m]) == 0 and yadcc_cache_store_entries > 0
  for: 5m
```

### 日志

所有组件使用 Go 标准 `log/slog` 输出结构化 JSON 日志到 `stderr`。

通过 `start-*.sh` 启动时，日志重定向到 `$YADCC_LOG_DIR/yadcc-{component}.log`：

```bash
# 查看 daemon 日志
tail -f /tmp/yadcc-logs/yadcc-daemon.log

# 停止所有组件
./scripts/stop-all.sh
```

---

## 缓存机制

### L1 内存缓存

- **范围**：每个 `yadcc-daemon` 进程独立
- **容量**：512 MiB，LRU 淘汰（`container/list` + `map` 实现）
- **生命周期**：进程重启后清空
- **命中延迟**：< 1ms（纯内存）

### L2 外部缓存（yadcc-cache）

- **范围**：集群内所有节点共享
- **容量**：取决于后端（memory 模式 512 MiB，disk 模式无限制）
- **生命周期**：memory 模式重启清空；disk 模式持久化
- **Bloom filter 预检**：daemon 后台维护布隆过滤器，key 明确不在缓存时直接跳过 RPC，避免无意义的网络请求

### 缓存 key 组成

缓存 key 是以下所有字段的 SHA-256 哈希：

| 字段 | 说明 |
|---|---|
| `compiler_digest` | 编译器二进制的 SHA-256（保证编译器版本隔离） |
| `compiler_kind` | 语言类型：`c` 或 `c++` |
| `host_os` / `host_arch` | 操作系统和架构（`linux/amd64` 等） |
| `target_triple` | `-target` 参数值（交叉编译时不同） |
| `abi` | 从 `-m32`/`-m64` 推断：`ilp32`/`lp64`/空 |
| `sysroot_digest` | sysroot 目录文件布局的哈希（`-isysroot`/`--sysroot`） |
| `stdlib_digest` | C++ 标准库路径和版本的哈希（`-stdlib=`） |
| `arguments` | 归一化后的编译参数（去除 `-o`、`-MF` 等输出相关 flag） |
| `source_digest` | 预处理后源文件的 SHA-256 |

**参数归一化**：以下 flag 不计入 key（不影响 object 内容）：
- `-o <output>` 输出路径
- `-MF`/`-MT`/`-MQ` 依赖文件相关
- `-MD`/`-MMD`/`-MP`/`-MG` 依赖生成

### 不可缓存任务

以下情况的编译结果**不会被缓存**，每次都重新编译：

1. **使用了时间相关宏**：预处理源码中出现 `__TIME__`、`__DATE__` 或 `__TIMESTAMP__`（且未被 `-D` 全部覆盖）。
2. **编译器非零退出**：编译失败不缓存。
3. **无 object 输出**：例如仅生成依赖文件。

**例外**：若所有三个宏都通过 `-D` 显式覆盖（如 `-D__TIME__="00:00:00" -D__DATE__="Jan 1 2000" -D__TIMESTAMP__="..."`），则结果视为确定性，可以缓存。

---

## 安全注意事项

> **当前版本不提供加密传输**。所有 gRPC 连接使用明文（`insecure.NewCredentials()`）。

**部署建议：**

1. **网络隔离**：将 `:8335`、`:8336`、`:8338` 端口限制在集群内网，不对公网暴露。
2. **认证边界**：当前 `--token` 还不是完整认证机制，不能阻止非授权节点加入。生产部署必须依赖内网隔离、防火墙或外层 mTLS/代理控制访问。
3. **本地端口**：`:8334` 仅监听 loopback，无需额外保护，但 `--local_addr` **不要改为** `0.0.0.0`。
4. **磁盘缓存权限**：`--disk-dir` 目录应限制为 yadcc-cache 进程的运行用户可读写，避免其他用户篡改缓存。

---

## 性能调优

**调大 servant 并发数：**

dedicated 模式的容量 = `floor(CPU数 × 0.95)`，但如果机器上除 yadcc-daemon 外还有其他负载，考虑用 `user` 模式（`× 0.40`）。

**调整 L1 缓存大小：**

当前 L1 上限写死为 512 MiB。如果内存充裕且项目重复编译率高，可以在 `internal/cache/store.go` 中修改 `DefaultMemoryStoreMaxBytes`，然后重新构建。

**使用 disk 缓存：**

对于 CI 环境（每次 clean build），只有 L2 disk 缓存有意义。确保 `yadcc-cache` 与编译节点之间网络延迟 < 5ms，否则 L2 缓存反而可能比本地编译慢。

**预处理并发限制：**

daemon 的 `ppSem` 大小 = `NumCPU`。如果机器同时运行大量构建进程导致 daemon 过载，它会优先让任务本地执行，而不是排队等待，保证不阻塞构建。

**Grant 申请：**

daemon 当前按任务实时申请 grant，确保 scheduler 使用本次任务的 `EnvironmentDesc` 做匹配。后续如重新引入 grant 预取，需要按环境维度分池，避免拿到不兼容 worker。

---

## 已知限制

| 限制 | 说明 |
|---|---|
| 与 C++ 原版缓存不兼容 | Go 版使用 SHA-256，C++ 版使用 BLAKE3，缓存 key 格式不同，无法共享 |
| gRPC 明文传输 | 暂不支持 TLS，需依赖网络层隔离 |
| 认证未完成 | `--token` 目前不是完整鉴权实现，需依赖网络层隔离 |
| Scheduler 无高可用 | 单点，scheduler 宕机时所有分布式编译失败（自动回退本地） |
| 仅支持 C/C++ | 不支持 Rust、Go、Fortran 等其他语言 |
| 不支持 `-pipe` / stdin 编译 | 有 `-`（stdin）参数时自动回退本地 |
| coverage/profile 多产物场景本地回退 | `--coverage`、`-fprofile-arcs`、`-ftest-coverage` 等需要额外产物，当前不分发 |
| macOS 可用内存读取精度低 | macOS 下 `AvailableMemoryBytes` 仅能读取 free pages，不含可回收缓存 |

---

## 开发

```bash
# 格式化
make fmt

# 单元测试
make test

# 全量集成测试（需要 gcc/clang）
go test -v -tags integration ./tests/integration/

# CMake e2e 测试
cd tests/cmake_project && mkdir build && cd build
CC="../../bin/yadcc cc" CXX="../../bin/yadcc c++" cmake .. && make && ctest

# 修改 proto 后重新生成
make proto-gen
```

**项目结构：**

```
api/
  proto/yadcc/v1/     proto 源文件
  gen/yadcc/v1/       生成的 Go 代码（不要手动修改）
cmd/
  yadcc/              wrapper 入口
  yadcc-daemon/       daemon 入口（含 flag 定义）
  yadcc-scheduler/    scheduler 入口
  yadcc-cache/        cache 入口
internal/
  cache/              LRU store、bloom filter、gRPC 服务端、entry 序列化
  client/             wrapper 主逻辑
  compiler/           参数解析、预处理、编译器摘要、sysroot digest
  compress/           zstd 压缩/解压
  daemon/             统一 daemon（local + servant）
  metrics/            Prometheus metric 定义
  objpatch/           远端 object 路径修复
  platform/           OS 抽象（execv、PATH 查找）
  protocol/           协议辅助
  scheduler/          scheduler 服务端
  sysinfo/            跨平台系统资源读取
  taskgroup/          singleflight 风格的任务去重
scripts/
  start-daemon.sh
  start-scheduler.sh
  stop-all.sh
tests/
  cmake_project/      CMake e2e 测试工程
  integration/        Go 集成测试（-tags integration）
```
