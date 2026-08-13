# Contract: Gateway Configuration (config.yaml)

**Branch**: `001-multichain-rpc-routing` | **Date**: 2026-08-13

> 配置文件契约。v1 启动时加载一次，无热重载；改动重启生效。secrets 以 `${ENV_VAR}` 注入，未设置即启动失败（fail-fast）。

## 1. Schema

```yaml
server:
  listen: ":8545"             # 必填；JSON-RPC 监听地址
  metrics_listen: ":9090"     # 必填；/metrics + /healthz 监听地址
  max_batch_elements: 100     # 可选；默认 100（FR-019）
  max_body_bytes: 1048576     # 可选；默认 1048576 = 1 MB（FR-019）
  timeouts:                   # 可选；按方法类上游超时
    default: 10               # 秒；默认 10
    eth_getLogs: 30           # 方法级覆盖（前缀匹配，最长匹配优先）
prober:                       # 可选；健康探测
  interval: 10s               # 默认 10s
  timeout: 5s                 # 默认 5s
  fail_threshold: 3           # 默认 3：连续失败判定 unhealthy
retry:                        # 可选；安全方法重试
  max_attempts: 2             # 默认 2（含首次）
  base_delay: 10ms            # 默认 10ms
  max_elapsed: 30s            # 默认 30s
circuit:                      # 可选；熔断器
  fail_threshold: 5           # 默认 5：连续失败打开
  cooldown: 30s               # 默认 30s：open → half-open 冷却
chains:                       # 必填；非空
  - chain_id: "1"             # 必填；十进制字符串，唯一
    adapter: ethereum         # 必填；已注册 adapter 名
    upstreams:                # 必填；至少 1 个
      - name: mainnet-a       # 可选；日志/指标别名（缺省用脱敏 URL）
        url: "${ETH_MAINNET_RPC_URL}"  # 必填；http/https
```

## 2. 校验规则（启动期，违规即退出并打印清晰错误）

| 规则 | 违规行为 |
|------|---------|
| YAML 可解析、字段已知 | 未知字段报错（`KnownFields(true)` 防 typo） |
| `${VAR}` 全部解析成功 | 未设置 → 启动失败 |
| chain_id 十进制非负整数、无重复 | 启动失败 |
| adapter 名已注册 | 启动失败 |
| upstreams 非空、url 可解析且 scheme 为 http/https | 启动失败 |
| 数值下限：max_batch_elements ≥ 1、max_body_bytes ≥ 1KB | 启动失败 |

## 3. Secret 处理

- 仅支持 `${VAR}` 花括号语法（不用 `os.ExpandEnv`，避免误伤 YAML 值中合法 `$`）
- 替换发生在 `yaml.Unmarshal` **之前**（原始字节正则替换）
- secrets（上游 URL 内嵌凭证等）永不写入日志/指标/仓库；日志中 URL 统一脱敏

## 4. 仓库内示例

- `config.example.yaml`：占位符 `${ETH_MAINNET_RPC_URL}` / `${BASE_RPC_URL}`，演示 Ethereum mainnet + Base 双链
- 本地 demo 用 mock 上游 URL（见 [quickstart.md](../quickstart.md)），无需真实 RPC 凭证
