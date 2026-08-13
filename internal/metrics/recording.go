// Recording helpers: the single write path for gateway metrics, shared by
// the router (request path), the prober (health gauges), and the HTTP
// handler (rejection outcomes). All helpers are nil-safe: before Register
// has been called (e.g. packages tested in isolation) they are no-ops, so
// call sites never need to check registration state.
package metrics

import "time"

// Gauge values for gateway_upstream_up, fixed by the metrics contract:
// 0 = unhealthy, 1 = healthy, 2 = unknown (initial state before first probe).
const (
	UpstreamUpUnhealthy = 0
	UpstreamUpHealthy   = 1
	UpstreamUpUnknown   = 2
)

// Gauge values for gateway_upstream_circuit_state, fixed by the metrics
// contract: 0 = closed, 1 = open, 2 = half-open.
const (
	CircuitClosed   = 0
	CircuitOpen     = 1
	CircuitHalfOpen = 2
)

// RecordRequest increments gateway_requests_total for one completed request.
// outcome is "success" or a JSON-RPC error code string ("-32002", ...).
func RecordRequest(chainID, upstream, method, outcome string) {
	if requestsTotal == nil {
		return
	}
	requestsTotal.WithLabelValues(chainID, upstream, method, outcome).Inc()
}

// RecordRequestLatency observes gateway_request_duration_seconds for one
// completed request (end-to-end latency, including upstream round trips).
func RecordRequestLatency(chainID, upstream, method string, d time.Duration) {
	if requestDuration == nil {
		return
	}
	requestDuration.WithLabelValues(chainID, upstream, method).Observe(d.Seconds())
}

// TrackInflight increments gateway_requests_inflight for a chain/upstream
// and returns the matching decrement function (intended for defer).
func TrackInflight(chainID, upstream string) func() {
	if requestsInflight == nil {
		return func() {}
	}
	g := requestsInflight.WithLabelValues(chainID, upstream)
	g.Inc()
	return g.Dec
}

// SetUpstreamUp sets gateway_upstream_up for one chain/upstream to one of
// the UpstreamUp* values.
func SetUpstreamUp(chainID, upstream string, value int) {
	if upstreamUp == nil {
		return
	}
	upstreamUp.WithLabelValues(chainID, upstream).Set(float64(value))
}

// SetUpstreamProbeLatency sets gateway_upstream_probe_latency_seconds to the
// most recent probe round-trip time for one chain/upstream.
func SetUpstreamProbeLatency(chainID, upstream string, d time.Duration) {
	if upstreamProbeLatency == nil {
		return
	}
	upstreamProbeLatency.WithLabelValues(chainID, upstream).Set(d.Seconds())
}

// SetUpstreamCircuitState sets gateway_upstream_circuit_state for one
// chain/upstream to one of the Circuit* values.
func SetUpstreamCircuitState(chainID, upstream string, value int) {
	if upstreamCircuitState == nil {
		return
	}
	upstreamCircuitState.WithLabelValues(chainID, upstream).Set(float64(value))
}
