# Contract: Prometheus Metrics

**Branch**: `001-multichain-rpc-routing` | **Date**: 2026-08-13

> 指标契约：`GET /metrics`（promhttp，标准文本格式）。命名遵循 Prometheus 约定：`gateway_` 前缀、snake_case、计数器后缀 `_total`、单位后缀 `_seconds`。label 值不含 payload 内容，上游 label 使用别名或脱敏 URL。

## 1. 指标清单

### 请求维度（FR-014 核心）

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `gateway_requests_total` | Counter | `chain`, `upstream`, `method`, `outcome` | 每链/每上游/每方法的请求数与结果；`outcome` ∈ {`success`, `-32000`, `-32001`, `-32002`, `-32003`, `-32004`, `-32005`, `-32600`, `-32700`, …}；请求率与错误率由此导出 |
| `gateway_request_duration_seconds` | Histogram | `chain`, `upstream`, `method` | 端到端延迟（含上游 RTT）；自定义低段加密桶 `[0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]`；p50/p95/p99 由 `histogram_quantile` 计算；若 `method` label 基数失控，收敛为 `chain`+`upstream` 两维（见 §2） |
| `gateway_requests_inflight` | Gauge | `chain`, `upstream` | 当前进行中（已接收入站、尚未完成响应）的请求数；SC-006 负载验收的队列增长有界性证据 |

### 健康与熔断（FR-007/008/012）

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `gateway_upstream_up` | Gauge | `chain`, `upstream` | 0 = unhealthy，1 = healthy（probe 判定），2 = unknown（初始态：可用但最低优先级） |
| `gateway_upstream_probe_latency_seconds` | Gauge | `chain`, `upstream` | 最近一次探测往返耗时 |
| `gateway_upstream_circuit_state` | Gauge | `chain`, `upstream` | 0 = closed，1 = open，2 = half-open |

### 进程

- `go_*` / `process_*`：`collectors.NewGoCollector()` + `NewProcessCollector()` 标准运行时指标

## 2. 使用要点

- **不用 Summary**（不可跨实例聚合）；分位数全部 `histogram_quantile(0.5, sum by (le) (rate(...)))`
- `method` label 基数可控（JSON-RPC 方法集 ~数十个）；若未来基数失控，指标面收敛为 `chain`+`upstream` 两维
- 基准场景：`gateway_request_duration_seconds` 直连对比见 [quickstart.md](../quickstart.md) 基准节
- 示例 Grafana 面板配置（可选交付物，JSON）：每链 QPS、错误率、p50/p95、上游健康热力表

## 3. 禁止事项

- label 值禁止出现：请求/响应 payload、params 内容、地址、原始 secret、上游 URL 凭证段
- 禁止高基数 label（如 `id`、`block_number`、用户地址）
