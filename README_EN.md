# Multichain RPC Gateway

> [中文](./README.md) | **English**

A JSON-RPC 2.0 gateway written in Go that exposes a **single HTTP endpoint** for
every supported chain and routes each request to a per-chain upstream: the
client selects the target chain with the `X-Chain-Id` HTTP header (decimal
chain id), and the gateway resolves the chain, forwards the request to a
configured upstream, and echoes the result back byte-for-byte with the exact
request id. It fully supports JSON-RPC 2.0 batches (ordered responses,
notification semantics, per-element chain override) and adds resilience on top
of plain forwarding: multiple upstreams per chain with health-probe-driven
selection, failover to healthy upstreams, bounded exponential-backoff retries
for safe methods only (`eth_sendRawTransaction` is never retried), and circuit
breakers that cut off repeatedly failing upstreams until recovery probes pass.
Operationally it ships Prometheus metrics (`GET /metrics`) covering request
rate, error rate, latency percentiles, health, and circuit state per chain and
per upstream, plus structured JSON logs with payloads and secrets redacted.
WebSocket transport is out of scope for v1 (`eth_subscribe` returns -32601).

## Architecture

```
                 +--------------------------------------------------+
                 |   client (any JSON-RPC 2.0 library: viem,        |
                 |   ethers, web3.js, curl)                         |
                 +------------------------+-------------------------+
                                          |  POST /  with X-Chain-Id header
                                          v
                 +--------------------------------------------------+
                 |              multichain-rpc-gateway (:8545)      |
                 |  +--------+   +--------+   +-----------------+   |
                 |  |  api   |-->| router |-->|  upstream pool  |   |
                 |  | jsonrpc|   | (chain |   |  (failover,     |   |
                 |  |        |   |  res.) |   |   retry, breaker)|  |
                 |  +--------+   +--------+   +-----------------+   |
                 |  prober (eth_chainId probes)                     |
                 |  metrics (:9090)   logging (redacted slog)       |
                 +----+--------------------------+------------------+
                      |  chain 1                  |  chain 8453
                      v                           v
                 +----------------+        +----------------+
                 | Ethereum       |        | Base           |
                 | upstreams      |        | upstreams      |
                 | (mainnet-a...) |        | (base-a...)    |
                 +----------------+        +----------------+
```

All configuration lives in one YAML file; adding a chain requires only a
config entry plus a new adapter in `internal/chain` — the routing core never
changes.

| Package | Responsibility |
|---|---|
| `cmd/gateway` | The only binary. Flags (`-config`, `-log-level`), config load, wiring of logging/metrics/router/api, HTTP servers, graceful shutdown on SIGINT/SIGTERM |
| `internal/api` | The single `POST /` JSON-RPC HTTP handler (body-size and batch-element caps, envelope validation, response writing) |
| `internal/config` | YAML loading with `${VAR}` environment substitution, validation, rejection of unknown fields |
| `internal/jsonrpc` | Hand-written JSON-RPC 2.0 envelope parse/marshal and error codes (no third-party RPC library) |
| `internal/chain` | Adapter registry; `ethereum` and `base` adapters self-register via `init()` |
| `internal/router` | Chain resolution from `X-Chain-Id`, per-chain upstream selection, `RoutingRecord` (source of logs and metrics) |
| `internal/upstream` | HTTP client and forwarding, failover, bounded backoff retries, circuit breaker |
| `internal/prober` | Active `eth_chainId` probes on a fixed interval; feeds health state and circuit breakers |
| `internal/metrics` | Prometheus collectors (see [Metrics](#metrics) below) |
| `internal/logging` | `slog` structured logger with payload and secret redaction |

## Quick start

Prerequisites: Go >= 1.26. No real RPC credentials needed — the demo uses
mock upstreams built into the repository (`cmd/mockupstream`), no external
services.

```bash
make demo   # starts 2 mock upstreams (chain 1 / 8453) + the gateway
```

Expected: the gateway listens on `:8545`, metrics on `:9090`; a health check
returns `ok`:

```bash
curl -s localhost:9090/healthz
```

Route a request to Base (chain 8453) — the result comes from the Base mock
upstream:

```bash
curl -s localhost:8545 -H 'Content-Type: application/json' \
  -H 'X-Chain-Id: 8453' \
  -d '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}'
# -> {"jsonrpc":"2.0","id":1,"result":"0x2105"}
```

An unknown chain is rejected with a gateway error in the reserved range and
nothing is forwarded:

```bash
curl -s localhost:8545 -H 'X-Chain-Id: 999' \
  -d '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":2}'
# -> {"jsonrpc":"2.0","id":2,"error":{"code":-32000,...}}
```

Standard client libraries work unmodified — the gateway behaves exactly like a
direct chain RPC endpoint. See the viem example below.

Automated validation and performance targets (same as
`specs/001-multichain-rpc-routing/quickstart.md`):

| Target | What it runs |
|---|---|
| `make test` | `go test ./...` — unit (routing, validation, breaker, backoff) + in-process integration on both chains |
| `make test-conformance` | JSON-RPC 2.0 conformance vectors (id echo, result/error exclusivity, batch ordering, notifications, standard error codes) |
| `make lint` | `go vet ./...` + `gofmt -l .` (CI-equivalent gate) |
| `make bench` | In-process passthrough overhead benchmark (direct vs gateway, p50 budget +20%) |
| `make load` | Sustained load via vegeta (1,000 req/s, gateway round + direct baseline round) |
| `make demo-failover` | End-to-end failover demo with mock upstream fault injection |

## Client example (viem)

The gateway is a drop-in HTTP JSON-RPC endpoint. The only thing a viem client
must do is send the `X-Chain-Id` header so the gateway knows which chain to
route to. viem's `http()` transport accepts custom fetch options, which is the
clean way to inject the header:

```js
// example.mjs — run with: node example.mjs   (npm i viem)
import { createPublicClient, defineChain, http } from "viem";

// The gateway requires this header on every request: it selects the target
// chain (decimal chain id, e.g. "8453" for Base).
const base = defineChain({
  id: 8453,
  name: "Base",
  nativeCurrency: { name: "Ether", symbol: "ETH", decimals: 18 },
  rpcUrls: { default: { http: ["http://localhost:8545"] } },
});

const client = createPublicClient({
  chain: base,
  transport: http("http://localhost:8545", {
    fetchOptions: {
      headers: { "X-Chain-Id": "8453" }, // required: routes to chain 8453
    },
  }),
});

const chainId = await client.getChainId();      // -> 8453n
const blockNumber = await client.getBlockNumber(); // -> latest block height
console.log({ chainId: chainId.toString(), blockNumber: blockNumber.toString() });
```

Start the demo (`make demo`), then run `node example.mjs`. The same pattern
works for any chain: point the URL at the gateway, set `X-Chain-Id` to the
target chain id, and keep a chain object with the matching `id`. Requests
without the header, or with an unknown chain id, are rejected with a gateway
error (-32000) and never forwarded.

## Configuration

Configuration is loaded from a YAML file at startup (`-config`, default
`config.yaml`; see `config.example.yaml`). `${VAR}` placeholders are
substituted from environment variables before parsing — an unset variable is a
load error, and unknown YAML fields are rejected. `config.yaml` itself is
gitignored; only `config.example.yaml` is tracked, and no real URLs or secrets
are committed. v1 loads configuration once at startup: there is **no hot
reload**, changes take effect on restart.

| Section | Field | Default | Description |
|---|---|---|---|
| `server` | `listen` | required | JSON-RPC listen address, e.g. `:8545` |
| `server` | `metrics_listen` | required | Listen address for `/metrics` + `/healthz`, e.g. `:9090` |
| `server` | `max_batch_elements` | `100` | Maximum JSON-RPC batch elements per request |
| `server` | `max_body_bytes` | `1048576` (1 MB) | Maximum request body size |
| `server.timeouts` | `default` | `10` | Per-attempt upstream timeout in seconds |
| `server.timeouts` | `eth_getLogs` | — | Method-level timeout override (prefix match, longest match wins) |
| `prober` | `interval` | `10s` | Active health probe interval |
| `prober` | `timeout` | `5s` | Per-probe timeout |
| `prober` | `fail_threshold` | `3` | Consecutive probe failures mark an upstream unhealthy |
| `retry` | `max_attempts` | `2` | Max attempts for safe methods (includes the first attempt) |
| `retry` | `base_delay` | `10ms` | Exponential backoff base delay |
| `retry` | `max_elapsed` | `30s` | Overall retry deadline |
| `circuit` | `fail_threshold` | `5` | Consecutive failures open the circuit breaker |
| `circuit` | `cooldown` | `30s` | Open -> half-open cooldown |
| `chains[]` | `chain_id` | required | Decimal chain id string, unique |
| `chains[]` | `adapter` | required | Registered adapter name (`ethereum`, `base`) |
| `chains[].upstreams[]` | `name` | redacted URL | Log/metric alias for the upstream |
| `chains[].upstreams[]` | `url` | required | Upstream endpoint, `http`/`https` |

Gateway flags: `-config <path>` (default `config.yaml`) and
`-log-level <debug|info|warn|error>` (default `info`).

## Metrics

Prometheus-format metrics are exposed at `GET /metrics` on the metrics
listener (`:9090` in the demo); liveness/readiness is at `GET /healthz`. All
gateway metrics use the `gateway_` prefix. Buckets for
`gateway_request_duration_seconds` are dense on the low end so p50/p95/p99 can
be computed with `histogram_quantile`.

| Metric | Type | Labels |
|---|---|---|
| `gateway_requests_total` | counter | `chain`, `upstream`, `method`, `outcome` |
| `gateway_request_duration_seconds` | histogram | `chain`, `upstream`, `method` |
| `gateway_requests_inflight` | gauge | `chain`, `upstream` |
| `gateway_upstream_up` | gauge (0 unhealthy / 1 healthy / 2 unknown) | `chain`, `upstream` |
| `gateway_upstream_probe_latency_seconds` | gauge | `chain`, `upstream` |
| `gateway_upstream_circuit_state` | gauge (0 closed / 1 open / 2 half-open) | `chain`, `upstream` |

Example:

```bash
curl -s localhost:9090/metrics | grep gateway_
# -> gateway_requests_total{chain="8453",upstream="base-a",method="eth_chainId",outcome="success"} 1
```

## Benchmark

Measured with `make bench` (`go test -bench . -benchtime=10s -count=5 ./bench/`) — two TCP hops
(bench → server → mock upstream) kept identical for baseline and gateway; the delta
is the gateway pipeline cost (parse + chain resolution + metrics/logging) excluding
upstream latency (FR-017 / SC-002).

**Environment:** `linux/amd64`, `AMD Ryzen 7 5800H with Radeon Graphics (16 cores)`, `Go 1.26.5`, `bench/passthrough_test.go`
(mock upstream echoes `{"jsonrpc":"2.0","id":<id>,"result":"0x1"}`).

**Latest run (2026-08-31, `tee /tmp/bench.txt`):**

| run | BenchmarkPassthrough p50_ns/op | ns/op | BenchmarkGateway p50_ns/op | ns/op | Δ p50 |
|-----|-------------------------------:|------:|---------------------------:|------:|------:|
| 1 | 1 009 530 | 1 052 340 | 1 152 854 | 1 182 407 | +14.2% |
| 2 | 1 000 524 | 1 020 564 | 1 127 497 | 1 158 025 | +12.7% |
| 3 |   996 465 | 1 016 416 | 1 096 123 | 1 112 370 | +10.0% |
| 4 |   995 077 | 1 015 483 | 1 142 843 | 1 182 548 | +14.8% |
| 5 | 1 057 237 | 1 081 621 | 1 182 315 | 1 228 226 | +11.8% |

- Median p50: passthrough **1 000 524 ns** → gateway **1 142 843 ns**, overhead **+142 319 ns (+14.2%)** ≤ budget **+20%** ✅
- Mean p50: 1 011 767 ns → 1 140 326 ns (**+12.7%**) — also within budget
- Mean ns/op (timer): 1 037 285 ns → 1 172 715 ns (**+13.1%**)

All 5 runs pass individually (worst +14.8% / +18.8% on ns/op basis on run 5 still < +20%). Prior acceptance (T049) was +8.2% on a faster host; variance is host/ambient-load dependent but budget holds.

Reproduce:

```bash
make bench
# raw: go test -bench . -benchtime=10s -count=5 ./bench/ | tee /tmp/bench.txt
```

Full raw output (`/tmp/bench.txt`):

```
BenchmarkPassthrough-16    10000    1052340 ns/op    1009530 p50_ns/op
BenchmarkPassthrough-16    10000    1020564 ns/op    1000524 p50_ns/op
BenchmarkPassthrough-16    11757    1016416 ns/op     996465 p50_ns/op
BenchmarkPassthrough-16    11599    1015483 ns/op     995077 p50_ns/op
BenchmarkPassthrough-16    10000    1081621 ns/op    1057237 p50_ns/op
BenchmarkGateway-16         9708    1182407 ns/op    1152854 p50_ns/op
BenchmarkGateway-16        10000    1158025 ns/op    1127497 p50_ns/op
BenchmarkGateway-16        10000    1112370 ns/op    1096123 p50_ns/op
BenchmarkGateway-16        10000    1182548 ns/op    1142843 p50_ns/op
BenchmarkGateway-16         8229    1228226 ns/op    1182315 p50_ns/op
```

See `bench/passthrough_test.go:reportP50` for p50 calculation and `specs/001-multichain-rpc-routing/quickstart.md §4` / `.github/workflows/ci.yml` for the CI gate.

## Reference

- Feature spec: `specs/001-multichain-rpc-routing/spec.md`
- Runbook (end-to-end verification): `specs/001-multichain-rpc-routing/quickstart.md`
- Contracts (normative): `specs/001-multichain-rpc-routing/contracts/` —
  `jsonrpc-api.md` (JSON-RPC envelope and error codes, including the gateway
  reserved range **-32000..-32005**: unknown chain -32000, upstream
  unavailable -32001, batch over cap -32003, body too large -32004, timeout
  -32005), `config-contract.md`, `metrics-contract.md`
- Data model and routing order: `specs/001-multichain-rpc-routing/data-model.md`
- Architecture decision records: `docs/adr/`
