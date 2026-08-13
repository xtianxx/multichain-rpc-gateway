# Contract: JSON-RPC 2.0 Gateway API

**Branch**: `001-multichain-rpc-routing` | **Date**: 2026-08-13

> 网关对外暴露的客户端契约。目标客户端：ethers.js / viem / web3.js 标准库，无需客户端侧修改。

## 1. Endpoints

| 端点 | 方法 | 用途 |
|------|------|------|
| `/` | POST | 唯一 JSON-RPC 2.0 入口（单请求与 batch） |
| `/healthz` | GET | 存活/就绪探测（200 + 纯文本） |
| `/metrics` | GET | Prometheus 文本格式指标（见 [metrics-contract.md](./metrics-contract.md)） |

v1 无 WebSocket；`eth_subscribe` 类订阅方法返回 -32601 Method not found。

## 2. 链寻址

- **默认链**：HTTP 头 `X-Chain-Id`（十进制字符串，如 `8453`）
- **批量 override**：批量子元素可选字段 `x-chain-id`（string 或 number），优先级高于头；无 override 时继承头
- **无头且无 override** → 按未知链处理：错误 -32000（见 §6）
- `x-chain-id` 为网关寻址元数据：校验后剥离，**不转发**给上游

## 3. 请求信封（与 JSON-RPC 2.0 规范一致）

```json
{"jsonrpc": "2.0", "method": "eth_getBalance", "params": ["0x...", "latest"], "id": 1}
```

- `jsonrpc` 必须为 `"2.0"`；`method` 必须为 string；`params` 可选（array 或 object）；`id` 为 string / number / null
- **Notification**：无 `id` 成员 → 不产生响应元素（batch 中亦然）
- 单请求顶部 body 为对象；batch 为数组；首 token 判定
- 未知方法（`eth_foo`）：**必须转发上游**（网关不透传方法表，上游可能支持扩展方法）；上游返回 -32601 时原样回传

## 4. 响应信封

- `id` 逐字节回显（`json.RawMessage` 原样），string/number/null 精确一致
- `result` 与 `error` 互斥
- **Batch**：响应数组严格按请求顺序；notification 不占位；空响应（全 notification）→ 返回空 body（HTTP 200）
- **空 batch `[]`** → -32600 单错误对象

## 5. HTTP 语义

| 场景 | HTTP 状态 | Body |
|------|-----------|------|
| body 非法 JSON（parse error） | 400 | `{"jsonrpc":"2.0","error":{"code":-32700,...},"id":null}` |
| body 超大小上限（-32004） | 400 | 同上，code -32004 |
| 其余一切（含网关/上游错误、batch 超限） | 200 | JSON-RPC 错误对象/数组 |

- Content-Type 宽松处理：不强制校验头，直接解析 body
- 每个请求有界截止时间（默认 10s，`eth_getLogs` 类 30s）；超时按 -32005 处理，绝不挂起

## 6. 错误码表

### 标准码（JSON-RPC 2.0 规范）

| 码 | 含义 | 触发 |
|----|------|------|
| -32700 | Parse error | body 非法 JSON |
| -32600 | Invalid Request | 信封结构非法、空 batch |
| -32601 | Method not found | （上游回传原样透传；`eth_subscribe` 网关直接返回） |
| -32602 | Invalid params | （上游回传原样透传） |
| -32603 | Internal error | 未预期内部错误 |

### 网关码（保留区间 -32000 ~ -32099，本功能固定分配）

| 码 | 名称 | 含义 |
|----|------|------|
| -32000 | Chain not configured | 未知/未配置链；头与 override 均缺失 |
| -32001 | Upstream unavailable | 该链所有上游不可用或熔断全开 |
| -32002 | Invalid upstream response | 上游返回非 JSON / 非法 JSON-RPC 响应 / id 不匹配 |
| -32003 | Batch too large | 批量元素数 > `max_batch_elements`（默认 100） |
| -32004 | Request body too large | body > `max_body_bytes`（默认 1 MB） |
| -32005 | Upstream timeout | 上游超时且无 failover 目标 / 总截止超时 |

错误对象 shape：`{"code": N, "message": "<稳定英文描述>", "data": {…}}`；`data` 在网关码下携带结构化上下文（如 `{"chain_id":"999"}`，chain_id 采用配置规范形）。网关码的 message 为稳定契约，客户端可编程依赖 code。

## 7. Batch 语义汇总

1. 解析为 `[]json.RawMessage`，逐元素处理
2. 每元素先校验（含 notification：先校验后转发，但无响应元素）——畸形元素不得转发
3. 链解析：`x-chain-id` > `X-Chain-Id` 头
4. 保序输出：成功 → `{id, result}`；失败 → `{id, error}`；notification → 无输出
5. 元素级失败不影响兄弟元素（-32000/-32001/-32002/-32005 等均可为元素级错误）

## 8. 与上游交互

- 转发体：剥离 `x-chain-id` 后的干净标准信封；单请求和 batch 子请求均作为单请求转发
- 上游响应校验：合法 JSON-RPC 且 `id` 与转发一致才视为成功；否则按「上游失败」走 failover（仅安全方法）或 -32002
- 幂等安全：安全方法至多 2 次尝试；`eth_sendTransaction`/`eth_sendRawTransaction` 永不自动重试
