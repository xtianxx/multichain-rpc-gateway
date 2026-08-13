package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtianxx/multichain-rpc-gateway/internal/chain"
	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
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

func TestRouteTotalDeadlineExceeds(t *testing.T) {
	primary := newMockUpstream(t, validReply(`"0x1"`))
	primary.delay = 2 * time.Second
	secondary := newMockUpstream(t, validReply(`"0x1"`))
	secondary.delay = 2 * time.Second
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{
			{Name: "slow-a", URL: primary.server.URL},
			{Name: "slow-b", URL: secondary.server.URL},
		},
	}}, map[string]int{"default": 1})
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
	if elapsed := time.Since(start); elapsed >= 2500*time.Millisecond {
		t.Errorf("total deadline must bound the attempt sequence, took %v", elapsed)
	}
	if primary.hits.Load() != 1 || secondary.hits.Load() != 1 {
		t.Errorf("hits: primary=%d secondary=%d, want 1/1", primary.hits.Load(), secondary.hits.Load())
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

	// An unhealthy upstream is only a last resort: once the fast one is
	// marked unhealthy, the slow one serves.
	fastUp.SetHealth(chain.HealthUnhealthy)
	if _, jrErr, _ := r.Route(context.Background(), ch, mustParseRequest(t)); jrErr != nil {
		t.Fatalf("Route (unhealthy fallback): %v", jrErr)
	}
	if slow.hits.Load() != 1 || fast.hits.Load() != 1 {
		t.Errorf("hits after unhealth: slow=%d fast=%d, want 1/1", slow.hits.Load(), fast.hits.Load())
	}
}
