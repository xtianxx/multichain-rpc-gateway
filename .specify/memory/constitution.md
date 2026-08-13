<!--
Sync Impact Report
==================
Version change: unversioned template scaffold → 1.0.0

Rationale: Initial ratification. The prior file was the raw Spec Kit scaffold
with all placeholders unfilled and no recorded version, dates, or values.

Modified principles (template placeholder → concrete):
- [PRINCIPLE_1_NAME] → I. Chain-Agnostic Routing
- [PRINCIPLE_2_NAME] → II. JSON-RPC 2.0 Spec Compliance (NON-NEGOTIABLE)
- [PRINCIPLE_3_NAME] → III. Resilience First: Failover & Health
- [PRINCIPLE_4_NAME] → IV. Test-First (NON-NEGOTIABLE)
- [PRINCIPLE_5_NAME] → V. Observability & Simplicity (YAGNI)

Added sections:
- Security & Operational Constraints
- Development Workflow & Quality Gates
- Governance rules (filled from placeholder)

Removed sections: none

Follow-up TODOs: none (all placeholders resolved)
-->

# Multi-Chain RPC Gateway Constitution

## Core Principles

### I. Chain-Agnostic Routing

The gateway MUST expose one uniform JSON-RPC 2.0 endpoint for every supported
chain. Chain-specific behavior — chain ID resolution, block parameter
normalization (e.g. EIP-1898), response shaping, native currency handling —
MUST be isolated in per-chain adapters. Adding support for a new chain MUST
require only configuration plus a new adapter; the routing core MUST NOT
change.

Rationale: Uniform multi-chain access is the product. Without adapter
isolation, every new chain grows core complexity and regresses existing ones.

### II. JSON-RPC 2.0 Spec Compliance (NON-NEGOTIABLE)

Every request and response MUST conform to JSON-RPC 2.0: id echo semantics,
result/error mutual exclusivity, error object shape (code, message, data),
batch request and response ordering, and notification handling. Standard error
codes MUST be used wherever the spec defines them (parse error -32700, invalid
request -32600, method not found -32601, invalid params -32602, internal error
-32603). Gateway-specific failures (upstream unreachable, chain not configured,
rate limited) MUST use the reserved server-error range -32000 to -32099 with
documented codes.

Rationale: Client libraries (ethers.js, viem, web3.js) depend on exact spec
behavior. A router that breaks spec compliance breaks every downstream client.

### III. Resilience First: Failover & Health

Routing MUST prefer healthy, low-latency upstreams, with active probe-based
health checks feeding routing decisions. On upstream failure the gateway MUST
fail over to another upstream for the same chain or return a structured server
error — it MUST NOT hang or swallow requests. Retries MUST apply only to
safe/idempotent methods (read-only methods and state-simulation methods such as
eth_call and eth_estimateGas); state-changing methods (eth_sendTransaction,
eth_sendRawTransaction) MUST NEVER be retried automatically. Retries MUST use
exponential backoff with jitter and hard caps on attempts and total deadline.
Circuit breakers MUST stop routing to endpoints that repeatedly fail.

Rationale: Reliability is the product. A router that adds failure modes or
duplicate transactions is worse than calling an upstream directly.

### IV. Test-First (NON-NEGOTIABLE)

TDD is mandatory for routing, validation, failover, and health-check logic:
write tests → see them fail → implement → refactor. Unit tests MUST cover
routing decisions, error mapping, JSON-RPC validation, and backoff logic
against a mock upstream. Integration tests MUST exercise the full gateway
against in-process mock JSON-RPC servers per chain, including batch requests,
notifications, and failover scenarios. Any change to the routing API or error
contract REQUIRES updated tests in the same change.

Rationale: As a resume project, the test suite is the evidence of engineering
quality; routing edge cases (partial batch failures, upstream timeouts) are
exactly where untested code rots.

### V. Observability & Simplicity (YAGNI)

Structured logging MUST record each request's chain, method, selected upstream,
latency, and outcome — without logging payload contents (see Security &
Operational Constraints). Metrics MUST cover request rate, error rate, and
latency percentiles per chain and per upstream. Features MUST be added only
for a demonstrated need: no speculative plugin systems, DSLs, or abstraction
layers until a second concrete use case exists.

Rationale: Simplicity keeps the codebase readable for reviewers (resume value)
and debuggable under load.

## Security & Operational Constraints

- When enabled, API-key authentication MUST be enforced by middleware; rate
  limits MUST be applied per key, never only globally.
- Chain and upstream configuration MUST come from environment variables or
  config files; secrets (API keys, upstream URLs with embedded credentials)
  MUST NOT be committed to the repository.
- Request and response payloads MUST NOT be logged in full; sensitive fields
  (private keys in eth_sendRawTransaction params, addresses, tokens) MUST be
  redacted before any logging.
- Malformed or invalid requests MUST be rejected before forwarding to
  upstreams.
- Supported chains MUST be declarable purely via configuration, so a demo or
  deployment can add chains without recompilation.
- A benchmark suite MUST measure passthrough overhead (gateway-added latency,
  excluding upstream latency). Any merge that degrades p50 overhead by more
  than 20% REQUIRES written justification.

## Development Workflow & Quality Gates

- CI MUST run linting, type checking, unit tests, and integration tests on
  every push and pull request; any failing gate blocks merge.
- Every feature or fix MUST include tests and update the README (architecture
  overview, demo instructions).
- Non-obvious decisions — including the technology stack choice, made BEFORE
  implementation begins — MUST be recorded as Architecture Decision Records
  (ADRs) in `docs/adr/`, each with decision, context, and rejected
  alternatives.
- The project MUST remain demoable: the README MUST document a one-command
  local demo that starts mock upstreams and routes real requests through the
  gateway.
- Any feature beyond a trivial fix MUST go through the Spec Kit workflow
  (specify → plan → tasks → implement).

## Governance

This constitution supersedes all other development practices; where a conflict
exists, the constitution wins. Amendments require a documented rationale, an
updated Sync Impact Report, and a semantic version bump. Versioning policy:
MAJOR for principle removals or redefinitions; MINOR for new principles or
materially expanded sections; PATCH for wording or clarification fixes. Every
pull request MUST be reviewed against this constitution; violations MUST block
merge unless an amendment is ratified first. Runtime development guidance lives
in the Spec Kit generated documents under `.specify/` (specs, plans, tasks),
which serve as the working guidance files. Complexity must be justified: any
new abstraction REQUIRES a written justification citing the concrete need it
serves.

**Version**: 1.0.0 | **Ratified**: 2026-08-13 | **Last Amended**: 2026-08-13
