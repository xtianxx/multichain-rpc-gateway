// Behavior tests for the recording helpers (recording.go): the single write
// path for gateway metrics, per the metrics contract
// (specs/001-multichain-rpc-routing/contracts/metrics-contract.md).
//
// Every test registers the package-level collectors on a fresh registry —
// Register reassigns the package-level vecs, so each test starts from an
// empty, deterministic state. Tests in this package are not parallel.
//
// The nil-safe no-op behavior (recording before Register) is structural in
// recording.go and skipped here: the package-level vecs are shared across
// tests in this binary, so registration state is order-dependent.
package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestRecordRequestAccumulatesByOutcome(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)

	// Two successes and one -32002 (invalid upstream response) on the same
	// chain/upstream/method must yield three series with exact counts, plus a
	// separate series for the other chain.
	RecordRequest("1", "eth-a", "eth_chainId", "success")
	RecordRequest("1", "eth-a", "eth_chainId", "success")
	RecordRequest("1", "eth-a", "eth_chainId", "-32002")
	RecordRequest("8453", "base-a", "eth_call", "success")

	mf := findFamily(t, gather(t, reg), "gateway_requests_total")
	if got := mf.GetType(); got != dto.MetricType_COUNTER {
		t.Fatalf("metric type = %v, want %v", got, dto.MetricType_COUNTER)
	}
	if len(mf.Metric) != 3 {
		t.Fatalf("got %d series, want 3", len(mf.Metric))
	}

	cases := []struct {
		labels map[string]string
		want   float64
	}{
		{map[string]string{"chain": "1", "upstream": "eth-a", "method": "eth_chainId", "outcome": "success"}, 2},
		{map[string]string{"chain": "1", "upstream": "eth-a", "method": "eth_chainId", "outcome": "-32002"}, 1},
		{map[string]string{"chain": "8453", "upstream": "base-a", "method": "eth_call", "outcome": "success"}, 1},
	}
	for _, tc := range cases {
		m := findMetricByLabels(t, mf, tc.labels)
		if got := m.GetCounter().GetValue(); got != tc.want {
			t.Errorf("counter value for %v = %v, want %v", tc.labels, got, tc.want)
		}
		assertLabels(t, m, tc.labels)
	}
}

func TestRecordRequestLatencyObserves(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)

	RecordRequestLatency("1", "eth-a", "eth_chainId", 150*time.Millisecond)

	mf := findFamily(t, gather(t, reg), "gateway_request_duration_seconds")
	if got := mf.GetType(); got != dto.MetricType_HISTOGRAM {
		t.Fatalf("metric type = %v, want %v", got, dto.MetricType_HISTOGRAM)
	}
	if len(mf.Metric) != 1 {
		t.Fatalf("got %d metrics, want 1", len(mf.Metric))
	}
	m := mf.Metric[0]
	assertLabels(t, m, map[string]string{
		"chain":    "1",
		"upstream": "eth-a",
		"method":   "eth_chainId",
	})
	h := m.GetHistogram()
	if got := h.GetSampleCount(); got != 1 {
		t.Errorf("histogram sample count = %d, want 1", got)
	}
	if got := h.GetSampleSum(); got != 0.15 {
		t.Errorf("histogram sample sum = %v, want 0.15", got)
	}
}

func TestTrackInflightIncDec(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)

	// Mid-flight: the returned decrement func has not been called yet, so the
	// gauge must already read 1.
	done := TrackInflight("1", "eth-a")
	assertGaugeValue(t, reg, "gateway_requests_inflight", 1)

	done()
	assertGaugeValue(t, reg, "gateway_requests_inflight", 0)
}

func TestSetUpstreamGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)

	// The same label set is overwritten, never duplicated: setting unknown
	// then healthy must leave exactly one series with the last value.
	SetUpstreamUp("1", "eth-a", UpstreamUpUnknown)
	SetUpstreamUp("1", "eth-a", UpstreamUpHealthy)

	mf := findFamily(t, gather(t, reg), "gateway_upstream_up")
	if got := mf.GetType(); got != dto.MetricType_GAUGE {
		t.Fatalf("metric type = %v, want %v", got, dto.MetricType_GAUGE)
	}
	if len(mf.Metric) != 1 {
		t.Fatalf("got %d series, want 1", len(mf.Metric))
	}
	m := mf.Metric[0]
	if got := m.GetGauge().GetValue(); got != float64(UpstreamUpHealthy) {
		t.Errorf("gauge value = %v, want %v", got, float64(UpstreamUpHealthy))
	}
	assertLabels(t, m, map[string]string{"chain": "1", "upstream": "eth-a"})

	SetUpstreamProbeLatency("1", "eth-a", 5*time.Millisecond)
	assertGaugeValue(t, reg, "gateway_upstream_probe_latency_seconds", 0.005)

	// Distinct series per circuit state, so all three contract values are
	// observable simultaneously.
	SetUpstreamCircuitState("1", "eth-a", CircuitClosed)
	SetUpstreamCircuitState("8453", "base-a", CircuitOpen)
	SetUpstreamCircuitState("1", "base-b", CircuitHalfOpen)

	cmf := findFamily(t, gather(t, reg), "gateway_upstream_circuit_state")
	if got := cmf.GetType(); got != dto.MetricType_GAUGE {
		t.Fatalf("metric type = %v, want %v", got, dto.MetricType_GAUGE)
	}
	if len(cmf.Metric) != 3 {
		t.Fatalf("got %d series, want 3", len(cmf.Metric))
	}
	circuitCases := []struct {
		labels map[string]string
		want   float64
	}{
		{map[string]string{"chain": "1", "upstream": "eth-a"}, float64(CircuitClosed)},
		{map[string]string{"chain": "8453", "upstream": "base-a"}, float64(CircuitOpen)},
		{map[string]string{"chain": "1", "upstream": "base-b"}, float64(CircuitHalfOpen)},
	}
	for _, tc := range circuitCases {
		m := findMetricByLabels(t, cmf, tc.labels)
		if got := m.GetGauge().GetValue(); got != tc.want {
			t.Errorf("circuit state for %v = %v, want %v", tc.labels, got, tc.want)
		}
		assertLabels(t, m, tc.labels)
	}
}

func TestRecordingGaugeConstants(t *testing.T) {
	// The gauge values are fixed by the metrics contract; consumers (prober,
	// circuit breaker) rely on them, so renumbering is a breaking change.
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"UpstreamUpUnhealthy", UpstreamUpUnhealthy, 0},
		{"UpstreamUpHealthy", UpstreamUpHealthy, 1},
		{"UpstreamUpUnknown", UpstreamUpUnknown, 2},
		{"CircuitClosed", CircuitClosed, 0},
		{"CircuitOpen", CircuitOpen, 1},
		{"CircuitHalfOpen", CircuitHalfOpen, 2},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d (metrics contract)", tc.name, tc.got, tc.want)
		}
	}
}

// findMetricByLabels returns the single series in mf whose label set equals
// want. Unlike assertGaugeValue — which assumes a single-series family — this
// locates one series inside a family that carries several.
func findMetricByLabels(t *testing.T, mf *dto.MetricFamily, want map[string]string) *dto.Metric {
	t.Helper()
	for _, m := range mf.Metric {
		got := make(map[string]string, len(m.GetLabel()))
		for _, lp := range m.GetLabel() {
			got[lp.GetName()] = lp.GetValue()
		}
		if len(got) != len(want) {
			continue
		}
		match := true
		for k, v := range want {
			if got[k] != v {
				match = false
				break
			}
		}
		if match {
			return m
		}
	}
	t.Fatalf("metric family %q has no series with labels %v", mf.GetName(), want)
	return nil
}
