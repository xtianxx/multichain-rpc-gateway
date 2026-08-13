package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// wantRequestDurationBuckets is the exact bucket upper-bound set mandated by
// the metrics contract: dense buckets on the low end for accurate latency
// percentiles (p50/p95/p99 via histogram_quantile).
var wantRequestDurationBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005,
	0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// gatewayFamilyNames is the exact set of metric family names the gateway
// exposes, per the metrics contract.
var gatewayFamilyNames = []string{
	"gateway_requests_total",
	"gateway_request_duration_seconds",
	"gateway_requests_inflight",
	"gateway_upstream_up",
	"gateway_upstream_probe_latency_seconds",
	"gateway_upstream_circuit_state",
}

func TestRegister(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg) // must not panic on a fresh registry
}

func TestRegisterPanicsOnDuplicate(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register on the same registry twice must panic")
		}
	}()
	Register(reg)
}

func TestRequestsTotal(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)

	RequestsTotal().WithLabelValues("1", "mainnet-a", "eth_chainId", "success").Inc()

	mf := findFamily(t, gather(t, reg), "gateway_requests_total")
	if got := mf.GetType(); got != dto.MetricType_COUNTER {
		t.Fatalf("metric type = %v, want %v", got, dto.MetricType_COUNTER)
	}
	if len(mf.Metric) != 1 {
		t.Fatalf("got %d metrics, want 1", len(mf.Metric))
	}
	m := mf.Metric[0]
	if got := m.GetCounter().GetValue(); got != 1 {
		t.Errorf("counter value = %v, want 1", got)
	}
	assertLabels(t, m, map[string]string{
		"chain":    "1",
		"upstream": "mainnet-a",
		"method":   "eth_chainId",
		"outcome":  "success",
	})
}

func TestRequestDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)

	RequestDuration().WithLabelValues("1", "mainnet-a", "eth_chainId").Observe(0.001)

	mf := findFamily(t, gather(t, reg), "gateway_request_duration_seconds")
	if got := mf.GetType(); got != dto.MetricType_HISTOGRAM {
		t.Fatalf("metric type = %v, want %v", got, dto.MetricType_HISTOGRAM)
	}
	if len(mf.Metric) != 1 {
		t.Fatalf("got %d metrics, want 1", len(mf.Metric))
	}
	h := mf.Metric[0].GetHistogram()
	if got := h.GetSampleCount(); got != 1 {
		t.Errorf("histogram sample count = %d, want 1", got)
	}
	if got := h.GetSampleSum(); got != 0.001 {
		t.Errorf("histogram sample sum = %v, want 0.001", got)
	}
	if len(h.Bucket) != len(wantRequestDurationBuckets) {
		t.Fatalf("got %d buckets, want %d", len(h.Bucket), len(wantRequestDurationBuckets))
	}
	for i, b := range h.Bucket {
		if got := b.GetUpperBound(); got != wantRequestDurationBuckets[i] {
			t.Errorf("bucket[%d] upper bound = %v, want %v", i, got, wantRequestDurationBuckets[i])
		}
	}
}

func TestRequestsInflight(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)

	g := RequestsInflight().WithLabelValues("1", "mainnet-a")
	g.Inc()
	g.Inc()
	g.Dec()
	g.Dec()
	assertGaugeValue(t, reg, "gateway_requests_inflight", 0)
}

func TestUpstreamGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)

	UpstreamUp().WithLabelValues("1", "mainnet-a").Set(1.0)
	assertGaugeValue(t, reg, "gateway_upstream_up", 1.0)

	UpstreamCircuitState().WithLabelValues("1", "mainnet-a").Set(2.0)
	assertGaugeValue(t, reg, "gateway_upstream_circuit_state", 2.0)

	UpstreamProbeLatency().WithLabelValues("1", "mainnet-a").Set(0.05)
	assertGaugeValue(t, reg, "gateway_upstream_probe_latency_seconds", 0.05)
}

func TestFamilyNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)

	// Populate at least one series per family so that Gather returns them all.
	RequestsTotal().WithLabelValues("1", "mainnet-a", "eth_chainId", "success").Inc()
	RequestDuration().WithLabelValues("1", "mainnet-a", "eth_chainId").Observe(0.001)
	RequestsInflight().WithLabelValues("1", "mainnet-a").Inc()
	UpstreamUp().WithLabelValues("1", "mainnet-a").Set(1.0)
	UpstreamProbeLatency().WithLabelValues("1", "mainnet-a").Set(0.05)
	UpstreamCircuitState().WithLabelValues("1", "mainnet-a").Set(0.0)

	got := make(map[string]bool)
	for _, mf := range gather(t, reg) {
		got[mf.GetName()] = true
	}
	if len(got) != len(gatewayFamilyNames) {
		t.Errorf("gathered %d metric families, want %d: %v", len(got), len(gatewayFamilyNames), got)
	}
	for _, name := range gatewayFamilyNames {
		if !got[name] {
			t.Errorf("missing metric family %q", name)
		}
	}
}

func gather(t *testing.T, reg *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return mfs
}

func findFamily(t *testing.T, mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	var names []string
	for _, mf := range mfs {
		names = append(names, mf.GetName())
	}
	t.Fatalf("metric family %q not found; gathered families: %v", name, names)
	return nil
}

func assertGaugeValue(t *testing.T, reg *prometheus.Registry, name string, want float64) {
	t.Helper()
	mf := findFamily(t, gather(t, reg), name)
	if got := mf.GetType(); got != dto.MetricType_GAUGE {
		t.Fatalf("metric type = %v, want %v", got, dto.MetricType_GAUGE)
	}
	if len(mf.Metric) != 1 {
		t.Fatalf("got %d metrics, want 1", len(mf.Metric))
	}
	if got := mf.Metric[0].GetGauge().GetValue(); got != want {
		t.Errorf("gauge value = %v, want %v", got, want)
	}
}

func assertLabels(t *testing.T, m *dto.Metric, want map[string]string) {
	t.Helper()
	for _, lp := range m.GetLabel() {
		wantVal, ok := want[lp.GetName()]
		if !ok {
			t.Errorf("unexpected label %q", lp.GetName())
			continue
		}
		if lp.GetValue() != wantVal {
			t.Errorf("label %q = %q, want %q", lp.GetName(), lp.GetValue(), wantVal)
		}
		delete(want, lp.GetName())
	}
	if len(want) > 0 {
		t.Errorf("missing labels: %v", want)
	}
}
