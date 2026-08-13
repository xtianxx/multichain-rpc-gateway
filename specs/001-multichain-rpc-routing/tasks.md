# Tasks: Multichain RPC Gateway Core

**Input**: Design documents from `/specs/001-multichain-rpc-routing/`

**Prerequisites**: plan.md ✅ · spec.md ✅ · research.md ✅ · data-model.md ✅ · contracts/ ✅ · quickstart.md ✅ · constitution.md ✅

**Tests**: 包含测试任务 —— 宪法 IV「Test-First」为不可协商项（TDD 强制：路由/校验/熔断/退避单测 + 进程内 mock 上游集成测试 + JSON-RPC 2.0 一致性向量）。

**Organization**: 按用户故事分组（US1→US4），每个故事独立实现、独立测试、独立交付。

**Tech Stack**: Go 1.26 · 标准库 net/http + slog · 运行时依赖仅 4 个（vegeta、mock-upstream 等工具为 dev/test-only，非运行时依赖）：sony/gobreaker/v2 v2.4.0、cenkalti/backoff/v4 v4.3.0、prometheus/client_golang v1.24.1、gopkg.in/yaml.v3 v3.0.1

**Module**: `github.com/xtianxx/multichain-rpc-gateway`

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无依赖）
- **[Story]**: 所属用户故事（US1/US2/US3/US4）
- 描述含精确文件路径

---

## Phase 1: Setup（共享基础设施）

**Purpose**: 项目初始化与基础结构

- [X] T001 初始化 Go module `github.com/xtianxx/multichain-rpc-gateway` 并引入 4 个依赖（gobreaker/v2 v2.4.0、backoff/v4 v4.3.0、prometheus/client_golang v1.24.1、yaml.v3 v3.0.1）写入 go.mod
- [X] T002 [P] 编写 ADR 到 docs/adr/：0001-language-runtime-go.md（语言/运行时决策）；0002-jsonrpc-envelope-handwritten.md（手写 JSON-RPC 薄层 vs go-ethereum/rpc 与 JSON-RPC 库——方法注册模型与透传方向相反、geth LGPL/GPL 许可负担；含库级选型 gobreaker/backoff/client_golang/yaml、x-chain-id 命名、-32000~-32005 错误码分配），均含 decision/context/rejected alternatives
- [X] T003 [P] 创建 config.example.yaml（Ethereum mainnet chain_id "1" + Base chain_id "8453"，`${ETH_MAINNET_RPC_URL}` / `${BASE_RPC_URL}` 占位符，含 server/prober/retry/circuit 全部字段）
- [X] T004 [P] 创建 Makefile（targets：demo、demo-failover、test、test-conformance、lint、bench、load）
- [X] T005 [P] 创建 .gitignore（忽略二进制、.env、config.yaml 等含 secrets 文件）

**Checkpoint**: `go build ./...` 可通过（空 main 亦可），依赖已锁定

---

## Phase 2: Foundational（阻塞性前置）

**Purpose**: 所有用户故事依赖的核心包。TDD：先测试后实现（测试先行失败）。

**⚠️ CRITICAL**: 本阶段完成前不得开始任何用户故事

### 测试（先写，预期 FAIL）

- [X] T006 [P] 编写 config 包测试：YAML 解析、`${VAR}` 替换、未知字段拒绝、chain_id 校验、url scheme 校验、数值下限到 internal/config/config_test.go
- [X] T007 [P] 编写 jsonrpc 包测试：信封解析/校验（-32700/-32600）、id 逐字节回显、notification 检测、标准+网关错误码表到 internal/jsonrpc/envelope_test.go
- [X] T008 [P] 编写 chain 包测试：adapter 注册表、EIP-1898 块参数归一化、chain_id 规范化比较到 internal/chain/chain_test.go
- [X] T009 [P] 编写 logging 包测试：脱敏规则（私钥/地址/token/URL 凭证）到 internal/logging/logging_test.go
- [X] T010 [P] 编写 metrics 包测试：collector 定义与 label 维度（chain/upstream/method/outcome）到 internal/metrics/metrics_test.go

### 实现（使测试通过）

- [X] T011 [P] 实现 config 包：YAML 解析（KnownFields 严格模式）+ 手写 `${VAR}` 正则替换（yaml.Unmarshal 前、未设置 fail-fast）+ 启动期校验到 internal/config/config.go
- [X] T012 [P] 实现 jsonrpc 包：信封类型（json.RawMessage 保 id 逐字节）、解析/校验、错误码表（标准 -32700/-32600/-32601/-32602/-32603 + 网关 -32000~-32005）到 internal/jsonrpc/envelope.go 与 internal/jsonrpc/errors.go
- [X] T013 [P] 实现 chain 包：Chain/Upstream 结构、Adapter 接口 + 注册表、ethereum 与 base adapter（EIP-1898 请求归一化、响应整形、原生币处理）到 internal/chain/chain.go、internal/chain/ethereum.go、internal/chain/base.go
- [X] T014 [P] 实现 logging 包：slog JSON handler 装配 + 白名单/ReplaceAttr 双重脱敏到 internal/logging/logging.go
- [X] T015 [P] 实现 metrics 包：gateway_requests_total（Counter）、gateway_request_duration_seconds（Histogram，低段加密桶 [0.0001...10]）、gateway_requests_inflight（Gauge，chain/upstream）、gateway_upstream_up（Gauge：0=unhealthy / 1=healthy / 2=unknown）/ probe_latency / circuit_state（Gauge）注册到 internal/metrics/metrics.go；gateway_request_duration_seconds 的 method label 若基数失控收敛为 chain+upstream 两维（metrics-contract §2）
- [X] T016 全量验证：`go build ./...` + `go test ./...` 全绿，`go vet ./...` 无告警

**Checkpoint**: 基础层就绪 —— 用户故事可并行开始

---

## Phase 3: User Story 1 - Route a JSON-RPC request to the correct chain upstream (Priority: P1) 🎯 MVP

**Goal**: 客户端（ethers.js/viem/web3.js）指向网关单端点，经 `X-Chain-Id` 头路由到对应链上游，返回带正确 id 的结果。未知链返回 -32000，不转发。

**Independent Test**: 启动网关 + 两个 mock 上游（chain 1 / chain 8453），对各自链发请求，结果来自对应上游（curl 与标准客户端库均可验证）。

### Tests for User Story 1（TDD，先写，预期 FAIL）

- [X] T017 [P] [US1] 编写 router 单测：X-Chain-Id 解析、未知链 -32000、单上游选择、RoutingRecord 构建到 internal/router/router_test.go
- [X] T018 [P] [US1] 编写集成测试：双 mock 上游进程内端到端路由（httptest）到 tests/integration/routing_test.go

### Implementation for User Story 1

- [X] T019 [P] [US1] 实现 upstream 转发客户端：每上游独立 http.Client + Transport 连接池、按方法类 context.WithTimeout、响应校验（合法 JSON-RPC 且 id 匹配，否则 -32002）到 internal/upstream/client.go
- [X] T020 [P] [US1] 实现 router：链解析（头 → chain_id 规范形）、上游选择（v1 基础：取第一个可用）、RoutingRecord（chain/method/upstream/outcome/latency）到 internal/router/router.go
- [X] T021 [US1] 实现 cmd/gateway/main.go：flag 解析、config 加载、slog/Prometheus 装配、HTTP handler（POST /：单请求 → router → upstream → 响应回显；`eth_subscribe` 网关侧拒绝返回 -32601，v1 无 WebSocket）、优雅停机
- [X] T022 [US1] 补齐 US1 错误路径：未知链 -32000（含 data 上下文 `{"chain_id":...}`）、-32002 无效上游响应；保证畸形请求先拒后转（FR-016）

**Checkpoint**: User Story 1 独立可测 —— 双 mock 上游路由正确、-32000 不转发

---

## Phase 4: User Story 2 - JSON-RPC 2.0 batch requests and notifications (Priority: P1)

**Goal**: batch（可混合链、可含 notification）单次调用：逐元素路由、响应数组严格保序、notification 无响应元素、元素级失败不影响兄弟元素、空 batch → -32600、超限 → -32003/-32004。

**Independent Test**: 发送混合双链 batch + notification，验证顺序、元素数、各元素结果链归属；空 batch 与超限 batch 返回文档化错误。

### Tests for User Story 2（TDD，先写，预期 FAIL）

- [ ] T023 [P] [US2] 编写一致性向量表：id echo、result/error 互斥、标准错误码、batch 保序、notification 处理到 tests/conformance/vectors_test.go
- [ ] T024 [P] [US2] 编写集成测试：混合链 batch、notification、空 batch -32600、单元素失败其余成功到 tests/integration/batch_test.go

### Implementation for User Story 2

- [ ] T025 [US2] 实现 batch 解析与装配：`[]json.RawMessage` 逐元素处理、保序响应数组、notification 不占位、全 notification 空 body 到 internal/jsonrpc/batch.go
- [ ] T026 [US2] 实现每元素链覆盖：`x-chain-id` 字段解析（string/number → 十进制规范形，优先于头、缺失继承头）、校验后剥离不转发到 internal/router/batch.go（或 router 现有文件扩展）
- [ ] T027 [US2] 实现 handler batch 编排：POST / 首 token 判定单请求 vs batch、逐元素校验先拒后转、`eth_subscribe` 元素 → -32601（v1 无 WebSocket）、元素级错误（-32000/-32002 等）不阻断兄弟元素到 cmd/gateway/handler.go
- [ ] T028 [US2] 实现批量限制：max_batch_elements（默认 100，超限 -32003）、max_body_bytes（默认 1 MB，超限 HTTP 400 + -32004），参数取自 config 到 cmd/gateway/handler.go 或 internal/config 调用点

**Checkpoint**: US1 + US2 均独立可测 —— SC-004 一致性向量全过

---

## Phase 5: User Story 3 - Upstream selection, failover, and health (Priority: P2)

**Goal**: 每链多上游：主动探测健康+延迟、偏好健康低延迟；安全方法有界指数退避重试 + failover；`eth_sendTransaction`/`eth_sendRawTransaction` 永不自动重试；熔断器隔离反复失败上游；全挂时 -32001/-32005 有界返回，绝不挂起。

**Independent Test**: 单链双 mock 上游，强制主上游失败（timeout/500/非法响应），读请求经备上游成功；`eth_sendRawTransaction` 恰好尝试 1 次。

### Tests for User Story 3（TDD，先写，预期 FAIL）

- [ ] T029 [P] [US3] 编写重试分类器测试：白名单前缀（eth_get*/eth_call/eth_estimateGas/net_*/web3_*）可重试、`eth_sendTransaction`/`eth_sendRawTransaction` 黑名单永不重试到 internal/upstream/retry_test.go
- [ ] T030 [P] [US3] 编写熔断+failover 测试：gobreaker 三态、失败切换下一上游、全挂 -32001、总截止 -32005 到 internal/router/failover_test.go
- [ ] T031 [P] [US3] 编写集成测试：双上游 failover 端到端、写方法 exactly-once、上游非法 JSON → -32002 后切换到 tests/integration/failover_test.go

### Implementation for User Story 3

- [ ] T032 [US3] 实现 prober：周期探测（默认 10s，`eth_chainId`）、健康状态机（unknown→healthy/unhealthy，连续 N 失败判定）、EWMA 延迟读数，探测走 breaker.Execute() 到 internal/prober/prober.go
- [ ] T033 [P] [US3] 实现熔断接线：每上游 gobreaker v2（fail_threshold 5 / cooldown 30s / half-open 1 试探），探测失败计入熔断计数到 internal/upstream/breaker.go
- [ ] T034 [P] [US3] 实现重试与退避：cenkalti/backoff v4（基数 10ms ×2 指数 + 全抖动、max_attempts 2、max_elapsed 30s）、黑名单方法 backoff.Permanent 短路到 internal/upstream/retry.go
- [ ] T035 [US3] 实现 failover 路由：router 按健康+延迟偏好选上游，失败（超时/网络/非法响应）对安全方法切换下一上游，耗尽返回 -32001/-32005；每请求有界截止时间到 internal/router/router.go（扩展）与 internal/upstream/client.go
- [ ] T036 [US3] 实现按方法类上游超时：default 10s、eth_getLogs 30s（前缀最长匹配）到 internal/upstream/client.go 或 internal/config 调用点

**Checkpoint**: US1+US2+US3 独立可测 —— SC-003（failover 100%、写方法不重复）

---

## Phase 6: User Story 4 - Observe and operate the gateway (Priority: P3)

**Goal**: 每请求日志含 chain/method/upstream/latency/outcome 且无 payload、敏感字段脱敏；每链/每上游指标（请求率、错误率、延迟分位）；`/metrics` + `/healthz` 端点；内置 passthrough 开销基准。

**Independent Test**: 打流量到网关（mock 上游），验证日志无 payload、敏感字段脱敏；`curl /metrics` 可见 gateway_* 指标；`make bench` 输出直连 vs 网关增量延迟。

### Tests for User Story 4（TDD，先写，预期 FAIL）

- [ ] T037 [P] [US4] 编写指标记录测试：请求计数/histogram 按 chain/upstream/method/outcome 维度正确累计到 internal/metrics/recording_test.go
- [ ] T038 [P] [US4] 编写日志脱敏测试：payload 不落日志、`eth_sendRawTransaction` 私钥/地址/token/URL 凭证脱敏到 internal/logging/logging_test.go（扩展）

### Implementation for User Story 4

- [ ] T039 [US4] 在请求路径埋点：router/upstream 记录 gateway_requests_total（outcome 含错误码）与 gateway_request_duration_seconds（含上游 RTT）；入站递增、完成递减 gateway_requests_inflight
- [ ] T040 [US4] 埋点健康指标：prober/breaker 更新 gateway_upstream_up、gateway_upstream_probe_latency_seconds、gateway_upstream_circuit_state gauge
- [ ] T041 [US4] 接线 HTTP 端点：GET /metrics（promhttp.Handler + go/process collectors）、GET /healthz（200 纯文本）到 cmd/gateway/main.go（metrics_listen 独立监听）
- [ ] T042 [US4] 每请求结构化日志：chain/method/upstream/outcome/latency/retries（RoutingRecord 流入 slog，脱敏后输出）
- [ ] T043 [P] [US4] 实现基准套件：进程内 httptest 上游，BenchmarkDirect vs BenchmarkGateway 对比到 bench/passthrough_test.go（`make bench` 输出 ns/op 与 p50 增量）

**Checkpoint**: 全部用户故事独立可用 —— 日志/指标可观测、基准可跑

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: 交付物完善与全量验收

- [ ] T044 [P] 编写 README.md：架构总览、一键 demo 说明、viem 完整可跑示例、配置说明、指标速查
- [ ] T045 [P] 实现 mock 上游二进制（支持 eth_chainId 等常用方法与故障注入开关）到 cmd/mockupstream/main.go
- [ ] T046 [P] 编写 demo 脚本与 Makefile 接线：make demo（2 mock 上游 + 网关 + 打印测试命令）、make demo-failover（故障注入端到端演示）
- [ ] T047 [P] 提供 Grafana 示例面板：每链 QPS/错误率/p50/p95、上游健康热力表到 docs/grafana/example-dashboard.json
- [ ] T048 [P] 配置 CI：.github/workflows/ci.yml（gofmt 检查 + go vet + go test ./... + test-conformance + make bench：p50 对比直连基线、回归 >20% 阻断合并（FR-017 门禁），全部门禁失败阻断合并）
- [ ] T049 全量验收 quickstart.md：make test、make test-conformance、make lint、make bench（p50 增量 ≤ 直连 +20%）、make load（vegeta 1000 req/s 60s：成功率 100%、零丢弃、gateway_requests_inflight 有界，SC-006）逐项通过并记录结果
- [ ] T050 最终代码清理：gofmt -l 为空、go vet 无告警、无 TODO 遗留、secrets 检查（git grep -E '\$\{|PRIVATE_KEY|API_KEY' 确认无真实 secret）

**Checkpoint**: 全部交付 —— SC-001~SC-006 均可复现验证

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: 无依赖，可立即开始
- **Foundational (Phase 2)**: 依赖 Setup —— **阻塞全部用户故事**
- **User Stories (Phase 3-6)**: 全部依赖 Foundational 完成
  - US1 (P1) 与 US2 (P1)：Foundational 后可开始；US2 建议在 US1 之后（共享 handler/router 调用点），也可并行（batch.go 独立文件）
  - US3 (P2)：依赖 US1（扩展 router/upstream），但 prober/breaker/retry 包文件独立可先行
  - US4 (P3)：依赖 US1（请求路径存在后埋点），metrics/logging 埋点测试可先行
- **Polish (Phase 7)**: 依赖全部故事完成

### User Story Dependencies

- **US1 (P1)**: Foundational 后即可开始，无故事间依赖
- **US2 (P1)**: 依赖 US1 的 handler 骨架（或与 US1 同批交付）
- **US3 (P2)**: 依赖 US1 的 router/upstream（在其上扩展 failover/熔断/重试）
- **US4 (P3)**: 依赖 US1 请求路径（埋点对象）；日志/指标包本体已在 Foundational 就绪

### Within Each User Story

- 测试任务先行（TDD：写测试 → 见其失败 → 实现 → 重构）
- 独立文件任务可并行（[P] 标记）
- 实现任务按依赖顺序：包 → 接线 → 错误路径

### Parallel Opportunities

- Phase 1：T002-T005 全并行
- Phase 2：T006-T010 测试全并行 → T011-T015 实现全并行
- US1：T017/T018 并行 → T019/T020 并行 → T021/T022
- US3：T029-T031 并行 → T033/T034 与 T032 并行 → T035/T036
- US4：T037/T038 并行；T043 与 T039-T042 并行
- Phase 7：T044-T048 全并行
- 团队场景：Foundational 完成后，A=US1+US2、B=US3（先写 prober/breaker/retry）、C=US4 埋点测试，可并行推进

---

## Parallel Example: Foundational Phase

```bash
# 并行启动全部测试（先行失败）：
Task: "config 包测试 internal/config/config_test.go"
Task: "jsonrpc 包测试 internal/jsonrpc/envelope_test.go"
Task: "chain 包测试 internal/chain/chain_test.go"
Task: "logging 包测试 internal/logging/logging_test.go"
Task: "metrics 包测试 internal/metrics/metrics_test.go"

# 测试就绪后并行实现：
Task: "internal/config/config.go"
Task: "internal/jsonrpc/envelope.go + errors.go"
Task: "internal/chain/*.go（registry + ethereum + base adapters）"
Task: "internal/logging/logging.go"
Task: "internal/metrics/metrics.go"
```

---

## Implementation Strategy

### MVP First（US1 + US2，宪法 P1 双故事）

1. Phase 1 Setup → Phase 2 Foundational（TDD）
2. Phase 3 US1：单请求路由端到端（curl + viem 验证）
3. Phase 4 US2：batch + notification + 一致性向量
4. **STOP AND VALIDATE**：SC-001、SC-004 达标即 MVP 可演示
5. 交付 `make demo` 演示链路

### Incremental Delivery

1. Setup + Foundational → 基础就绪（go test 全绿）
2. +US1 → 单链路由可用 → 验证
3. +US2 → batch/notification 合规 → 验证（MVP！）
4. +US3 → failover/熔断/重试 → 验证（SC-003）
5. +US4 → 日志/指标/基准 → 验证（SC-002/SC-006）
6. +Polish → README/CI/demo/全量验收

### Parallel Team Strategy

1. 团队共同完成 Setup + Foundational
2. Foundational 完成后分派：
   - 开发者 A：US1 → US2（核心路由路径）
   - 开发者 B：US3 独立包（prober/breaker/retry 先行）+ 集成
   - 开发者 C：US4 测试与埋点 + 基准
3. 各故事独立验证后合并，Phase 7 统一收口

---

## Notes

- [P] 任务 = 不同文件、无依赖，可并行
- [Story] 标签映射任务到用户故事，保证可追踪
- 每个用户故事独立完成、独立测试、独立交付
- TDD（宪法 IV）：测试先行失败再实现；错误契约变更必须同变更更新测试
- 宪法 II：错误码 -32000~-32005 为稳定契约；标准码严格按规范
- 宪法 I：加链 = 配置 + adapter，路由核心零改动（SC-005）
- 宪法 V：仅 4 个第三方库，无投机抽象；payload 永不落日志
- 每个任务或逻辑组完成后 commit；每个 Checkpoint 停下独立验证
