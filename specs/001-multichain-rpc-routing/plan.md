# Implementation Plan: Multichain RPC Gateway Core

**Branch**: `001-multichain-rpc-routing` | **Date**: 2026-08-13 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/001-multichain-rpc-routing/spec.md`

## Summary

多链 RPC 网关核心：单一 HTTP 端点接收 JSON-RPC 2.0 请求，按 `X-Chain-Id` 头（批量子元素可带 `x-chain-id` 覆盖字段）路由到对应链的上游 RPC。对客户端完全透明——ethers.js / viem / web3.js 直接指向网关即可使用所有配置链。核心能力：严格 JSON-RPC 2.0 合规（batch 保序、notification 语义、标准错误码）、上游健康探测 + 熔断 + 仅安全方法的限次指数退避重试、每链/每上游 Prometheus 指标、脱敏结构化日志、内置 passthrough 开销基准。

技术路线：**Go**（用户确认），基于标准库 net/http 的薄网关层；JSON-RPC 信封手写薄层以精确控制合规与自定义错误码；配置 = YAML 文件 + 环境变量替换 secrets（v1 重启生效）。具体库选型见 [research.md](./research.md) 与 ADR。

## Technical Context

**Language/Version**: Go 1.26.x（go1.26.5，2026-07；Go 1.27 发布后可直接升级；利用 log/slog、增强版 ServeMux）

**Primary Dependencies**: 标准库 net/http（路由+上游客户端）+ 4 个运行时第三方库依赖：sony/gobreaker/v2 v2.4.0（熔断）、cenkalti/backoff/v4 v4.3.0（重试退避）、prometheus/client_golang v1.24.1（指标）、gopkg.in/yaml.v3 v3.0.1（配置）；JSON-RPC 信封手写薄层；vegeta 与 mock-upstream 工具为 dev/test-only，非运行时依赖

**Storage**: N/A（无持久化；routing record 为瞬时非持久数据）

**Testing**: Go 原生 `testing` + `httptest` 进程内 mock JSON-RPC 上游；JSON-RPC 2.0 一致性向量；集成测试（双上游 failover、混合链 batch）

**Target Platform**: Linux 服务器（单节点 v1）

**Project Type**: web-service（JSON-RPC 网关）

**Performance Goals**: 单节点 ≥1,000 req/s 持续吞吐（SC-006）；网关增量延迟 p50 ≤ 直连上游基准 +20%（SC-002）

**Constraints**: 批量 ≤100 元素 / 请求体 ≤1 MB（默认，可配置）；有界截止时间、绝不挂起；payload 不落日志、敏感字段脱敏；secrets 不入库

**Scale/Scope**: 单节点，无水平扩展；演示链：Ethereum mainnet (1) + Base (8453)，纯配置驱动

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Gate | 设计承诺 | 状态 |
|------|---------|------|
| I. Chain-Agnostic Routing | 链 = 配置 + adapter；路由核心零改动加链；EIP-1898 归一化隔离在 adapter | ✅ 计划符合 |
| II. JSON-RPC 2.0 合规（不可协商） | 手写信封校验层：-32700/-32600/-32601/-32602 标准码 + -32000~-32099 网关码文档化；id echo、batch 保序、notification 语义 | ✅ 计划符合 |
| III. Resilience First | 主动健康探测、熔断器三态、仅安全方法重试（指数退避+抖动+硬上限）、有界截止时间 | ✅ 计划符合 |
| IV. Test-First（不可协商） | TDD：路由/校验/熔断/退避单测 + 进程内 mock 上游集成测试；错误契约变更必须同步更新测试 | ✅ 计划符合 |
| V. Observability & YAGNI | slog 结构化日志（脱敏、无 payload）+ Prometheus 每链/每上游指标 + 内置基准；无投机抽象 | ✅ 计划符合 |
| 安全与运维约束 | YAML + 环境变量替换；secrets 永不入库；畸形请求先拒后转 | ✅ 计划符合 |
| ADR 要求 | 技术栈决策写入 `docs/adr/`（decision/context/rejected alternatives） | ✅ 计划符合（Phase 0 产出） |
| 基准合并门禁 | p50 开销回归 >20% 需书面理由；基准随仓库交付 | ✅ 计划符合 |

**Phase 1 设计后复检（2026-08-13）**: ✅ 全部通过。设计产物（data-model.md、contracts/、quickstart.md、research.md）逐项兑现上表承诺：路由核心零链耦合（chain/ adapter 隔离）、错误码 -32000~-32005 文档化、熔断+安全方法重试策略参数化、TDD 与一致性向量验证路径、脱敏日志与自定义桶 Histogram、仅 4 个第三方库（YAGNI）。无违规，Complexity Tracking 无需填写。

## Project Structure

### Documentation (this feature)

```text
specs/[###-feature]/
├── plan.md              # This file (/speckit.plan command output)
├── research.md          # Phase 0 output (/speckit.plan command)
├── data-model.md        # Phase 1 output (/speckit.plan command)
├── quickstart.md        # Phase 1 output (/speckit.plan command)
├── contracts/           # Phase 1 output (/speckit.plan command)
└── tasks.md             # Phase 2 output (/speckit.tasks command - NOT created by /speckit.plan)
```

### Source Code (repository root)

```text
cmd/gateway/              # main：flag 解析、config 加载、slog/Prometheus 装配、优雅停机
internal/
├── config/               # YAML 解析 + ${VAR} 替换 + 启动期校验（config-contract 实现）
├── jsonrpc/              # 手写信封薄层：解析/校验/序列化、batch 保序、错误码表
├── chain/                # Chain/Upstream 注册表 + Adapter 接口 + ethereum/base 实现
├── router/               # 链解析、路由决策（健康+延迟+熔断）、RoutingRecord
├── upstream/             # 转发执行：http.Client 池、重试/退避、熔断联动
├── prober/               # 主动健康探测循环
├── metrics/              # Prometheus 注册与收集（metrics-contract 实现）
└── logging/              # slog 装配 + 白名单/ReplaceAttr 双重脱敏
config.example.yaml       # 占位符示例（Ethereum mainnet + Base）
docs/adr/                 # ADR（0001 Go 语言/运行时、0002 JSON-RPC 手写层与库级选型）
tests/
├── integration/          # 进程内 mock 上游端到端测试（双链、failover、混合 batch）
└── conformance/          # JSON-RPC 2.0 一致性向量表
bench/                    # 增量延迟基准：BenchmarkDirect vs BenchmarkGateway
```

单元测试与包同目录（`internal/jsonrpc/jsonrpc_test.go` 等），符合 Go 惯例。

**Structure Decision**: 单 Go 项目（无前端、无多服务）。`internal/` 按职责分包：信封层（jsonrpc）→ 路由层（router）→ 执行层（upstream）→ 旁路（prober/metrics/logging）；链特定行为全部收敛在 `chain/` adapter 包（宪法 I：加链零核心改动）。

## Complexity Tracking

> **Fill ONLY if Constitution Check has violations that must be justified**

无违规。设计仅引入 4 个运行时第三方库依赖（gobreaker、backoff、client_golang、yaml.v3；vegeta、mock-upstream 等为 dev/test-only，非运行时依赖），无投机抽象、无额外项目/服务，符合宪法 YAGNI 与单节点 v1 范围。
