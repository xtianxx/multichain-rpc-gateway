# Feature Specification: Multichain RPC Gateway Core

**Feature Branch**: `001-multichain-rpc-routing`

**Created**: 2026-08-13

**Status**: Draft

**Input**: User description: "多链 RPC 网关核心:统一入口接收 JSON-RPC 请求并按 chainId 路由到对应链上游 RPC"

## Clarifications

### Session 2026-08-13

- Q: 客户端发送请求时，如何指定目标链？ → A: 单一 URL + HTTP 头 `X-Chain-Id` 作为默认链；批量请求中元素可携带 per-element 链覆盖字段（字段名在规划阶段确定），覆盖优先于头；混合链批量由此实现。
- Q: 网关需要设计为承受多大的请求吞吐量（每秒请求数）？ → A: 单节点 ~1,000 req/s 持续吞吐，p50 开销在该负载下测量；水平扩展不在 v1 范围。
- Q: 链/上游配置如何交付，运行时可以变更而不重启吗？ → A: 配置文件（如 YAML，具体格式规划时定）+ 环境变量替换秘密字段；v1 重启生效，无热重载。
- Q: 批量元素数与请求体大小的默认上限取多少？ → A: 批量 ≤100 元素 / 请求体 ≤1 MB，均可配置；超限返回 -32000 区间文档化错误。
- Q: v1 是否需要自建 UI？ → A: 无自建 UI；操作面 = CLI + 结构化日志 + Prometheus 格式指标端点（可附带 Grafana 示例配置）；管理 dashboard 推迟为后续 feature。

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Route a JSON-RPC request to the correct chain upstream (Priority: P1)

A dApp developer points a standard JSON-RPC client library (ethers.js / viem / web3.js) at the gateway. They send a request targeting a specific chain (for example `eth_getBalance` on Base). The gateway resolves the target chain, forwards the request to a configured upstream for that chain, and returns the upstream's result with the correct id echoed back. To the client, the gateway behaves exactly like a direct chain RPC endpoint.

**Why this priority**: Uniform multi-chain access is the product. Every other capability (failover, metrics, auth) is meaningless if basic routing does not work correctly.

**Independent Test**: Stand up the gateway with two mock upstream servers, each answering for a different chain. Sending requests for each chain returns results from the corresponding upstream, verifiable end-to-end with a standard client library.

**Acceptance Scenarios**:

1. **Given** the gateway is configured with upstreams for chain 1 (Ethereum mainnet) and chain 8453 (Base), **When** a client sends `eth_chainId` addressed to Base, **Then** the response echoes the request id and returns the chain id from the Base upstream.
2. **Given** a request addressed to a chain that has no configured upstream, **When** the client sends it, **Then** the gateway returns a JSON-RPC 2.0 error with a documented code in the reserved range (-32000 to -32099) and forwards nothing.
3. **Given** a request with a valid, configured target chain, **When** the client sends it, **Then** the gateway forwards it to exactly one upstream for that chain and returns that upstream's result with the exact request id.

---

### User Story 2 - JSON-RPC 2.0 batch requests and notifications (Priority: P1)

A client sends a JSON-RPC batch — possibly mixing requests for different chains and notifications — in a single call. The gateway routes each element to its chain and returns an array of responses in the same order as the request elements. Notifications produce no response element. If one element fails, that element carries an error object while the rest succeed.

**Why this priority**: The constitution declares JSON-RPC 2.0 spec compliance non-negotiable; every downstream client library depends on exact batch and notification behavior.

**Independent Test**: Send a batch containing requests for two different chains plus a notification; verify the response array preserves order, contains one element per non-notification request, and each element carries the correct chain's result.

**Acceptance Scenarios**:

1. **Given** a batch `[A (chain 1), B (chain 8453)]`, **When** sent, **Then** the gateway returns an ordered array with A's result first and B's result second.
2. **Given** a batch containing a notification among requests, **When** sent, **Then** the response array contains no element for the notification and preserves order for the rest.
3. **Given** an empty batch (`[]`), **When** sent, **Then** the gateway returns the standard invalid request error (-32600).
4. **Given** a batch where one element targets an unavailable upstream, **When** sent, **Then** that element returns an error object while the other elements still return their results.

---

### User Story 3 - Upstream selection, failover, and health (Priority: P2)

An operator configures more than one upstream for a chain. The gateway keeps track of each upstream's health and latency through active probes and prefers healthy, low-latency upstreams. When the chosen upstream fails, the gateway either fails over to another upstream for the same chain or returns a structured server error — it never hangs. Read-only and state-simulation methods may be retried with bounded exponential backoff; state-changing methods are never retried automatically. Upstreams that repeatedly fail are cut off by a circuit breaker until they recover.

**Why this priority**: The constitution declares resilience the product's core value. A gateway that hangs, drops, or duplicates requests is worse than calling an upstream directly.

**Independent Test**: Configure one chain with two mock upstreams. Force the primary to fail (timeout / invalid response) and verify read requests succeed via the secondary, while a state-changing request is attempted exactly once.

**Acceptance Scenarios**:

1. **Given** a chain with two upstreams where the primary fails, **When** a read request is sent, **Then** the gateway retries on the secondary with bounded backoff and returns the correct result.
2. **Given** an `eth_sendRawTransaction` fails against its upstream, **When** sent, **Then** the gateway returns an error and never retries — the upstream receives exactly one attempt.
3. **Given** an upstream that repeatedly fails, **When** subsequent requests arrive, **Then** a circuit breaker stops routing to that upstream until it passes recovery probes.
4. **Given** all upstreams for a chain are down, **When** a request arrives, **Then** the gateway returns a structured server error within a bounded deadline and never hangs.

---

### User Story 4 - Observe and operate the gateway (Priority: P3)

An operator can inspect what the gateway is doing without seeing user payloads: each request is recorded with its chain, method, selected upstream, latency, and outcome; aggregate metrics show request rate, error rate, and latency percentiles per chain and per upstream. A benchmark quantifies the overhead the gateway adds relative to calling an upstream directly.

**Why this priority**: Observability and a measurable overhead budget are constitution requirements and make the project debuggable and credible as engineering evidence.

**Independent Test**: Run traffic through the gateway against mock upstreams and verify log entries contain chain/method/upstream/latency/outcome but no payload content, and that per-chain/per-upstream metrics are exposed.

**Acceptance Scenarios**:

1. **Given** a request is processed, **When** the operator inspects logs, **Then** they see chain, method, selected upstream, latency, and outcome — and no request/response payload content; sensitive fields (private keys in `eth_sendRawTransaction`, addresses, tokens) are redacted.
2. **Given** mixed traffic across chains and upstreams, **When** the operator inspects metrics, **Then** request rate, error rate, and latency percentiles are available per chain and per upstream.
3. **Given** the benchmark suite is run, **When** results are collected, **Then** the gateway's added latency (excluding upstream latency) is measured at p50.

---

### Edge Cases

- Request addressed to an unknown chain id → documented gateway error, nothing forwarded.
- A batch element without a chain override is routed to the header's default chain; an override naming an unknown or unconfigured chain produces a per-element documented error.
- Request addressed to a known chain whose upstream configuration is invalid or unreachable → structured server error within the deadline.
- Upstream times out mid-request → failover to another upstream (safe methods only) or structured error; never an open hang.
- Batch mixing unknown-chain, invalid, and valid elements → per-element errors/results with order preserved.
- A notification with invalid parameters → still validated before forwarding (constitution: malformed requests are rejected before forwarding), but no response element is produced.
- Concurrent requests with identical ids → each response echoes its own request's id; no cross-request contamination.
- Upstream returns non-JSON or JSON that is not a valid JSON-RPC response → treated as an upstream failure, handled per failover rules.
- A read request to an upstream that fails during a state-simulation method (`eth_call`, `eth_estimateGas`) → retryable, same as other safe methods.
- EIP-1898 block parameter forms → normalized by the per-chain adapter before forwarding (constitution: chain-specific behavior isolated in adapters).
- Batch exceeding the element cap (default 100) or a body exceeding the size cap (default 1 MB) → documented gateway error (-32000 to -32099), nothing forwarded.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The gateway MUST expose a single unified HTTP endpoint for every supported chain; the client addresses the target chain via the `X-Chain-Id` HTTP header. In batch requests, an element MAY carry a per-element chain override field (exact field name fixed during planning) that takes precedence over the header; without an override, the element inherits the header's chain.
- **FR-002**: The gateway MUST resolve the target chain from the request addressing and reject requests for unknown or unconfigured chains with a documented error code in -32000 to -32099.
- **FR-003**: The gateway MUST forward each valid request to a configured upstream for the resolved chain and return the upstream's result with the exact request id.
- **FR-004**: The gateway MUST validate the JSON-RPC 2.0 envelope before forwarding: parse errors (-32700), invalid request (-32600), method not found (-32601), and invalid params (-32602) per spec.
- **FR-005**: The gateway MUST support batch requests with ordered responses and notification semantics per the JSON-RPC 2.0 spec.
- **FR-006**: Gateway-specific failures (upstream unreachable, chain not configured, upstream invalid response) MUST use documented error codes in the reserved range -32000 to -32099.
- **FR-007**: The gateway MUST support multiple upstreams per chain and prefer healthy, low-latency upstreams when routing.
- **FR-008**: The gateway MUST run active, probe-based health checks whose results feed routing decisions.
- **FR-009**: On upstream failure, the gateway MUST fail over to another upstream for the same chain or return a structured server error within a bounded deadline; it MUST NOT hang or swallow requests.
- **FR-010**: Automatic retries MUST apply only to safe/idempotent methods (read-only methods and state-simulation methods such as `eth_call` and `eth_estimateGas`); state-changing methods (`eth_sendTransaction`, `eth_sendRawTransaction`) MUST NEVER be retried automatically.
- **FR-011**: Retries MUST use exponential backoff with jitter and hard caps on both the number of attempts and the total deadline.
- **FR-012**: Circuit breakers MUST stop routing to upstreams that repeatedly fail, and allow them back only after recovery probes pass.
- **FR-013**: Structured logging MUST record each request's chain, method, selected upstream, latency, and outcome; payloads MUST NOT be logged in full; sensitive fields (private keys, addresses, tokens) MUST be redacted.
- **FR-014**: Metrics MUST cover request rate, error rate, and latency percentiles per chain and per upstream, exposed via a Prometheus-format pull endpoint.
- **FR-015**: Supported chains MUST be declarable purely via configuration (adding a chain requires configuration plus a new adapter, never a change to the routing core), and chain-specific behavior (e.g., EIP-1898 block parameter normalization) MUST be isolated in per-chain adapters.
- **FR-016**: Malformed or invalid requests MUST be rejected before being forwarded to any upstream.
- **FR-017**: The gateway MUST ship a benchmark that measures passthrough overhead (gateway-added latency excluding upstream latency), and a merge that degrades p50 overhead by more than 20% requires written justification.
- **FR-018**: The gateway MUST accept JSON-RPC 2.0 requests over HTTP. WebSocket transport (subscriptions such as `eth_subscribe`) is out of scope for v1.
- **FR-019**: The gateway MUST enforce configurable caps on batch element count (default 100) and request body size (default 1 MB); requests exceeding either cap are rejected with a documented error code in -32000 to -32099 before any forwarding.

### Key Entities *(include if feature involves data)*

- **Chain**: A supported network; identified by chain id; carries its configured upstream list and its adapter behavior (block parameter normalization, native currency handling).
- **Upstream**: A concrete JSON-RPC endpoint for one chain; carries its health state (healthy/unhealthy, measured latency, circuit state) and its connection credentials (never committed to the repository).
- **Routing record**: A transient, non-persisted trace of one request: chain, method, selected upstream, latency, and outcome; source of logs and metrics.
- **Health probe result**: The current availability and latency reading for one upstream, produced by active probing and consumed by routing decisions.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A standard client library (ethers.js, viem, or web3.js) can be pointed at the gateway and use every supported chain without client-side changes, with correct results on 100% of requests in the conformance test suite.
- **SC-002**: The gateway's added latency at p50, excluding upstream latency, stays within 20% of the direct-upstream baseline as measured by the included benchmark under ~1,000 req/s sustained load.
- **SC-003**: With two upstreams configured for a chain and one forced down, 100% of read requests succeed via failover in the mock test environment, and state-changing requests are never duplicated.
- **SC-004**: The gateway passes all JSON-RPC 2.0 conformance vectors: id echo, result/error exclusivity, standard error codes, batch ordering, and notification handling.
- **SC-005**: Adding support for a new chain is demonstrated to require only configuration plus a new adapter, with zero changes to routing-core behavior.
- **SC-006**: The gateway sustains at least 1,000 requests per second on a single node against mock upstreams in the load test environment, without unbounded queue growth or dropped requests.

## Assumptions

- Transport: HTTP JSON-RPC only for v1 (confirmed with user); WebSocket support (subscriptions) is deferred to a later feature.
- Chain addressing: a single unified HTTP endpoint; the target chain is carried in the `X-Chain-Id` header, with an optional per-element override for mixed-chain batches (confirmed with user); no per-chain URL paths or ports in v1.
- Authentication (API keys) and rate limiting are out of scope for this feature and will be specified separately; this feature focuses on routing, resilience, and observability.
- No self-built UI in v1 (confirmed with user): the operational surface is the CLI, structured logs, and a Prometheus-format metrics endpoint (an example Grafana config may be provided); an admin dashboard is deferred to a later feature.
- The v1 demo chain lineup is Ethereum mainnet plus at least one L2, fully config-driven; the exact chains are decided during planning.
- A single upstream per chain is the minimum configuration; failover behavior applies when multiple upstreams are configured.
- Request payloads are never persisted; logs and metrics are ephemeral operational data.
- Deployment scale: v1 targets a single gateway node with ~1,000 req/s sustained design throughput (confirmed with user); horizontal scaling is out of scope for this feature.
- Configuration (chains, upstreams, secrets) is delivered via a config file (e.g., YAML; exact format fixed during planning) with environment-variable substitution for secret values; changes take effect on gateway restart — no hot reload in v1. Secrets are never committed to the repository.
- Batch size and request body size limits use configurable caps with documented error codes: default 100 elements per batch and 1 MB per request body (confirmed with user).
