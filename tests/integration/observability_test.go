// Observability tests (US4, T039): the gateway records Prometheus metrics for
// routed requests and rejection outcomes. Assertions use exact counter values
// (1): no other test in this package calls metrics.Register, so the package
// level collectors are exclusively ours for the duration of this test.
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/xtianxx/multichain-rpc-gateway/internal/api"
	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
	"github.com/xtianxx/multichain-rpc-gateway/internal/metrics"
	"github.com/xtianxx/multichain-rpc-gateway/internal/router"
)

// findMetric returns the dto.Metric in family name whose label set exactly
// matches want, or nil when no such series exists.
func findMetric(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) *dto.Metric {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("registry.Gather: %v", err)
	}
	for _, fam := range fams {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.Metric {
			if len(m.Label) != len(want) {
				continue
			}
			match := true
			for _, lp := range m.Label {
				if v, ok := want[lp.GetName()]; !ok || lp.GetValue() != v {
					match = false
					break
				}
			}
			if match {
				return m
			}
		}
	}
	return nil
}

// mustMetric fails the test when the series is missing and returns it.
func mustMetric(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) *dto.Metric {
	t.Helper()
	m := findMetric(t, reg, name, want)
	if m == nil {
		t.Fatalf("series %s with labels %v not found", name, want)
	}
	return m
}

func TestObservabilityMetrics(t *testing.T) {
	// Register before building the router so New() seeds the initial gauge
	// series and routed requests are counted.
	reg := prometheus.NewRegistry()
	metrics.Register(reg)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env map[string]json.RawMessage
		_ = json.Unmarshal(body, &env)
		id := env["id"]
		fmt.Fprintf(w, `{"jsonrpc":"2.0","result":"0x1","id":%s}`, id)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.Server{Timeouts: map[string]int{"default": 10}, MaxBodyBytes: 1048576},
		Chains: []config.Chain{{
			ChainID: "1", Adapter: "ethereum",
			Upstreams: []config.Upstream{{Name: "obs-a", URL: upstream.URL}},
		}},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	rt, err := router.New(cfg, logger)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	h := api.New(rt, 1048576, 100, logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Step 1: routed success on chain 1.
	status, body := post(t, srv.URL, map[string]string{"X-Chain-Id": "1"}, `{"jsonrpc":"2.0","method":"eth_chainId","id":1}`)
	if status != 200 || !bytes.Contains(body, []byte(`"result":"0x1"`)) {
		t.Fatalf("step 1: status %d body %s", status, body)
	}

	m := mustMetric(t, reg, "gateway_requests_total", map[string]string{
		"chain": "1", "upstream": "obs-a", "method": "eth_chainId", "outcome": "success",
	})
	if got := m.Counter.GetValue(); got != 1 {
		t.Errorf("gateway_requests_total success: got %v want 1", got)
	}

	m = mustMetric(t, reg, "gateway_request_duration_seconds", map[string]string{
		"chain": "1", "upstream": "obs-a", "method": "eth_chainId",
	})
	if m.Histogram == nil || m.Histogram.GetSampleCount() != 1 {
		t.Errorf("gateway_request_duration_seconds: got sample_count %v want 1", m.Histogram.GetSampleCount())
	}

	m = mustMetric(t, reg, "gateway_requests_inflight", map[string]string{
		"chain": "1", "upstream": "obs-a",
	})
	if got := m.Gauge.GetValue(); got != 0 {
		t.Errorf("gateway_requests_inflight after completion: got %v want 0", got)
	}

	// Initial gauge series seeded by router.New: unknown health (2), closed
	// circuit (0) before any probe runs.
	m = mustMetric(t, reg, "gateway_upstream_up", map[string]string{"chain": "1", "upstream": "obs-a"})
	if got := m.Gauge.GetValue(); got != float64(metrics.UpstreamUpUnknown) {
		t.Errorf("gateway_upstream_up initial: got %v want %d", got, metrics.UpstreamUpUnknown)
	}
	m = mustMetric(t, reg, "gateway_upstream_circuit_state", map[string]string{"chain": "1", "upstream": "obs-a"})
	if got := m.Gauge.GetValue(); got != float64(metrics.CircuitClosed) {
		t.Errorf("gateway_upstream_circuit_state initial: got %v want %d", got, metrics.CircuitClosed)
	}

	// Step 2: chain resolution rejection counted with "-" placeholders.
	status, body = post(t, srv.URL, map[string]string{"X-Chain-Id": "999"}, `{"jsonrpc":"2.0","method":"eth_chainId","id":2}`)
	if status != 200 || !bytes.Contains(body, []byte("-32000")) {
		t.Fatalf("step 2: status %d body %s", status, body)
	}

	m = mustMetric(t, reg, "gateway_requests_total", map[string]string{
		"chain": "-", "upstream": "-", "method": "eth_chainId", "outcome": "-32000",
	})
	if got := m.Counter.GetValue(); got != 1 {
		t.Errorf("gateway_requests_total rejected: got %v want 1", got)
	}
}
