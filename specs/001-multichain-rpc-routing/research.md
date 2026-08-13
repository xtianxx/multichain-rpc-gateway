# Phase 0 Research: Multichain RPC Gateway Core

**Branch**: `001-multichain-rpc-routing` | **Date**: 2026-08-13

> 本文档汇总 Phase 0 研究结论。技术栈库选型见「技术栈选型」节（基于 @librarian 调研），协议与设计决策见后续各节。

## 1. 技术栈选型

**Decision**: Go（用户确认，2026-08-13 会话）。

**Rationale**: 单节点 ~1,000 req/s 持续吞吐 + 大量上游扇出 + 超时/熔断管理是 Go 的舒适区；geth/erigon 同生态；标准库 HTTP 生产级；Prometheus 官方 client 一等公民；开发效率与性能平衡最佳，符合简历工程展示定位。

**Alternatives considered**:
- Rust (tokio/axum)：性能/内存最优，但开发速度慢、生态较 Go 弱，影响 v1 交付节奏
- TypeScript (Node.js)：与 ethers/viem 同语言，但单线程事件循环在高扇出 + 超时管理下复杂度更高
- Python (FastAPI)：开发最快，但 1,000 req/s 目标下性能余量小，威胁 SC-002/SC-006

> **ADR 要求**（宪法）：本决策须以 Architecture Decision Record 形式写入 `docs/adr/0001-language-runtime-go.md`（tasks 阶段实施）。

### 1.1 库级选型（@librarian 调研，2026-08-13 核实版本）

| 关注点 | 调研结论 | 版本 |
|--------|---------|------|
| HTTP 路由层 | 标准库 net/http 增强版 ServeMux（方法路由 + `"POST /{$}"` 精确匹配），零依赖；仅 3 个端点（POST /、GET /metrics、GET /healthz），不需要 chi | Go 1.22+ |
| JSON-RPC 处理 | **手写薄层**：encoding/json + `json.RawMessage` envelope（id 逐字节 echo）、batch 保序、notification 语义；不用任何 JSON-RPC 库（均为方法注册模型，与透传代理方向相反；geth rpc 有 LGPL/GPL 许可负担） | stdlib |
| 上游 HTTP 客户端 | 标准库 http.Client + 每上游独立 Transport 连接池（MaxIdleConnsPerHost ≥32、IdleConnTimeout 90s、Dial/TLS 超时 10s）；每请求 `context.WithTimeout` 按方法类配置；不用 fasthttp | stdlib |
| 熔断器 | sony/gobreaker **v2**（泛型化、滚动窗口分桶、IsExcluded）；健康探测直接走 `cb.Execute()` | v2.4.0 (2026-01) |
| 重试与退避 | cenkalti/backoff **v4**（ExponentialBackOff + RandomizationFactor 抖动 + MaxElapsedTime；`backoff.Permanent` 短路写方法）| v4.3.0 (2024-01) |
| Prometheus 指标 | prometheus/client_golang + `promhttp.Handler()`；延迟用 **HistogramVec**（labels: chain, upstream, method）+ 低段加密自定义桶 `[0.1ms … 10s]`；用 Histogram 不用 Summary | v1.24.1 (2026-07) |
| 结构化日志 | 标准库 log/slog JSON handler，唯一日志依赖；白名单字段 + `ReplaceAttr` 双重脱敏；不用 zerolog/zap（热路径日志量趋零，性能差异无关） | stdlib (≥1.21) |
| YAML 配置 | gopkg.in/yaml.v3 + **手写** `${VAR}` 替换（yaml.Unmarshal 前正则替换、未设置即 fail-fast；不用 os.ExpandEnv 避免误伤 `$`） | v3.0.1 |
| 基准测试 | 进程内 `testing.B` 测增量延迟（BenchmarkDirect vs BenchmarkGateway 对比 httptest 上游）+ **vegeta** 做 1,000 req/s 持续负载（两侧 p50 对比） | v12.13.0 |
| Go 版本 | go 1.26 线（go1.26.5，2026-07）；Go 1.27 若发布可直接升级，代码无需改动 | go1.26.5 |

**依赖合计：4 个第三方库**（gobreaker、backoff、client_golang、yaml.v3），全部活跃维护、零传递依赖负担，符合宪法「成熟 + YAGNI」双约束。

**Sources**: go.dev/VERSION · go.dev/doc/devel/release · pkg.go.dev（prometheus/client_golang、sony/gobreaker/v2、cenkalti/backoff/v4、go-chi/chi/v5、gopkg.in/yaml.v3、go-ethereum/rpc、valyala/fasthttp、a8m/envsubst、tsenart/vegeta/v12）· prometheus.io/docs/practices/histograms/

## 2. 协议设计决策

### 2.1 批量元素链覆盖字段名

**Decision**: `x-chain-id`（批量子元素可选字段，覆盖 `X-Chain-Id` 头）。

**Rationale**: 与 HTTP 头命名对称、语义直观；`x-` 前缀明确标记为网关扩展字段，不与 JSON-RPC 2.0 标准成员（`jsonrpc`/`method`/`params`/`id`）冲突，也避免与 `eth_chainId` 的 `chainId` 语义混淆。

**Alternatives considered**: `chainId`（易与 eth_chainId 语义混淆）、`_chainId`（下划线前缀约定不直观）、`target`（过于泛化）。

**Rules**:
- 接受 JSON string 或 number，归一化为十进制字符串后按配置校验
- 该字段是网关寻址元数据：校验后**剥离，不转发**给上游（上游只收到干净的标准 JSON-RPC 信封）
- 无 override → 继承 `X-Chain-Id` 头；头缺失且无 override → 按未知链处理（-32000）

### 2.2 网关错误码分配（-32000 ~ -32099）

**Decision**: 固定分配如下（写入 contracts）：

| 码 | 名称 | 含义 | HTTP 状态 |
|----|------|------|-----------|
| -32000 | Chain not configured | 未知链 id / 未配置上游 / 头缺失 | 200（JSON-RPC 语义） |
| -32001 | Upstream unavailable | 该链所有上游不可用/熔断全开 | 200 |
| -32002 | Invalid upstream response | 上游返回非 JSON 或非法 JSON-RPC 响应 | 200 |
| -32003 | Batch too large | 批量元素数超上限（默认 100） | 200 |
| -32004 | Request body too large | 请求体超上限（默认 1 MB） | 200 |
| -32005 | Upstream timeout | 上游超时且无可 failover 目标 | 200 |

**Rationale**: 覆盖 spec 中所有网关特有失败场景（FR-002/FR-006/FR-019、Edge Cases），保持稳定契约便于客户端处理；-32603 internal error 保留给规范语义。

**Alternatives considered**: 只用一个 -32000 带 data 细分（丢失代码级可编程性）；复用 -32603（违反规范语义）。

### 2.3 演示链阵容

**Decision**: Ethereum mainnet (chain id `1`) + Base (chain id `8453`)，纯配置驱动。

**Rationale**: 与 spec 验收场景（US1 场景 1、US2 场景 1）一致；Base 覆盖 L2 场景，证明「加链 = 配置 + adapter」且 L2 无特殊路由逻辑。

**Adapters**: `ethereum`（chain 1，参考实现）、`base`（chain 8453）。EIP-1898 块参数归一化（`{blockNumber: "0x..."}` → `"0x..."`、`"pending"`/`"latest"`/`"earliest"` 透传）在各 adapter 中实现；v1 中两个 adapter 行为一致，结构上验证「adapter 隔离」架构（SC-005）。

### 2.4 重试策略参数（FR-010/FR-011）

**Decision**:
- 仅安全方法可重试：JSON-RPC 方法名前缀白名单（如 `eth_call`、`eth_get*`、`eth_estimateGas`、`eth_blockNumber`、`eth_chainId`、`net_version`、`web3_*` 等）；`eth_sendTransaction`/`eth_sendRawTransaction` 显式黑名单，永不自动重试
- 最多 2 次尝试（1 次重试）+ 指数退避（基数 10ms，×2）+ 全抖动，总截止时间 30s（可配置）

**Rationale**: 有界、可预测；满足 spec「限次、指数退避+抖动、硬上限」。

## 3. 关键实体（与 spec Key Entities 对应，详见 data-model.md）

- **Chain**：chain id（十进制字符串规范形）、adapter、upstreams 列表
- **Upstream**：URL、健康状态（probe 驱动）、延迟读数、熔断状态
- **Routing record**：chain、method、upstream、latency、outcome（瞬时，日志/指标源）
- **Health probe result**：可用性 + 延迟读数

## 4. 配置格式（FR-015、Assumptions）

**Decision**: 单一 YAML 文件 + `${ENV_VAR}` 语法替换 secret 值；v1 无热重载，改动重启生效。

**Rationale**: spec 已确认方向；`${VAR}` 语法是运维最熟悉的约定。

```yaml
server:
  listen: ":8545"
  metrics_listen: ":9090"
  max_batch_elements: 100
  max_body_bytes: 1048576
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - url: "${ETH_MAINNET_RPC_URL}"
  - chain_id: "8453"
    adapter: base
    upstreams:
      - url: "${BASE_RPC_URL}"
```

## 5. 结论状态

- 全部 NEEDS CLARIFICATION 已解决：语言（Go 1.26）、库选型（§1.1）、override 字段名、错误码、演示链、重试参数、配置格式
- 待办移交 tasks 阶段：ADR 写入 `docs/adr/`、config.example.yaml、mock 上游、一致性向量表、基准实现
