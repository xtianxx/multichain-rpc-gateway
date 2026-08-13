package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
	"github.com/xtianxx/multichain-rpc-gateway/internal/logging"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func testConfig(chainDefs []config.Chain, timeouts map[string]int) *config.Config {
	if timeouts == nil {
		timeouts = map[string]int{"default": 10}
	}
	return &config.Config{
		Server: config.Server{Timeouts: timeouts},
		Chains: chainDefs,
	}
}

func mustParseRequest(t *testing.T) *jsonrpc.Request {
	t.Helper()
	req, jrErr := jsonrpc.ParseSingle([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","id":1}`))
	if jrErr != nil {
		t.Fatalf("ParseSingle: %v", jrErr)
	}
	return req
}

func TestNewUnknownAdapterRejected(t *testing.T) {
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "solana",
		Upstreams: []config.Upstream{{URL: "https://x.example.com"}},
	}}, nil)
	if _, err := New(cfg, discardLogger()); err == nil {
		t.Error("unregistered adapter must be rejected at router construction")
	}
}

func TestResolveChain(t *testing.T) {
	cfg := testConfig([]config.Chain{
		{ChainID: "1", Adapter: "ethereum", Upstreams: []config.Upstream{{Name: "eth-a", URL: "https://eth.example.com"}}},
		{ChainID: "8453", Adapter: "base", Upstreams: []config.Upstream{{Name: "base-a", URL: "https://base.example.com"}}},
	}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := []struct {
		name      string
		header    string
		wantChain string
		wantErr   bool
		wantData  string // data.chain_id expectation ("" = not checked)
	}{
		{"ethereum", "1", "1", false, ""},
		{"base", "8453", "8453", false, ""},
		{"leading-zeros", "08453", "8453", false, ""},
		{"unknown-chain", "999", "", true, "999"},
		{"missing-header", "", "", true, ""},
		{"invalid-format", "0x2105", "", true, "0x2105"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, jrErr := r.ResolveChain(tc.header)
			if tc.wantErr {
				if jrErr == nil {
					t.Fatalf("expected error for header %q", tc.header)
				}
				if jrErr.Code != jsonrpc.CodeChainNotConfigured {
					t.Errorf("code: got %d want -32000", jrErr.Code)
				}
				if tc.wantData != "" {
					data, _ := jrErr.Data.(map[string]any)
					if data["chain_id"] != tc.wantData {
						t.Errorf("data.chain_id: got %v want %v", data["chain_id"], tc.wantData)
					}
				}
				return
			}
			if jrErr != nil {
				t.Fatalf("unexpected error: %v", jrErr)
			}
			if ch.ChainID != tc.wantChain {
				t.Errorf("chain: got %s want %s", ch.ChainID, tc.wantChain)
			}
		})
	}
}

// mockUpstream builds an httptest upstream whose handler records the last
// forwarded envelope and replies per the given behaviour.
type mockUpstream struct {
	server *httptest.Server
	hits   atomic.Int32
	// captured forward envelope (method/params/keys), guarded by single-threaded tests
	lastBody []byte
	reply    func(method string, id json.RawMessage) (int, []byte)
	delay    time.Duration
}

func newMockUpstream(t *testing.T, reply func(method string, id json.RawMessage) (int, []byte)) *mockUpstream {
	t.Helper()
	m := &mockUpstream{reply: reply}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		m.lastBody = body
		var env map[string]json.RawMessage
		_ = json.Unmarshal(body, &env)
		var method string
		_ = json.Unmarshal(env["method"], &method)
		if m.delay > 0 {
			time.Sleep(m.delay)
		}
		status, out := m.reply(method, env["id"])
		w.WriteHeader(status)
		_, _ = w.Write(out)
	}))
	t.Cleanup(m.server.Close)
	return m
}

func TestRouteLogsStructuredRecordWithoutPayload(t *testing.T) {
	// T042 (US4): every Route call emits one structured log record with
	// routing metadata only — the request payload must never reach the log.
	mock := newMockUpstream(t, func(method string, id json.RawMessage) (int, []byte) {
		return 200, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","result":"0x1","id":%s}`, id))
	})
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "mainnet-a", URL: mock.server.URL}},
	}}, nil)

	var buf bytes.Buffer
	r, err := New(cfg, logging.NewWithOutput(&buf, slog.LevelDebug))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	req, jrErr := jsonrpc.ParseSingle([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x52908400098527886e0f7030069857d2e4169ee7","latest"],"id":7}`))
	if jrErr != nil {
		t.Fatalf("ParseSingle: %v", jrErr)
	}
	if _, jrErr, _ := r.Route(context.Background(), ch, req); jrErr != nil {
		t.Fatalf("Route: %v", jrErr)
	}

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v (%s)", err, buf.String())
	}
	if rec["msg"] != "request" {
		t.Errorf("msg: got %v want %q", rec["msg"], "request")
	}
	if rec["chain_id"] != "1" {
		t.Errorf("chain_id: got %v", rec["chain_id"])
	}
	if rec["method"] != "eth_getBalance" {
		t.Errorf("method: got %v", rec["method"])
	}
	if rec["upstream"] != "mainnet-a" {
		t.Errorf("upstream: got %v", rec["upstream"])
	}
	if rec["outcome"] != "success" {
		t.Errorf("outcome: got %v", rec["outcome"])
	}
	if lat, ok := rec["latency"].(string); !ok || lat == "" {
		t.Errorf("latency: got %v", rec["latency"])
	}
	if _, ok := rec["retries"]; !ok {
		t.Error("retries field missing from log record")
	}
	if strings.Contains(buf.String(), "52908400098527886e0f7030069857d2e4169ee7") {
		t.Error("payload address must never appear in the log record")
	}
}

func TestRouteSelectsSingleUpstreamAndBuildsRecord(t *testing.T) {
	mock := newMockUpstream(t, func(method string, id json.RawMessage) (int, []byte) {
		return 200, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","result":"0x1","id":%s}`, id))
	})
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "mainnet-a", URL: mock.server.URL}},
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
	if string(result) != `"0x1"` {
		t.Errorf("result: got %s", result)
	}
	if rec.ChainID != "1" || rec.Method != "eth_chainId" || rec.Upstream != "mainnet-a" || rec.Outcome != "success" {
		t.Errorf("record: %+v", rec)
	}
	if rec.Latency <= 0 {
		t.Errorf("latency must be positive: %v", rec.Latency)
	}
	if rec.Retries != 0 {
		t.Errorf("v1 has no retries: %+v", rec)
	}
	if mock.hits.Load() != 1 {
		t.Errorf("upstream hits: got %d want 1", mock.hits.Load())
	}
}

func TestRouteForwardsCleanEnvelopeWithNormalizedParams(t *testing.T) {
	mock := newMockUpstream(t, func(method string, id json.RawMessage) (int, []byte) {
		return 200, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","result":"ok","id":%s}`, id))
	})
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "mainnet-a", URL: mock.server.URL}},
	}}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	// EIP-1898 blockNumber object must be unwrapped before forwarding; the
	// gateway metadata member must not leak upstream.
	body := `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xabc",{"blockNumber":"0x1"}],"id":7,"x-chain-id":"1"}`
	req, jrErr := jsonrpc.ParseSingle([]byte(body))
	if jrErr != nil {
		t.Fatalf("ParseSingle: %v", jrErr)
	}
	if _, jrErr, _ := r.Route(context.Background(), ch, req); jrErr != nil {
		t.Fatalf("Route: %v", jrErr)
	}

	var fwd map[string]json.RawMessage
	if err := json.Unmarshal(mock.lastBody, &fwd); err != nil {
		t.Fatalf("forwarded body not JSON: %v (%s)", err, mock.lastBody)
	}
	if string(fwd["params"]) != `["0xabc","0x1"]` {
		t.Errorf("forwarded params: got %s", fwd["params"])
	}
	if _, leak := fwd["x-chain-id"]; leak {
		t.Error("gateway metadata must be stripped before forwarding")
	}
	if string(fwd["jsonrpc"]) != `"2.0"` {
		t.Errorf("forwarded jsonrpc: got %s", fwd["jsonrpc"])
	}
	if string(fwd["id"]) != "7" {
		t.Errorf("forwarded id: got %s", fwd["id"])
	}
}

func TestRouteUpstreamErrorPassthrough(t *testing.T) {
	mock := newMockUpstream(t, func(method string, id json.RawMessage) (int, []byte) {
		return 200, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":3,"message":"execution reverted"},"id":%s}`, id))
	})
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "mainnet-a", URL: mock.server.URL}},
	}}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	_, jrErr, rec := r.Route(context.Background(), ch, mustParseRequest(t))
	if jrErr == nil {
		t.Fatal("expected upstream error passthrough")
	}
	if jrErr.Code != 3 || jrErr.Message != "execution reverted" {
		t.Errorf("upstream error: %+v", jrErr)
	}
	if rec.Outcome != "3" {
		t.Errorf("outcome: got %q", rec.Outcome)
	}
}

func TestRouteInvalidUpstreamResponse(t *testing.T) {
	mock := newMockUpstream(t, func(method string, id json.RawMessage) (int, []byte) {
		return 200, []byte(`{"foo":1}`)
	})
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "mainnet-a", URL: mock.server.URL}},
	}}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	_, jrErr, rec := r.Route(context.Background(), ch, mustParseRequest(t))
	if jrErr == nil || jrErr.Code != jsonrpc.CodeInvalidUpstreamResponse {
		t.Fatalf("expected -32002, got %+v", jrErr)
	}
	if rec.Outcome != "-32002" {
		t.Errorf("outcome: got %q", rec.Outcome)
	}
}

func TestRouteTimeout(t *testing.T) {
	mock := newMockUpstream(t, func(method string, id json.RawMessage) (int, []byte) {
		return 200, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","result":"late","id":%s}`, id))
	})
	mock.delay = 2 * time.Second
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "mainnet-a", URL: mock.server.URL}},
	}}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, _ := r.ResolveChain("1")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, jrErr, rec := r.Route(ctx, ch, mustParseRequest(t))
	if jrErr == nil || jrErr.Code != jsonrpc.CodeUpstreamTimeout {
		t.Fatalf("expected -32005, got %+v", jrErr)
	}
	if rec.Outcome != "-32005" {
		t.Errorf("outcome: got %q", rec.Outcome)
	}
}

func TestRouteUnreachable(t *testing.T) {
	// Grab a port and close it so the upstream is guaranteed unreachable.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "dead", URL: "http://" + addr}},
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
	if rec.Outcome != "-32001" {
		t.Errorf("outcome: got %q", rec.Outcome)
	}
	if rec.Upstream != "dead" {
		t.Errorf("upstream: got %q", rec.Upstream)
	}
}

func TestMethodClassTimeoutLongestPrefix(t *testing.T) {
	rt := &Router{cfg: &config.Config{Server: config.Server{Timeouts: map[string]int{
		"default": 10, "eth_getLogs": 30, "eth_get": 20,
	}}}}
	cases := []struct {
		method string
		want   time.Duration
	}{
		{"eth_getLogs", 30 * time.Second},
		{"eth_getBalance", 20 * time.Second},
		{"eth_call", 10 * time.Second},
		{"net_version", 10 * time.Second},
	}
	for _, tc := range cases {
		if got := rt.methodTimeout(tc.method); got != tc.want {
			t.Errorf("methodTimeout(%s): got %v want %v", tc.method, got, tc.want)
		}
	}
}

func TestUnsafeMethodStillForwardedInV1(t *testing.T) {
	// Retry classification is US3; v1 forwards every method exactly once.
	mock := newMockUpstream(t, func(method string, id json.RawMessage) (int, []byte) {
		return 200, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","result":"txhash","id":%s}`, id))
	})
	cfg := testConfig([]config.Chain{{
		ChainID: "1", Adapter: "ethereum",
		Upstreams: []config.Upstream{{Name: "mainnet-a", URL: mock.server.URL}},
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
	if _, jrErr, _ := r.Route(context.Background(), ch, req); jrErr != nil {
		t.Fatalf("Route: %v", jrErr)
	}
	if mock.hits.Load() != 1 {
		t.Errorf("exactly one forward expected, got %d", mock.hits.Load())
	}
	if !strings.Contains(string(mock.lastBody), "deadbeef") {
		t.Error("raw transaction payload must be forwarded intact")
	}
}
