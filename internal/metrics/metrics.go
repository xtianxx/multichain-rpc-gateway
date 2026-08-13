// Package metrics defines the Prometheus metric surface of the multichain RPC
// gateway, as fixed by the metrics contract
// (specs/001-multichain-rpc-routing/contracts/metrics-contract.md).
//
// The package-level collectors are created and registered by Register and are
// nil until then; the gateway calls Register once at startup with its registry.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metric family names, fixed by the metrics contract. All names use the
// "gateway_" prefix and snake_case, with "_total" and "_seconds" suffixes
// where required by Prometheus conventions.
const (
	requestsTotalName        = "gateway_requests_total"
	requestDurationName      = "gateway_request_duration_seconds"
	requestsInflightName     = "gateway_requests_inflight"
	upstreamUpName           = "gateway_upstream_up"
	upstreamProbeLatencyName = "gateway_upstream_probe_latency_seconds"
	upstreamCircuitStateName = "gateway_upstream_circuit_state"
)

// requestDurationBuckets are the histogram bucket upper bounds mandated by the
// metrics contract: dense buckets on the low end for accurate latency
// percentiles (p50/p95/p99 computed via histogram_quantile).
var requestDurationBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005,
	0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// Package-level collectors. They are initialized by Register and are nil
// before the first call; the gateway calls Register once at startup.
var (
	requestsTotal        *prometheus.CounterVec
	requestDuration      *prometheus.HistogramVec
	requestsInflight     *prometheus.GaugeVec
	upstreamUp           *prometheus.GaugeVec
	upstreamProbeLatency *prometheus.GaugeVec
	upstreamCircuitState *prometheus.GaugeVec
)

// Register creates and registers all gateway collectors on reg.
// It is idempotent per Registry; registering the collectors a second time on
// the same Registry panics with a duplicate-registration error.
func Register(reg prometheus.Registerer) {
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: requestsTotalName,
			Help: "Total number of gateway requests, per chain, upstream, method and outcome.",
		},
		[]string{"chain", "upstream", "method", "outcome"},
	)
	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    requestDurationName,
			Help:    "End-to-end gateway request latency in seconds, including upstream round trips.",
			Buckets: requestDurationBuckets,
		},
		[]string{"chain", "upstream", "method"},
	)
	requestsInflight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: requestsInflightName,
			Help: "Number of gateway requests currently in flight, per chain and upstream.",
		},
		[]string{"chain", "upstream"},
	)
	upstreamUp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: upstreamUpName,
			Help: "Health state of an upstream: 0 = unhealthy, 1 = healthy, 2 = unknown.",
		},
		[]string{"chain", "upstream"},
	)
	upstreamProbeLatency = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: upstreamProbeLatencyName,
			Help: "Round-trip latency in seconds of the most recent upstream health probe.",
		},
		[]string{"chain", "upstream"},
	)
	upstreamCircuitState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: upstreamCircuitStateName,
			Help: "Circuit breaker state of an upstream: 0 = closed, 1 = open, 2 = half-open.",
		},
		[]string{"chain", "upstream"},
	)

	reg.MustRegister(
		requestsTotal,
		requestDuration,
		requestsInflight,
		upstreamUp,
		upstreamProbeLatency,
		upstreamCircuitState,
	)
}

// RequestsTotal returns the gateway_requests_total counter vector.
// It is nil until Register has been called.
func RequestsTotal() *prometheus.CounterVec { return requestsTotal }

// RequestDuration returns the gateway_request_duration_seconds histogram vector.
// It is nil until Register has been called.
func RequestDuration() *prometheus.HistogramVec { return requestDuration }

// RequestsInflight returns the gateway_requests_inflight gauge vector.
// It is nil until Register has been called.
func RequestsInflight() *prometheus.GaugeVec { return requestsInflight }

// UpstreamUp returns the gateway_upstream_up gauge vector.
// It is nil until Register has been called.
func UpstreamUp() *prometheus.GaugeVec { return upstreamUp }

// UpstreamProbeLatency returns the gateway_upstream_probe_latency_seconds gauge
// vector. It is nil until Register has been called.
func UpstreamProbeLatency() *prometheus.GaugeVec { return upstreamProbeLatency }

// UpstreamCircuitState returns the gateway_upstream_circuit_state gauge vector.
// It is nil until Register has been called.
func UpstreamCircuitState() *prometheus.GaugeVec { return upstreamCircuitState }
