# ADR 0002: Handwritten JSON-RPC 2.0 Envelope Layer and Library Selection

- **Status**: Accepted
- **Date**: 2026-08-13
- **Deciders**: Implementation team (library versions verified 2026-08-13 per research.md §1.1)
- **Related**: [ADR 0001](./0001-language-runtime-go.md) — Go language/runtime decision

## Context

The gateway is a **passthrough proxy**: it accepts arbitrary JSON-RPC 2.0 requests on POST / and forwards them to the upstream RPC of the chain selected by the `X-Chain-Id` header (or the per-element `x-chain-id` override). It implements no RPC methods of its own.

Constitution II (non-negotiable) requires strict JSON-RPC 2.0 compliance: byte-exact id echo, result/error mutual exclusivity, batch request/response ordering, notification semantics, and the standard error codes (-32700 parse error, -32600 invalid request, -32601 method not found, -32602 invalid params, -32603 internal error). Gateway-specific failures must use the reserved server-error range -32000 to -32099 with documented codes.

Research (research.md §1.1) found that every surveyed JSON-RPC library — including go-ethereum/rpc — is built on a **method registration model** (register a handler per method). That is the opposite direction from this gateway's passthrough behavior. go-ethereum/rpc additionally carries an LGPL/GPL license burden. Writing the envelope layer by hand with `encoding/json` + `json.RawMessage` gives exact control over id echo, batch ordering, notification semantics, and the custom error code table.

The same research pass fixed the remaining library-level choices (versions verified 2026-08-13), the batch-element chain override field name, and the gateway error code allocation.

## Decision

1. **Handwritten JSON-RPC envelope layer** (`internal/jsonrpc`, stdlib only): parse/validate with `encoding/json` and `json.RawMessage` to preserve ids byte-for-byte, keep batch response order, honor notification semantics (no response element), and emit the standard + gateway error table. No JSON-RPC library is used (research.md §1.1).

2. **No go-ethereum/rpc**: rejected on both grounds — its method registration model is opposite to the passthrough direction, and it carries an LGPL/GPL license burden (research.md §1.1).

3. **Library selection** (research.md §1.1; versions verified 2026-08-13):

   | Concern | Library | Version | Notes |
   |---------|---------|---------|-------|
   | Circuit breaker | sony/gobreaker/v2 | v2.4.0 | v2 is generic, rolling-window buckets, IsExcluded; health probes go through `cb.Execute()` |
   | Retry backoff | cenkalti/backoff/v4 | v4.3.0 | ExponentialBackOff + RandomizationFactor jitter + MaxElapsedTime; `backoff.Permanent` short-circuits state-changing methods |
   | Metrics | prometheus/client_golang | v1.24.1 | `promhttp.Handler()`; latency via HistogramVec (labels: chain, upstream, method) with custom low-end-dense buckets [0.1ms … 10s]; Histogram, not Summary |
   | Config | gopkg.in/yaml.v3 | v3.0.1 | Handwritten `${VAR}` substitution before `yaml.Unmarshal` (regex replace, fail-fast on unset); deliberately not `os.ExpandEnv` to avoid mangling legitimate `$` in YAML values |

   The upstream HTTP client stays stdlib `http.Client` with a per-upstream Transport pool (MaxIdleConnsPerHost ≥ 32, IdleConnTimeout 90s, 10s Dial/TLS timeouts); fasthttp is not used (research.md §1.1). Structured logging stays on stdlib `log/slog` (ADR 0001). Total: exactly 4 runtime third-party dependencies, all actively maintained with zero transitive dependency burden (research.md §1.1), consistent with Constitution V (YAGNI).

4. **Batch element chain override field name: `x-chain-id`** (research.md §2.1). An optional per-element field that overrides the `X-Chain-Id` header. Rationale: symmetric with the header naming; the `x-` prefix marks it explicitly as a gateway extension, so it cannot collide with JSON-RPC 2.0 standard members (`jsonrpc`/`method`/`params`/`id`) and avoids semantic confusion with `eth_chainId`'s `chainId`. It accepts a JSON string or number, normalized to a decimal string before validation against config; it is gateway addressing metadata, stripped after validation and never forwarded upstream.

5. **Gateway error code allocation -32000 … -32005** (research.md §2.2, fixed contract):

   | Code | Name | Meaning | HTTP status |
   |------|------|---------|-------------|
   | -32000 | Chain not configured | Unknown chain id / no upstream configured / header missing | 200 (JSON-RPC semantics) |
   | -32001 | Upstream unavailable | All upstreams for the chain unavailable / circuit fully open | 200 |
   | -32002 | Invalid upstream response | Upstream returned non-JSON or an invalid JSON-RPC response | 200 |
   | -32003 | Batch too large | Batch element count exceeds limit (default 100) | 200 |
   | -32004 | Request body too large | Body exceeds limit (default 1 MB) | 400 (transport-level rejection) |
   | -32005 | Upstream timeout | Upstream timeout with no failover target remaining | 200 |

   -32603 (internal error) is reserved for its spec semantics and is not reused for gateway failures.

## Consequences

- The envelope layer is ours to maintain, but it is small, stable, and the only JSON-RPC surface; byte-exact id echo is achievable only by handling raw bytes by hand (research.md §1.1).
- Full control over compliance and the error contract; the fixed -32000…-32005 allocation keeps a stable, programmable contract for client libraries (ethers.js, viem, web3.js) instead of a single catch-all code (research.md §2.2).
- No LGPL/GPL license burden on the core path from geth.
- Exactly 4 runtime third-party dependencies; dev/test tools (vegeta, mock-upstream) stay out of the runtime graph (plan.md).
- The compliance obligation is now explicit and testable: the conformance vector suite (`tests/conformance/`) must cover id echo, result/error exclusivity, batch ordering, notification handling, and the standard codes (Constitution IV; quickstart.md §3).

## Rejected Alternatives

- **go-ethereum/rpc**: method registration model is opposite to the passthrough direction; LGPL/GPL license burden (research.md §1.1).
- **Any other JSON-RPC library**: all surveyed libraries use the method registration model, which fights the passthrough direction (research.md §1.1).
- **Single -32000 with `data` subcodes for all gateway failures**: loses code-level programmability for clients (research.md §2.2).
- **Reusing -32603 for gateway failures**: violates spec semantics (research.md §2.2).
- **Override field names `chainId` / `_chainId` / `target`**: `chainId` is confusable with `eth_chainId` semantics; the underscore prefix convention is unintuitive; `target` is too generic (research.md §2.1).
- **zerolog / zap for logging**: performance difference is irrelevant given near-zero hot-path log volume (research.md §1.1); stdlib slog is used instead (ADR 0001).

## References

- [research.md](../../specs/001-multichain-rpc-routing/research.md) — §1.1 library-level selection, §2.1 x-chain-id naming, §2.2 error code allocation
- [plan.md](../../specs/001-multichain-rpc-routing/plan.md) — Primary Dependencies, Constitution Check (II)
- [Constitution](../../.specify/memory/constitution.md) — II (JSON-RPC 2.0 compliance), V (YAGNI), ADR requirement
- [jsonrpc-api.md](../../specs/001-multichain-rpc-routing/contracts/jsonrpc-api.md) — error code contract (HTTP status column)
