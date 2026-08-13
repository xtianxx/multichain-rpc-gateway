# ADR 0001: Go as Language and Runtime

- **Status**: Accepted
- **Date**: 2026-08-13
- **Deciders**: User + implementation team (Go confirmed by user in the 2026-08-13 session; research.md §1)
- **Related**: [ADR 0002](./0002-jsonrpc-envelope-handwritten.md) — JSON-RPC envelope layer and library selection

## Context

The multichain RPC gateway is a single-node web service (plan.md) that accepts JSON-RPC 2.0 requests on one HTTP endpoint and routes them, per `X-Chain-Id`, to the configured upstream RPC of the target chain. The workload properties that constrain the runtime choice:

- Sustained single-node throughput of ≥ 1,000 req/s (SC-006) and gateway-added p50 latency ≤ direct upstream baseline + 20% (SC-002) — plan.md Performance Goals.
- Heavy upstream fan-out with per-request timeouts, retries with backoff, and circuit breaking; the gateway must never hang (Constitution III).
- Per-chain/per-upstream Prometheus metrics and redacted structured logging (Constitution V).
- Target platform: Linux server, single node v1 (plan.md).
- Runtime dependency count is a design constraint: Constitution V (YAGNI) and the plan cap runtime third-party dependencies at four.

research.md §1 records the decision: Go, confirmed by the user in the 2026-08-13 session. The documented rationale: single-node ~1,000 req/s sustained throughput with heavy upstream fan-out and timeout/circuit management is Go's comfort zone; the ecosystem overlaps with geth/erigon; the standard library HTTP stack is production-grade; the official Prometheus client is first-class; and Go offers the best balance of development efficiency and performance, fitting the project's resume-engineering positioning.

## Decision

Adopt **Go** (go 1.26 line — go1.26.5, 2026-07) as the language and runtime, with the standard library as the backbone:

- **HTTP serving and upstream client**: stdlib `net/http`, including the enhanced ServeMux (method routing, `"POST /{$}"` exact match). Only three endpoints exist (POST /, GET /metrics, GET /healthz), so no third-party router is needed (chi explicitly not required — research.md §1.1).
- **Structured logging**: stdlib `log/slog` JSON handler — the only logging dependency, with whitelist + `ReplaceAttr` double redaction (research.md §1.1).
- **Upgrade path**: Go 1.27 can be adopted directly once released, without code changes (plan.md Technical Context; research.md §1.1).
- **Runtime third-party dependencies**: capped at four — sony/gobreaker v2, cenkalti/backoff v4, prometheus/client_golang, gopkg.in/yaml.v3 (selected in ADR 0002). vegeta and the mock-upstream tool are dev/test-only, not runtime dependencies (plan.md).

## Consequences

Positive:

- Single static binary and straightforward Linux deployment; the goroutine-per-request model fits the fan-out + timeout/circuit workload (research.md §1).
- Zero-dependency routing: the enhanced stdlib ServeMux covers the three-endpoint surface (research.md §1.1).
- Official Prometheus client integration is first-class, satisfying Constitution V metrics requirements (research.md §1).
- slog removes any third-party logging dependency; hot-path log volume is near zero, so performance differences versus zerolog/zap are irrelevant (research.md §1.1).

Negative:

- The performance budget is now binding: ≥ 1,000 req/s sustained (SC-006) and p50 overhead ≤ direct +20% (SC-002). The benchmark suite in `bench/` must stay green as a merge gate, and any regression > 20% requires written justification (Constitution, Security & Operational Constraints).

Neutral:

- The team must track Go releases; the 1.26 → 1.27 upgrade is expected to be mechanical with no code changes (research.md §1.1).

## Rejected Alternatives

From research.md §1 (Alternatives considered):

- **Rust (tokio/axum)**: best performance/memory profile, but slower development velocity and a weaker ecosystem relative to Go, risking the v1 delivery cadence.
- **TypeScript (Node.js)**: shares a language with ethers.js/viem, but the single-threaded event loop adds complexity under high fan-out with timeout management.
- **Python (FastAPI)**: fastest development, but insufficient performance headroom at the 1,000 req/s target — threatens SC-002/SC-006.

## References

- [plan.md](../../specs/001-multichain-rpc-routing/plan.md) — Technical Context, Performance Goals, Constitution Check
- [research.md](../../specs/001-multichain-rpc-routing/research.md) — §1 technology stack selection, §1.1 library-level selection
- [Constitution](../../.specify/memory/constitution.md) — III (Resilience First), V (Observability & Simplicity), ADR requirement
