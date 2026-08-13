// Package bench holds the in-process passthrough overhead benchmarks (T043,
// US4 observability). They quantify the gateway's added latency over a direct
// upstream call — the SC-002 passthrough budget defined in
// specs/001-multichain-rpc-routing/quickstart.md §4: BenchmarkDirect vs
// BenchmarkGateway ns/op, gateway increment = the difference, and the gateway
// p50 must stay within direct +20%.
//
// Fairness argument: both benchmarks measure one complete loopback HTTP
// round trip through the same mock upstream (full net/http path, real
// sockets, real TCP). BenchmarkGateway adds exactly one more hop — through
// the gateway's own httptest server — plus the production pipeline between
// the hops: body parsing (internal/jsonrpc), chain resolution
// (internal/router), envelope rebuild, upstream forward with pooled client,
// response validation and shaping, per-request slog record at info level
// (internal/logging, redaction included), and Prometheus recording overhead
// (internal/metrics, registered on a real registry). The difference between
// the two numbers is therefore precisely the gateway's incremental cost for
// one passthrough request.
package bench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/xtianxx/multichain-rpc-gateway/internal/api"
	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
	"github.com/xtianxx/multichain-rpc-gateway/internal/logging"
	"github.com/xtianxx/multichain-rpc-gateway/internal/metrics"
	"github.com/xtianxx/multichain-rpc-gateway/internal/router"
)

// passthroughBody is the fixed single-request payload both benchmarks POST.
// The id is a bare JSON number so byte-for-byte id echoing is exercised.
const passthroughBody = `{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`

// benchClient is the plain http.Client shared by both benchmarks, created
// once in setup. Keeping one client for the whole run avoids per-iteration
// connection setup dominating the measurement. The gateway itself uses its
// own pooled per-upstream client (upstream.NewHTTPClient), as in production.
var benchClient *http.Client

// setup builds the full benchmark fixture and returns the direct upstream
// URL, the gateway URL, and a cleanup function:
//
//   - mock upstream: httptest server echoing the request id byte-for-byte in
//     a minimal {"jsonrpc":"2.0","id":<id>,"result":"0x1"} response, matching
//     the shape jsonrpc.ValidUpstreamResponse requires
//   - metrics: metrics.Register on a fresh prometheus registry, so the
//     gateway benchmark includes the production metric recording cost
//   - logger: production-style JSON slog logger at info level writing to
//     io.Discard (redaction runs on every record, exactly like production)
//   - config: built programmatically — no config.Load, no env vars; the
//     circuit breaker is wired (FailThreshold 5) like production
//   - gateway: router.New + api.New behind a second httptest server, the
//     full HTTP path including the X-Chain-Id resolution step
func setup() (directURL, gatewayURL string, cleanup func()) {
	// Mock upstream: minimal passthrough responder. It only needs to echo
	// the id member; the result value is fixed.
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var envelope struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id := envelope.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":"0x1"}`, id)
	}))

	// Production recording overhead: the metrics collectors are package-level
	// and only non-nil after Register; registering them on a real registry
	// keeps the gateway measurement faithful.
	metrics.Register(prometheus.NewRegistry())

	// Production logging overhead: JSON slog handler with redaction at info
	// level; the router logs one record per request.
	logger := logging.NewWithOutput(io.Discard, slog.LevelInfo)

	cfg := &config.Config{
		Server:  config.Server{Timeouts: map[string]int{"default": 10}},
		Retry:   config.Retry{MaxAttempts: 2, BaseDelay: config.Duration(10 * time.Millisecond), MaxElapsed: config.Duration(30 * time.Second)},
		Circuit: config.Circuit{FailThreshold: 5, Cooldown: config.Duration(30 * time.Second)}, // breaker wired, like production
		Chains: []config.Chain{{
			ChainID: "1",
			Adapter: "ethereum", // self-registers via init()
			Upstreams: []config.Upstream{{
				Name: "bench-a",
				URL:  upstreamSrv.URL,
			}},
		}},
	}

	rt, err := router.New(cfg, logger)
	if err != nil {
		upstreamSrv.Close()
		panic(fmt.Sprintf("bench setup: router.New: %v", err))
	}
	handler := api.New(rt, 1<<20, 100, logger)
	gatewaySrv := httptest.NewServer(handler)

	benchClient = &http.Client{} // plain client, created once per setup
	return upstreamSrv.URL, gatewaySrv.URL, func() {
		gatewaySrv.Close()
		upstreamSrv.Close()
	}
}

// postOnce performs one POST round trip and asserts HTTP 200. The full
// wall-clock time from just before the request to just after the body has
// been read and closed is the per-op measurement.
func postOnce(b *testing.B, url string, body []byte, headers map[string]string) time.Duration {
	b.Helper()
	start := time.Now()

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		b.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := benchClient.Do(req)
	if err != nil {
		b.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body) // read fully so the connection is reusable
	if err != nil {
		b.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	return time.Since(start)
}

// reportP50 reports the median per-op latency as a custom metric. The
// durations slice is O(b.N) memory — bounded by typical bench runs (~10s at
// the Makefile benchtime), i.e. on the order of 10^5 entries, so the cost is
// acceptable for the p50 signal.
func reportP50(b *testing.B, durations []time.Duration) {
	b.Helper()
	if len(durations) == 0 {
		return
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	var median time.Duration
	switch n := len(durations); n % 2 {
	case 1:
		median = durations[n/2]
	default:
		median = (durations[n/2-1] + durations[n/2]) / 2
	}
	b.ReportMetric(float64(median.Nanoseconds()), "p50_ns/op")
}

// BenchmarkDirect measures the baseline: one loopback HTTP round trip from
// the bench process straight to the mock upstream. The wall-clock round trip
// is the measurement — no StopTimer/StartTimer wrapping.
func BenchmarkDirect(b *testing.B) {
	directURL, _, cleanup := setup()
	defer cleanup()

	body := []byte(passthroughBody)
	b.ResetTimer()

	durations := make([]time.Duration, 0, b.N)
	for i := 0; i < b.N; i++ {
		durations = append(durations, postOnce(b, directURL, body, nil))
	}
	reportP50(b, durations)
}

// BenchmarkGateway measures the same request through the full gateway
// pipeline: bench -> gateway httptest server -> router (resolution,
// envelope rebuild, retry/failover logic) -> mock upstream -> back. One
// extra loopback hop plus the routing/envelope/metrics/logging overhead on
// top of BenchmarkDirect — that difference is the SC-002 passthrough
// budget.
func BenchmarkGateway(b *testing.B) {
	_, gatewayURL, cleanup := setup()
	defer cleanup()

	body := []byte(passthroughBody)
	b.ResetTimer()

	durations := make([]time.Duration, 0, b.N)
	for i := 0; i < b.N; i++ {
		durations = append(durations, postOnce(b, gatewayURL, body, map[string]string{"X-Chain-Id": "1"}))
	}
	reportP50(b, durations)
}
