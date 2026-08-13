# Phase 1 Data Model: Multichain RPC Gateway Core

**Branch**: `001-multichain-rpc-routing` | **Date**: 2026-08-13

> 网关无持久化存储（v1）：全部实体为内存态，由配置文件在启动时构建，生命周期 = 进程生命周期。本文件定义领域模型与状态机；外部契约见 [contracts/](./contracts/)。

## 1. 实体

### 1.1 Chain（链）

| 字段 | 类型 | 说明 |
|------|------|------|
| `chain_id` | string（十进制规范形，如 `"8453"`） | 全局唯一键；来自 `X-Chain-Id` 头或 `x-chain-id` override，数字输入归一化后比较 |
| `adapter` | Adapter 接口 | 链特定行为：EIP-1898 请求归一化、响应整形（response shaping）、原生币（native currency）处理、方法分类辅助；`ethereum`（chain 1）、`base`（chain 8453） |
| `upstreams` | []*Upstream | 有序列表（配置顺序）；路由排序规则（权威定义）：healthy 优先 → EWMA 延迟升序 → unknown 垫底（unknown 可用但最低优先级） |

**校验规则**（加载时失败即启动失败）：
- `chain_id` 必须为十进制非负整数；重复 chain_id 拒绝
- `upstreams` 非空（最少 1 个）
- `adapter` 名称必须在已注册 adapter 表中

**关系**：Chain 1—N Upstream；Chain N—1 配置。

### 1.2 Upstream（上游）

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 日志/指标 label 别名（缺省用 url 脱敏形式） |
| `url` | *url.URL | http/https；凭证（若内嵌）永不写入日志/指标 |
| `client` | *http.Client | 专用 Client + Transport 连接池（每上游独立超时与池参数） |
| `health` | HealthState | probe 驱动：`unknown` → `healthy` / `unhealthy` |
| `latency` | time.Duration | 最近一次 probe 或请求的延迟读数（EWMA） |
| `breaker` | 熔断器（gobreaker v2） | 三态：closed / open / half-open |

**校验规则**：url 必须可解析、scheme 为 http/https；配置中的 secret 以 `${VAR}` 形式注入，未设置即启动失败。

### 1.3 HealthProbeResult（健康探测结果）

| 字段 | 类型 | 说明 |
|------|------|------|
| `upstream` | *Upstream | 归属 |
| `ok` | bool | 探测是否成功 |
| `latency` | time.Duration | 探测往返耗时 |
| `checked_at` | time.Time | 采样时间 |

**生产**：prober 协程周期探测（默认 10s 间隔，方法 `eth_chainId`），结果写入 Upstream.health/latency 并计入熔断计数；**消费**：router 的路由决策。

### 1.4 RoutingRecord（路由记录，瞬时）

| 字段 | 类型 | 说明 |
|------|------|------|
| `chain_id` | string | 解析后的目标链 |
| `method` | string | JSON-RPC 方法名 |
| `upstream` | string | 最终选中的上游 name（失败且无 failover 时为候选） |
| `outcome` | string | `success` / 错误码（-320xx / -326xx / -32700） |
| `latency` | time.Duration | 网关处理总耗时 |
| `retries` | int | 实际重试次数（安全方法上限内） |

**生命周期**：一请求一记录；仅流入日志与指标，**永不持久化、不含 payload**。

## 2. 状态机

### 2.1 Upstream 健康状态

```text
unknown ──probe ok──▶ healthy ──probe fail──▶ unhealthy
  ▲                      │  ▲                    │
  └──────连续失败◀───────┘  └────probe ok（恢复）──┘
```

- 初始 `unknown`：不参与「健康偏好」排名（最低优先级但可用；排序规则见 §1.1）
- 连续 N 次探测失败（默认 3）→ `unhealthy`；恢复探测成功即回 `healthy`（有界防抖）

### 2.2 熔断器三态（gobreaker）

```text
closed ──失败数≥阈值──▶ open ──冷却超时──▶ half-open ──试探成功──▶ closed
                         │                      │
                         └─────试探失败──────────┘
```

- 阈值/冷却/半开放行数可配置（默认：5 次失败触发、30s 冷却、半开 1 次试探）
- 健康探测请求走 `breaker.Execute()`：探测失败计入熔断计数，恢复探测使 breaker 回到 closed 的路径一致

## 3. 方法分类（重试策略输入，FR-010）

| 类别 | 判定 | 行为 |
|------|------|------|
| **不可重试（状态变更）** | `eth_sendTransaction`、`eth_sendRawTransaction` | 至多 1 次尝试，失败即返回错误，绝不自动重试 |
| **可重试（安全）** | 白名单前缀：`eth_get*`、`eth_call`、`eth_estimateGas`、`eth_blockNumber`、`eth_chainId`、`eth_syncing`、`net_*`、`web3_*` 等 | 最多 2 次尝试，指数退避（10ms 基数 ×2）+ 全抖动，总截止 30s（可配置） |
| 未分类方法 | 默认按可重试处理（只读假设） | 同上；白名单/黑名单可配置扩展 |

> 规范形：方法名前缀匹配；`eth_sendTransaction`/`eth_sendRawTransaction` 显式黑名单优先于任何白名单。

## 4. 配置模型（YAML → struct，v1 无热重载）

```yaml
server:
  listen: ":8545"            # JSON-RPC 端点
  metrics_listen: ":9090"    # /metrics + /healthz
  max_batch_elements: 100    # FR-019 默认
  max_body_bytes: 1048576    # FR-019 默认（1 MB）
  timeouts:                  # 按方法类上游超时（秒）
    default: 10
    eth_getLogs: 30
prober:
  interval: 10s
  timeout: 5s
  fail_threshold: 3
retry:
  max_attempts: 2
  base_delay: 10ms
  max_elapsed: 30s
circuit:
  fail_threshold: 5
  cooldown: 30s
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - name: mainnet-a
        url: "${ETH_MAINNET_RPC_URL}"
  - chain_id: "8453"
    adapter: base
    upstreams:
      - name: base-a
        url: "${BASE_RPC_URL}"
```

完整字段契约见 [contracts/config-contract.md](./contracts/config-contract.md)。

## 5. 关系总览

```text
Config ─1:N─▶ Chain ─1:N─▶ Upstream ─1:1─▶ breaker + health + client
                                  ▲
                                  │ 写入
                            HealthProbeResult（prober 周期产生）
Request ─1:1─▶ RoutingRecord（瞬时）──N:1──▶ Upstream（选中者）
RoutingRecord ──流入──▶ slog 日志（脱敏）+ Prometheus 指标
```
