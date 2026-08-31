# 多链 RPC 网关（Multichain RPC Gateway）

> [English](./README_EN.md) | **中文**

一个用 Go 编写的 JSON-RPC 2.0 网关，对所有已支持的链暴露**单一 HTTP 端口**：客户端通过 `X-Chain-Id` HTTP 头（十进制 chain id）选择目标链，网关解析链、将请求转发到对应上游，并将结果按原样回显（`id` 逐字节保留）。完整支持 JSON-RPC 2.0 批量请求（有序响应、通知语义、按元素的链覆盖），并在纯转发之上提供韧性能力：每条链多上游 + 基于健康探测的选择、故障上游自动 failover、安全方法限定的有界指数退避重试（`eth_sendRawTransaction` 永不重试）、以及对连续失败上游的熔断隔离直至探测恢复。运维侧提供 Prometheus 指标（`GET /metrics`）覆盖请求量、错误率、时延分位数、健康与熔断状态，以及 payload 与敏感信息脱敏的结构化 JSON 日志。v1 不支持 WebSocket（`eth_subscribe` 返回 -32601）。

## 架构

```
                 +--------------------------------------------------+
                 |   client (any JSON-RPC 2.0 library: viem,        |
                 |   ethers, web3.js, curl)                         |
                 +------------------------+-------------------------+
                                          |  POST /  with X-Chain-Id header
                                          v
                 +--------------------------------------------------+
                 |              multichain-rpc-gateway (:8545)      |
                 |  +--------+   +--------+   +-----------------+   |
                 |  |  api   |-->| router |-->|  upstream pool  |   |
                 |  | jsonrpc|   | (chain |   |  (failover,     |   |
                 |  |        |   |  res.) |   |   retry, breaker)|  |
                 |  +--------+   +--------+   +-----------------+   |
                 |  prober (eth_chainId probes)                     |
                 |  metrics (:9090)   logging (redacted slog)       |
                 +----+--------------------------+------------------+
                      |  chain 1                  |  chain 8453
                      v                           v
                 +----------------+        +----------------+
                 | Ethereum       |        | Base           |
                 | upstreams      |        | upstreams      |
                 | (mainnet-a...) |        | (base-a...)    |
                 +----------------+        +----------------+
```

所有配置集中在单个 YAML 文件；新增一条链只需增加一条配置 + 在 `internal/chain` 新增一个 adapter，路由核心无需改动。

| 包 | 职责 |
|---|---|
| `cmd/gateway` | 唯一二进制。参数（`-config`、`-log-level`）、配置加载、日志/指标/路由/API 组装、HTTP 服务、SIGINT/SIGTERM 优雅停机 |
| `internal/api` | 单一 `POST /` JSON-RPC HTTP 处理器（请求体大小与批量元素上限、信封校验、响应写入） |
| `internal/config` | YAML 加载与 `${VAR}` 环境变量替换、校验、拒绝未知字段 |
| `internal/jsonrpc` | 手写 JSON-RPC 2.0 信封解析/序列化与错误码（不依赖第三方 RPC 库） |
| `internal/chain` | 适配器注册表；`ethereum` 与 `base` 适配器通过 `init()` 自注册 |
| `internal/router` | 基于 `X-Chain-Id` 的链解析、按链的上游选择、`RoutingRecord`（日志与指标的数据源） |
| `internal/upstream` | HTTP 客户端与转发、failover、有界退避重试、熔断器 |
| `internal/prober` | 固定间隔的 `eth_chainId` 主动健康探测；驱动健康状态与熔断器 |
| `internal/metrics` | Prometheus 采集器（见下方[指标](#指标)） |
| `internal/logging` | 带 payload 与敏感信息脱敏的 `slog` 结构化日志 |

## 快速开始

前置要求：Go >= 1.26。无需真实 RPC 凭证——演示使用仓库内置的 mock 上游（`cmd/mockupstream`），无外部依赖。

```bash
make demo   # 启动 2 个 mock 上游（chain 1 / 8453）+ 网关
```

预期：网关监听 `:8545`，指标在 `:9090`；健康检查返回 `ok`：

```bash
curl -s localhost:9090/healthz
```

向 Base（chain 8453）路由请求——结果来自 Base 的 mock 上游：

```bash
curl -s localhost:8545 -H 'Content-Type: application/json' \
  -H 'X-Chain-Id: 8453' \
  -d '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}'
# -> {"jsonrpc":"2.0","id":1,"result":"0x2105"}
```

未知链会被网关保留错误码段直接拒绝，且不会转发：

```bash
curl -s localhost:8545 -H 'X-Chain-Id: 999' \
  -d '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":2}'
# -> {"jsonrpc":"2.0","id":2,"error":{"code":-32000,...}}
```

标准客户端库可无改动使用——网关表现与直连链 RPC 完全一致。见下方 viem 示例。

自动化校验与性能目标（与 `specs/001-multichain-rpc-routing/quickstart.md` 一致）：

| 目标 | 说明 |
|---|---|
| `make test` | `go test ./...` —— 单测（路由、校验、熔断、退避）+ 双链进程内集成测试 |
| `make test-conformance` | JSON-RPC 2.0 一致性向量（id 回显、result/error 互斥、批量顺序、通知、标准错误码） |
| `make lint` | `go vet ./...` + `gofmt -l .`（与 CI 一致的门禁） |
| `make bench` | 进程内透传开销基准（直连 vs 网关，p50 预算 +20%） |
| `make load` | vegeta 持续压测（1,000 req/s，网关一轮 + 直连基线一轮） |
| `make demo-failover` | 基于 mock 上游故障注入的端到端 failover 演示 |

## 客户端示例（viem）

网关是一个开箱即用的 HTTP JSON-RPC 端点。viem 客户端唯一需要做的是在每次请求中携带 `X-Chain-Id` 头以告知网关路由到哪条链。viem 的 `http()` 传输支持自定义 `fetchOptions`，是注入该头的推荐方式：

```js
// example.mjs — 运行：node example.mjs   (npm i viem)
import { createPublicClient, defineChain, http } from "viem";

// 网关要求每个请求都携带该头：用于选择目标链（十进制 chain id，如 Base 为 "8453"）。
const base = defineChain({
  id: 8453,
  name: "Base",
  nativeCurrency: { name: "Ether", symbol: "ETH", decimals: 18 },
  rpcUrls: { default: { http: ["http://localhost:8545"] } },
});

const client = createPublicClient({
  chain: base,
  transport: http("http://localhost:8545", {
    fetchOptions: {
      headers: { "X-Chain-Id": "8453" }, // 必需：路由到 chain 8453
    },
  }),
});

const chainId = await client.getChainId();      // -> 8453n
const blockNumber = await client.getBlockNumber(); // -> 最新块高
console.log({ chainId: chainId.toString(), blockNumber: blockNumber.toString() });
```

先执行 `make demo` 启动演示，再运行 `node example.mjs`。同理适用于任意链：把 URL 指向网关，将 `X-Chain-Id` 设为目标链 id，并保持 chain 对象的 `id` 一致。缺少该头或 chain id 未知时，请求会被网关错误（-32000）拒绝且不会转发。

## 配置

配置在启动时从 YAML 文件加载（`-config`，默认 `config.yaml`；见 `config.example.yaml`）。`${VAR}` 占位符在解析前通过环境变量替换——未设置即报错，未知 YAML 字段会被拒绝。`config.yaml` 本身被 gitignore，仅 `config.example.yaml` 会被提交，不会提交任何真实 URL 或密钥。v1 为启动时一次性加载：**不支持热重载**，改动需重启生效。

| 段 | 字段 | 默认值 | 说明 |
|---|---|---|---|
| `server` | `listen` | 必需 | JSON-RPC 监听地址，如 `:8545` |
| `server` | `metrics_listen` | 必需 | `/metrics` + `/healthz` 监听地址，如 `:9090` |
| `server` | `max_batch_elements` | `100` | 单次请求最大 JSON-RPC 批量元素数 |
| `server` | `max_body_bytes` | `1048576` (1 MB) | 最大请求体大小 |
| `server.timeouts` | `default` | `10` | 单次上游尝试超时（秒） |
| `server.timeouts` | `eth_getLogs` | — | 方法级超时覆盖（前缀匹配，最长匹配生效） |
| `prober` | `interval` | `10s` | 主动健康探测间隔 |
| `prober` | `timeout` | `5s` | 单次探测超时 |
| `prober` | `fail_threshold` | `3` | 连续失败 N 次后标记为不健康 |
| `retry` | `max_attempts` | `2` | 安全方法的最大尝试次数（含首次） |
| `retry` | `base_delay` | `10ms` | 指数退避基线时延 |
| `retry` | `max_elapsed` | `30s` | 重试总截止时间 |
| `circuit` | `fail_threshold` | `5` | 连续失败 N 次后打开熔断器 |
| `circuit` | `cooldown` | `30s` | 打开 → 半开的冷却时间 |
| `chains[]` | `chain_id` | 必需 | 十进制 chain id 字符串，需唯一 |
| `chains[]` | `adapter` | 必需 | 已注册的适配器名（`ethereum`、`base`） |
| `chains[].upstreams[]` | `name` | 脱敏后的 URL | 上游的日志/指标别名 |
| `chains[].upstreams[]` | `url` | 必需 | 上游地址，`http`/`https` |

网关启动参数：`-config <path>`（默认 `config.yaml`）与 `-log-level <debug|info|warn|error>`（默认 `info`）。

## 指标

Prometheus 格式指标暴露在指标监听器的 `GET /metrics`（演示中为 `:9090`）；存活/就绪探针在 `GET /healthz`。所有网关指标以 `gateway_` 为前缀。`gateway_request_duration_seconds` 的桶在低时延区间更密集，便于通过 `histogram_quantile` 计算 p50/p95/p99。

| 指标 | 类型 | 标签 |
|---|---|---|
| `gateway_requests_total` | counter | `chain`, `upstream`, `method`, `outcome` |
| `gateway_request_duration_seconds` | histogram | `chain`, `upstream`, `method` |
| `gateway_requests_inflight` | gauge | `chain`, `upstream` |
| `gateway_upstream_up` | gauge（0 不健康 / 1 健康 / 2 未知） | `chain`, `upstream` |
| `gateway_upstream_probe_latency_seconds` | gauge | `chain`, `upstream` |
| `gateway_upstream_circuit_state` | gauge（0 关闭 / 1 打开 / 2 半开） | `chain`, `upstream` |

示例：

```bash
curl -s localhost:9090/metrics | grep gateway_
# -> gateway_requests_total{chain="8453",upstream="base-a",method="eth_chainId",outcome="success"} 1
```

## 性能基准

通过 `make bench` 测量（`go test -bench . -benchtime=10s -count=5 ./bench/`）——基线与网关保持相同的两跳 TCP 路径（bench → server → mock 上游），差值即为网关流水线的增量开销（解析 + 链解析 + 指标/日志），不含上游本身时延（FR-017 / SC-002）。

**环境：** `linux/amd64`、`AMD Ryzen 7 5800H with Radeon Graphics (16 核)`、`Go 1.26.5`、`bench/passthrough_test.go`（mock 上游回显 `{"jsonrpc":"2.0","id":<id>,"result":"0x1"}`）。

**最新运行（2026-08-31，`tee /tmp/bench.txt`）：**

| 轮次 | BenchmarkPassthrough p50_ns/op | ns/op | BenchmarkGateway p50_ns/op | ns/op | Δ p50 |
|-----|-------------------------------:|------:|---------------------------:|------:|------:|
| 1 | 1 009 530 | 1 052 340 | 1 152 854 | 1 182 407 | +14.2% |
| 2 | 1 000 524 | 1 020 564 | 1 127 497 | 1 158 025 | +12.7% |
| 3 |   996 465 | 1 016 416 | 1 096 123 | 1 112 370 | +10.0% |
| 4 |   995 077 | 1 015 483 | 1 142 843 | 1 182 548 | +14.8% |
| 5 | 1 057 237 | 1 081 621 | 1 182 315 | 1 228 226 | +11.8% |

- 中位数 p50：基线 **1 000 524 ns** → 网关 **1 142 843 ns**，开销 **+142 319 ns (+14.2%)** ≤ 预算 **+20%** ✅
- 均值 p50：1 011 767 ns → 1 140 326 ns（**+12.7%**）—— 同样在预算内
- 均值 ns/op（timer）：1 037 285 ns → 1 172 715 ns（**+13.1%**）

5 轮均单独通过（最差 +14.8% / 按 ns/op 计 +18.8% 的第 5 轮仍 < +20%）。此前验收（T049）为 +8.2%（更快宿主机）；波动与宿主机/环境负载相关，但预算始终满足。

复现：

```bash
make bench
# 原始命令：go test -bench . -benchtime=10s -count=5 ./bench/ | tee /tmp/bench.txt
```

完整原始输出（`/tmp/bench.txt`）：

```
BenchmarkPassthrough-16    10000    1052340 ns/op    1009530 p50_ns/op
BenchmarkPassthrough-16    10000    1020564 ns/op    1000524 p50_ns/op
BenchmarkPassthrough-16    11757    1016416 ns/op     996465 p50_ns/op
BenchmarkPassthrough-16    11599    1015483 ns/op     995077 p50_ns/op
BenchmarkPassthrough-16    10000    1081621 ns/op    1057237 p50_ns/op
BenchmarkGateway-16         9708    1182407 ns/op    1152854 p50_ns/op
BenchmarkGateway-16        10000    1158025 ns/op    1127497 p50_ns/op
BenchmarkGateway-16        10000    1112370 ns/op    1096123 p50_ns/op
BenchmarkGateway-16        10000    1182548 ns/op    1142843 p50_ns/op
BenchmarkGateway-16         8229    1228226 ns/op    1182315 p50_ns/op
```

p50 计算见 `bench/passthrough_test.go:reportP50`，CI 门禁见 `specs/001-multichain-rpc-routing/quickstart.md §4` / `.github/workflows/ci.yml`。

## 参考资料

- 功能规格：`specs/001-multichain-rpc-routing/spec.md`
- 端到端验证手册：`specs/001-multichain-rpc-routing/quickstart.md`
- 契约（规范性）：`specs/001-multichain-rpc-routing/contracts/` —— `jsonrpc-api.md`（JSON-RPC 信封与错误码，含网关保留段 **-32000..-32005**：未知链 -32000、上游不可用 -32001、批量超限 -32003、请求体过大 -32004、超时 -32005）、`config-contract.md`、`metrics-contract.md`
- 数据模型与路由顺序：`specs/001-multichain-rpc-routing/data-model.md`
- 架构决策记录：`docs/adr/`
