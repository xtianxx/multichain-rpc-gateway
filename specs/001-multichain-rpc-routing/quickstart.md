# Quickstart: 运行与验证

**Branch**: `001-multichain-rpc-routing` | **Date**: 2026-08-13

> 端到端验证指南：证明多链 RPC 网关核心功能可用。不包含实现代码细节（见 tasks.md）。契约细节引用：[jsonrpc-api.md](./contracts/jsonrpc-api.md) · [config-contract.md](./contracts/config-contract.md) · [metrics-contract.md](./contracts/metrics-contract.md) · [data-model.md](./data-model.md)。

## 0. 前置条件

- Go ≥ 1.26（`go version` 确认；1.27 发布后可直接用）
- 压测工具 vegeta v12+（可选，仅负载/基准场景需要）：`go install github.com/tsenart/vegeta/v12@latest`
- 无真实 RPC 凭证要求：演示全部使用仓库内置 mock 上游

## 1. 一键本地 demo（README 同步要求）

```bash
make demo          # 等价于：启动 2 个 mock 上游（chain 1 / 8453）→ 启动网关 → 打印测试命令
```

预期：网关监听 `:8545`，指标 `:9090`；`curl -s localhost:9090/healthz` → `ok`。

## 2. 验收场景映射（对应 spec.md Acceptance Scenarios）

### US1 单链路由（curl 即可，无需 SDK）

```bash
# chain 8453（Base）：应返回 Base mock 上游的 chainId
curl -s localhost:8545 -H 'Content-Type: application/json' \
  -H 'X-Chain-Id: 8453' \
  -d '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}'
# → {"jsonrpc":"2.0","id":1,"result":"0x2105"}

# 未知链：-32000，不转发
curl -s localhost:8545 -H 'X-Chain-Id: 999' \
  -d '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":2}'
# → {"jsonrpc":"2.0","id":2,"error":{"code":-32000,...}}

# 标准客户端库透传（viem 示例，README 中给出完整可跑代码）
```

### US2 Batch + notification

```bash
# 混合链 batch（元素 1 → 头默认链，元素 2 → override 8453，元素 3 → notification）
curl -s localhost:8545 -H 'Content-Type: application/json' -H 'X-Chain-Id: 1' \
  -d '[{"jsonrpc":"2.0","method":"eth_chainId","id":1},
       {"jsonrpc":"2.0","method":"eth_chainId","id":2,"x-chain-id":"8453"},
       {"jsonrpc":"2.0","method":"eth_subscribe"}]'
# → 数组仅 2 元素，顺序 id=1（chain 1 结果）→ id=2（chain 8453 结果）；notification 无输出

# 空 batch：-32600
curl -s localhost:8545 -H 'Content-Type: application/json' -d '[]'
```

### US3 Failover（mock 上游支持故障注入）

```bash
# 使 chain 1 主上游返回 500，读请求应经备上游成功；eth_sendRawTransaction 恰好尝试 1 次
make demo-failover    # 脚本封装：单测之外的端到端可见演示
```

### US4 可观测性

```bash
curl -s localhost:9090/metrics | grep gateway_
# → gateway_requests_total{chain="8453",upstream="base-a",method="eth_chainId",outcome="success"} ≥ 1
# → gateway_upstream_up / gateway_upstream_circuit_state 存在
```

## 3. 自动化验证

```bash
make test           # go test ./... — 单元（路由/校验/熔断/退避）+ 集成（进程内 mock 双链）
make test-conformance  # JSON-RPC 2.0 一致性向量表（id echo、result/error 互斥、batch 保序、notification、-32700/-32600/-32601/-32602）
make lint           # go vet + gofmt 检查（CI 同款门禁）
```

验收标准：SC-001（标准客户端库 100% 正确）、SC-004（一致性向量全过）。

## 4. 基准与负载（SC-002 / SC-006）

```bash
# 增量延迟基准（进程内）：直连 mock 上游 vs 经网关完整管线
make bench          # go test -bench . -benchtime=10s -count=5 ./bench/
# 输出：BenchmarkDirect vs BenchmarkGateway 的 ns/op；网关增量 = 差值；p50 需 ≤ 直连 +20%

# 持续负载（外部压测，vegeta）
make load           # vegeta attack -rate=1000 -duration=60s（直连与经网关各一轮）
# 验收：两侧 p50 对比 <20% 预算；网关侧 ≥1000 req/s 无队列无界增长、无丢请求
```

合并门禁（宪法）：任何 merge 使基准 p50 开销恶化 >20% 必须书面理由（记录于 PR 描述）。

## 5. 预期失败模式速查（契约一致性）

| 输入 | 期望 | 引用 |
|------|------|------|
| 非法 JSON body | HTTP 400 + -32700 | jsonrpc-api §5 |
| 空 batch `[]` | -32600 | jsonrpc-api §4 |
| batch 101 元素 | 元素级 -32003 | jsonrpc-api §6 |
| body 2 MB | HTTP 400 + -32004 | jsonrpc-api §6 |
| `eth_subscribe` | -32601（v1 无 WebSocket） | jsonrpc-api §1 |
| 上游全挂 | -32001 或 -32005，有界返回，绝不挂起 | jsonrpc-api §6 |
