package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/xtianxx/multichain-rpc-gateway/internal/chain"
	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
	"github.com/xtianxx/multichain-rpc-gateway/internal/metrics"
)

// buildConfig is testConfig plus US3 retry/circuit options.
func buildConfig(chainDefs []config.Chain, timeouts map[string]int, retry config.Retry, circuit config.Circuit) *config.Config {
	cfg := testConfig(chainDefs, timeouts)
	cfg.Retry = retry
	cfg.Circuit = circuit
	return cfg
}

// garbageReply answers with a non-JSON-RPC body (invalid upstream response).
func garbageReply(method string, id json.RawMessage) (int, []byte) {
	return 200, []byte(`{"foo":1}`)
}

// validReply answers with a well-formed JSON-RPC result echoing the request id.
func validReply(result string) func(method string, id json.RawMessage) (int, []byte) {
	return func(method string, id json.RawMessage) (int, []byte) {
		return 200, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","result":%s,"id":%s}`, result, id))
	}
}

// deadURL returns an http URL for a just-closed ephemeral port, so the
// upstream is guaranteed unreachable (transport error -> -32001).
func deadURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

func TestRouteFailoverToSecondary(t *testing.T) {
	primary := newMockUpstream(t, garbageReply)
	secondary := newMockUpstream(t, validReply(`"0x2105"`))
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{
			{Name: "primary", URL: primary.server.URL},
			{Name: "secondary", URL: secondary.server.URL},
		},
	}}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	result, jrErr, rec := r.Route(context.Background(), ch, mustParseRequest(t))
	if jrErr != nil {
		t.Fatalf("Route: %v", jrErr)
	}
	if string(result) != `"0x2105"` {
		t.Errorf("result: got %s want secondary's marker", result)
	}
	if rec.Upstream != "secondary" {
		t.Errorf("rec.Upstream: got %q want secondary", rec.Upstream)
	}
	if rec.Retries != 1 {
		t.Errorf("rec.Retries: got %d want 1", rec.Retries)
	}
	if primary.hits.Load() != 1 || secondary.hits.Load() != 1 {
		t.Errorf("hits: primary=%d secondary=%d, want 1/1", primary.hits.Load(), secondary.hits.Load())
	}
}

func TestRouteWriteMethodExactlyOnce(t *testing.T) {
	primary := newMockUpstream(t, garbageReply)
	secondary := newMockUpstream(t, validReply(`"0x1"`))
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{
			{Name: "primary", URL: primary.server.URL},
			{Name: "secondary", URL: secondary.server.URL},
		},
	}}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	req, jrErr := jsonrpc.ParseSingle([]byte(`{"jsonrpc":"2.0","method":"eth_sendRawTransaction","params":["0xdeadbeef"],"id":9}`))
	if jrErr != nil {
		t.Fatalf("ParseSingle: %v", jrErr)
	}
	_, jrErr, rec := r.Route(context.Background(), ch, req)
	if jrErr == nil || jrErr.Code != jsonrpc.CodeInvalidUpstreamResponse {
		t.Fatalf("expected -32002, got %+v", jrErr)
	}
	if rec.Outcome != "-32002" {
		t.Errorf("outcome: got %q", rec.Outcome)
	}
	if primary.hits.Load() != 1 || secondary.hits.Load() != 0 {
		t.Errorf("hits: primary=%d secondary=%d, want 1/0", primary.hits.Load(), secondary.hits.Load())
	}
}

func TestRouteNotificationForwardedOnce(t *testing.T) {
	// Notifications have no response slot: a failed attempt is invisible to
	// the client, so they must never be retried (exactly one forward).
	primary := newMockUpstream(t, garbageReply)
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "primary", URL: primary.server.URL}},
	}}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	req, jrErr := jsonrpc.ParseSingle([]byte(`{"jsonrpc":"2.0","method":"eth_chainId"}`))
	if jrErr != nil {
		t.Fatalf("ParseSingle: %v", jrErr)
	}
	if _, jrErr, _ := r.Route(context.Background(), ch, req); jrErr == nil || jrErr.Code != jsonrpc.CodeInvalidUpstreamResponse {
		t.Fatalf("expected -32002, got %+v", jrErr)
	}
	if primary.hits.Load() != 1 {
		t.Errorf("notification must be forwarded exactly once, got %d hits", primary.hits.Load())
	}
}

func TestRouteAllUpstreamsDown(t *testing.T) {
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{
			{Name: "dead-a", URL: deadURL(t)},
			{Name: "dead-b", URL: deadURL(t)},
		},
	}}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	_, jrErr, rec := r.Route(context.Background(), ch, mustParseRequest(t))
	if jrErr == nil || jrErr.Code != jsonrpc.CodeUpstreamUnavailable {
		t.Fatalf("expected -32001, got %+v", jrErr)
	}
	if rec.Upstream != "dead-b" {
		t.Errorf("rec.Upstream: got %q want dead-b (last attempted)", rec.Upstream)
	}
	if rec.Outcome != "-32001" {
		t.Errorf("outcome: got %q", rec.Outcome)
	}
}

func TestRoutePerAttemptTimeoutBoundedPerAttempt(t *testing.T) {
	// T053: with no overall cap configured (max_elapsed <= 0), every
	// attempt gets its own full method-class timeout (1s here). Two slow
	// upstreams therefore take ~2s in total; the old total/maxAttempts
	// split would have given each attempt 500ms and finished in ~1s.
	primary := newMockUpstream(t, validReply(`"0x1"`))
	primary.delay = 2 * time.Second
	secondary := newMockUpstream(t, validReply(`"0x1"`))
	secondary.delay = 2 * time.Second
	cfg := buildConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{
			{Name: "slow-a", URL: primary.server.URL},
			{Name: "slow-b", URL: secondary.server.URL},
		},
	}}, map[string]int{"default": 1},
		config.Retry{MaxAttempts: 2}, // max_elapsed 0: no overall cap
		config.Circuit{},
	)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	start := time.Now()
	_, jrErr, rec := r.Route(context.Background(), ch, mustParseRequest(t))
	if jrErr == nil || jrErr.Code != jsonrpc.CodeUpstreamTimeout {
		t.Fatalf("expected -32005, got %+v", jrErr)
	}
	if rec.Outcome != "-32005" {
		t.Errorf("outcome: got %q", rec.Outcome)
	}
	if elapsed := time.Since(start); elapsed < 1800*time.Millisecond || elapsed >= 2500*time.Millisecond {
		t.Errorf("two 1s per-attempt timeouts expected (~2s total), took %v", elapsed)
	}
	if primary.hits.Load() != 1 || secondary.hits.Load() != 1 {
		t.Errorf("hits: primary=%d secondary=%d, want 1/1", primary.hits.Load(), secondary.hits.Load())
	}
}

func TestRoutePerAttemptTimeoutHonorsMethodTimeout(t *testing.T) {
	// T053: the per-attempt deadline is server.timeouts.<method> itself
	// (2s here), not a share of it. The old total/maxAttempts split would
	// allow only 1s per attempt and fail this 1.5s response.
	mock := newMockUpstream(t, validReply(`"0x1"`))
	mock.delay = 1500 * time.Millisecond
	cfg := buildConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "slow-ok", URL: mock.server.URL}},
	}}, map[string]int{"default": 2},
		config.Retry{MaxAttempts: 2}, // max_elapsed 0: no overall cap
		config.Circuit{},
	)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	start := time.Now()
	if _, jrErr, _ := r.Route(context.Background(), ch, mustParseRequest(t)); jrErr != nil {
		t.Fatalf("Route: expected success within the 2s per-attempt timeout, got %+v", jrErr)
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Errorf("per-attempt timeout must be 2s (method timeout), took %v", elapsed)
	}
	if mock.hits.Load() != 1 {
		t.Errorf("hits: got %d want 1", mock.hits.Load())
	}
}

func TestRouteOverallDeadlineBoundsSequence(t *testing.T) {
	// T053: retry.max_elapsed (1s) bounds the whole attempt+backoff
	// sequence as an outer context. The per-attempt timeout alone would
	// allow 2s here, so a 1.5s upstream response would succeed; the
	// overall deadline must fire mid-attempt and surface as -32005 before
	// any failover attempt starts.
	primary := newMockUpstream(t, validReply(`"0x1"`))
	primary.delay = 1500 * time.Millisecond
	secondary := newMockUpstream(t, validReply(`"0x1"`))
	secondary.delay = 1500 * time.Millisecond
	cfg := buildConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{
			{Name: "slow-a", URL: primary.server.URL},
			{Name: "slow-b", URL: secondary.server.URL},
		},
	}}, map[string]int{"default": 2},
		config.Retry{MaxAttempts: 2, MaxElapsed: config.Duration(time.Second)},
		config.Circuit{},
	)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	start := time.Now()
	_, jrErr, rec := r.Route(context.Background(), ch, mustParseRequest(t))
	if jrErr == nil || jrErr.Code != jsonrpc.CodeUpstreamTimeout {
		t.Fatalf("expected -32005, got %+v", jrErr)
	}
	if rec.Outcome != "-32005" {
		t.Errorf("outcome: got %q", rec.Outcome)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond || elapsed >= 1500*time.Millisecond {
		t.Errorf("overall deadline (1s) must bound the sequence, took %v", elapsed)
	}
	if rec.Upstream != "slow-a" {
		t.Errorf("rec.Upstream: got %q want slow-a (second attempt never reached)", rec.Upstream)
	}
	if secondary.hits.Load() != 0 {
		t.Errorf("secondary hits: got %d want 0 (overall deadline fired before failover)", secondary.hits.Load())
	}
}

func TestRouteAllUnhealthyExcludedFromRouting(t *testing.T) {
	// T054: probe-unhealthy upstreams are excluded from routing entirely
	// (they recover via probes, same as breaker-open). An all-unhealthy
	// chain yields the structured -32001 with zero upstream traffic.
	mock := newMockUpstream(t, validReply(`"0x1"`))
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "sick-a", URL: mock.server.URL}},
	}}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")
	ch.Upstreams[0].SetHealth(chain.HealthUnhealthy)

	_, jrErr, rec := r.Route(context.Background(), ch, mustParseRequest(t))
	if jrErr == nil || jrErr.Code != jsonrpc.CodeUpstreamUnavailable {
		t.Fatalf("expected -32001, got %+v", jrErr)
	}
	if data, _ := jrErr.Data.(map[string]any); data["chain_id"] != "1" {
		t.Errorf("data.chain_id: got %v want %q", data["chain_id"], "1")
	}
	if mock.hits.Load() != 0 {
		t.Errorf("unhealthy upstream must not be routed to, got %d hits", mock.hits.Load())
	}
	if rec.Upstream != "" {
		t.Errorf("rec.Upstream: got %q want empty (no candidate attempted)", rec.Upstream)
	}
}

func TestRouteUnknownComesAfterHealthy(t *testing.T) {
	// T054 / data-model §1.1: healthy upstreams sort ahead of unknown ones
	// (EWMA latency ascending among healthy); unknown is usable but lowest
	// priority, and unhealthy ones are excluded entirely.
	known := newMockUpstream(t, validReply(`"0x1"`))
	unknown := newMockUpstream(t, validReply(`"0x2105"`))
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{
			{Name: "known-slow", URL: known.server.URL},
			{Name: "never-probed", URL: unknown.server.URL},
		},
	}}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch := r.Chains()[0]
	knownUp, unknownUp := ch.Upstreams[0], ch.Upstreams[1]
	knownUp.SetHealth(chain.HealthHealthy)
	knownUp.SetLatency(2 * time.Second) // healthy but slow
	unknownUp.SetHealth(chain.HealthUnknown)

	result, jrErr, rec := r.Route(context.Background(), ch, mustParseRequest(t))
	if jrErr != nil {
		t.Fatalf("Route: %v", jrErr)
	}
	if string(result) != `"0x1"` {
		t.Errorf("result: got %s want known upstream's marker", result)
	}
	if rec.Upstream != "known-slow" {
		t.Errorf("rec.Upstream: got %q want known-slow (healthy beats unknown)", rec.Upstream)
	}
	if known.hits.Load() != 1 || unknown.hits.Load() != 0 {
		t.Errorf("hits: known=%d unknown=%d, want 1/0", known.hits.Load(), unknown.hits.Load())
	}
}

func TestRouteCircuitBreakerThreeStates(t *testing.T) {
	var good atomic.Bool // false: garbage reply; true: valid reply
	mock := newMockUpstream(t, func(method string, id json.RawMessage) (int, []byte) {
		if good.Load() {
			return 200, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","result":"0x1","id":%s}`, id))
		}
		return 200, []byte(`{"foo":1}`)
	})
	cfg := buildConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "breaker-a", URL: mock.server.URL}},
	}}, nil,
		config.Retry{MaxAttempts: 1}, // deterministic: one attempt per request
		config.Circuit{FailThreshold: 2, Cooldown: config.Duration(50 * time.Millisecond)},
	)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	// Request 1: failure, breaker still closed.
	if _, jrErr, _ := r.Route(context.Background(), ch, mustParseRequest(t)); jrErr == nil || jrErr.Code != jsonrpc.CodeInvalidUpstreamResponse {
		t.Fatalf("request 1: expected -32002, got %+v", jrErr)
	}
	// Request 2: second consecutive failure opens the breaker.
	if _, jrErr, _ := r.Route(context.Background(), ch, mustParseRequest(t)); jrErr == nil || jrErr.Code != jsonrpc.CodeInvalidUpstreamResponse {
		t.Fatalf("request 2: expected -32002, got %+v", jrErr)
	}
	if mock.hits.Load() != 2 {
		t.Fatalf("hits before open: got %d want 2", mock.hits.Load())
	}
	// Request 3: breaker open -> no candidates, no forward.
	if _, jrErr, _ := r.Route(context.Background(), ch, mustParseRequest(t)); jrErr == nil || jrErr.Code != jsonrpc.CodeUpstreamUnavailable {
		t.Fatalf("request 3: expected -32001, got %+v", jrErr)
	}
	if mock.hits.Load() != 2 {
		t.Errorf("open breaker must not be hit: got %d", mock.hits.Load())
	}

	// Wait out the cooldown: the next request is a half-open trial and, on
	// success, closes the breaker again.
	time.Sleep(120 * time.Millisecond)
	good.Store(true)
	result, jrErr, rec := r.Route(context.Background(), ch, mustParseRequest(t))
	if jrErr != nil {
		t.Fatalf("request 4: %v", jrErr)
	}
	if string(result) != `"0x1"` {
		t.Errorf("request 4 result: got %s", result)
	}
	if rec.Outcome != "success" {
		t.Errorf("request 4 outcome: got %q", rec.Outcome)
	}
	if mock.hits.Load() != 3 {
		t.Errorf("hits after recovery: got %d want 3", mock.hits.Load())
	}
}

func TestRoutePrefersHealthyByLatency(t *testing.T) {
	slow := newMockUpstream(t, validReply(`"0x1"`))
	fast := newMockUpstream(t, validReply(`"0x2105"`))
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{
			{Name: "slow", URL: slow.server.URL},
			{Name: "fast", URL: fast.server.URL},
		},
	}}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch := r.Chains()[0]
	if ch.ChainID != "1" {
		t.Fatalf("Chains(): first chain is %q, want 1", ch.ChainID)
	}
	slowUp, fastUp := ch.Upstreams[0], ch.Upstreams[1]
	slowUp.SetHealth(chain.HealthHealthy)
	fastUp.SetHealth(chain.HealthHealthy)
	slowUp.SetLatency(50 * time.Millisecond)
	fastUp.SetLatency(10 * time.Millisecond)

	result, jrErr, rec := r.Route(context.Background(), ch, mustParseRequest(t))
	if jrErr != nil {
		t.Fatalf("Route: %v", jrErr)
	}
	if string(result) != `"0x2105"` {
		t.Errorf("result: got %s want fast upstream's marker", result)
	}
	if rec.Upstream != "fast" {
		t.Errorf("rec.Upstream: got %q want fast (lowest latency)", rec.Upstream)
	}
	if slow.hits.Load() != 0 || fast.hits.Load() != 1 {
		t.Errorf("hits: slow=%d fast=%d, want 0/1", slow.hits.Load(), fast.hits.Load())
	}

	// A probe-unhealthy upstream is excluded from routing entirely (T054):
	// once the fast one is marked unhealthy, only the slow (healthy) one
	// can serve; the unhealthy one recovers via probes.
	fastUp.SetHealth(chain.HealthUnhealthy)
	_, jrErr, rec = r.Route(context.Background(), ch, mustParseRequest(t))
	if jrErr != nil {
		t.Fatalf("Route (unhealthy excluded): %v", jrErr)
	}
	if rec.Upstream != "slow" {
		t.Errorf("rec.Upstream: got %q want slow (healthy only candidate)", rec.Upstream)
	}
	if slow.hits.Load() != 1 || fast.hits.Load() != 1 {
		t.Errorf("hits after unhealth: slow=%d fast=%d, want 1/1", slow.hits.Load(), fast.hits.Load())
	}
}

func TestRouteReflectsBreakerOpenImmediately(t *testing.T) {
	// T057 (FR-014): a breaker transition on the request path must be
	// visible on gateway_upstream_circuit_state immediately, without
	// waiting for the next probe tick (default 10s).
	reg := prometheus.NewRegistry()
	metrics.Register(reg)

	var good atomic.Bool // false: garbage reply; true: valid reply
	mock := newMockUpstream(t, func(method string, id json.RawMessage) (int, []byte) {
		if good.Load() {
			return 200, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","result":"0x1","id":%s}`, id))
		}
		return 200, []byte(`{"foo":1}`)
	})
	cfg := buildConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "breaker-a", URL: mock.server.URL}},
	}}, nil,
		config.Retry{MaxAttempts: 1}, // deterministic: one attempt per request
		config.Circuit{FailThreshold: 2, Cooldown: config.Duration(50 * time.Millisecond)},
	)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")
	labels := map[string]string{"chain": "1", "upstream": "breaker-a"}

	// Request 1: failure, breaker still closed -> gauge 0.
	if _, jrErr, _ := r.Route(context.Background(), ch, mustParseRequest(t)); jrErr == nil || jrErr.Code != jsonrpc.CodeInvalidUpstreamResponse {
		t.Fatalf("request 1: expected -32002, got %+v", jrErr)
	}
	if got := gaugeValue(t, gatherMetrics(t, reg), "gateway_upstream_circuit_state", labels); got != metrics.CircuitClosed {
		t.Errorf("after request 1: circuit state = %v, want %d (closed)", got, metrics.CircuitClosed)
	}

	// Request 2: second consecutive failure opens the breaker; the gauge
	// must reflect it right after the request, with no probe involved.
	if _, jrErr, _ := r.Route(context.Background(), ch, mustParseRequest(t)); jrErr == nil || jrErr.Code != jsonrpc.CodeInvalidUpstreamResponse {
		t.Fatalf("request 2: expected -32002, got %+v", jrErr)
	}
	if got := gaugeValue(t, gatherMetrics(t, reg), "gateway_upstream_circuit_state", labels); got != metrics.CircuitOpen {
		t.Errorf("after request 2: circuit state = %v, want %d (open) immediately", got, metrics.CircuitOpen)
	}
	if !ch.Upstreams[0].BreakerOpen() {
		t.Fatal("expected breaker to be open after request 2")
	}
}

// gatherMetrics gathers all metric families from the registry, failing the
// test when the registry reports an error.
func gatherMetrics(t *testing.T, reg *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return mfs
}

// gaugeValue returns the gauge value of the series in the named metric
// family whose labels match want exactly; it fails the test when no series
// matches.
func gaugeValue(t *testing.T, mfs []*dto.MetricFamily, name string, want map[string]string) float64 {
	t.Helper()
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
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
			if !match {
				continue
			}
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("metric family %q with labels %v not found", name, want)
	return 0
}
