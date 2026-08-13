# AGENTS.md

Go 1.26.5 JSON-RPC 2.0 gateway routing requests to per-chain upstreams.
Module `github.com/xtianxx/multichain-rpc-gateway`. Spec-Kit driven — read
`.specify/memory/constitution.md` before any non-trivial work. The spec docs
(`specs/001-multichain-rpc-routing/*.md`) are in Chinese; code, comments, and
commit messages are in English.

## Workflow rules

- TDD is non-negotiable (Constitution IV): write failing tests, implement, then
  refactor. Contract changes require updated tests in the same change.
- `specs/001-multichain-rpc-routing/tasks.md` is the progress ledger: tick
  checkboxes `[X]` as tasks complete and commit after each task or logical
  group. Phases are strictly sequential — Phase 2 done, Phase 3 done,
  Phases 4-7 (T023-T050) pending.
- No new third-party dependencies. Allowed runtime deps: prometheus/client_golang,
  gopkg.in/yaml.v3 (gobreaker, backoff are planned for later phases only — not
  yet in go.mod). Run `go mod tidy` once per work batch, never from parallel lanes.
- Commit style: `feat: phase N ...`, `docs: ...`, `build: ...` (English).
- Spec Kit hooks in `.specify/extensions.yml` are `optional: true` auto-commits —
  announce, don't force-execute them.

## Commands

- `make test` — `go test ./...` (unit + in-process integration via httptest mocks)
- `make lint` — `go vet ./...` + `gofmt -l .` (the CI-equivalent gate; both must be clean)
- `go test -run Name ./internal/router/` — single package/test
- NOT yet runnable (future phases): `make demo`, `make demo-failover`,
  `make test-conformance`, `make bench`, `make load` — they reference
  `scripts/`, `cmd/mockupstream`, `tests/conformance/`, `bench/` which don't
  exist yet.
- No root README yet (Phase 7); `specs/001-multichain-rpc-routing/quickstart.md`
  is the operational runbook.

## Architecture

- `cmd/gateway/main.go` — the only binary; wires config → logging → metrics →
  router → api.
- `internal/api` — HTTP handler lives here, NOT in cmd/gateway. tasks.md says
  `cmd/gateway/handler.go`, but package main can't be imported by in-process
  integration tests; internal/api is the committed deviation.
- `internal/` packages: `jsonrpc` (hand-written envelope parse/marshal + error
  codes), `chain` (adapter registry; ethereum and base adapters self-register
  via `init()`), `config` (yaml + ${VAR} substitution), `logging` (slog +
  secret redaction), `metrics` (prometheus), `upstream` (http.Client +
  Forward), `router` (chain resolution, upstream selection, RoutingRecord).
- Adding a chain = config + new adapter in internal/chain. The routing core
  must not change (Constitution I).
- `tests/integration/routing_test.go` — end-to-end against two httptest mock
  upstreams (chain 1 / 8453) with special methods `eth_slow` (2s), `eth_bad`
  (garbage response), `eth_error` (error passthrough); reuse when adding cases.

## Contract gotchas (specs/.../contracts/*.md are normative)

- Gateway error codes -32000..-32005 have fixed English messages locked by
  tests — never reword. HTTP status: parse error and body-too-large → 400;
  everything else → 200.
- Request ids must be echoed byte-for-byte: keep ids as `json.RawMessage`,
  never re-marshal.
- Chain addressing: `X-Chain-Id` header (decimal). v1 status quo: batch
  requests → -32600 placeholder, `eth_subscribe` → -32601 (no WebSocket);
  per-element `x-chain-id` override is US2, not yet implemented.
- Config env vars use `${VAR}` regex substitution before yaml.Unmarshal (not
  os.ExpandEnv); an unset var is a load error. Unknown YAML fields are rejected.
- Never log payloads; route through `logging.Redact`. Secrets only via env
  vars — `config.yaml` is gitignored, `config.example.yaml` is tracked.
