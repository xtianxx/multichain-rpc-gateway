// Package integration exercises the gateway end-to-end against in-process
// mock upstreams (httptest), per tasks.md US3 (upstream failover). These
// tests are TDD: they are expected to fail until the router failover/retry
// rewrite lands.
package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtianxx/multichain-rpc-gateway/internal/api"
	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
	"github.com/xtianxx/multichain-rpc-gateway/internal/router"
)

// flakyUpstream serves a JSON-RPC upstream whose responses are controlled by
// an atomic mode: 0=ok, 1=http500, 2=garbage, 3=slow2s. The ok response
// echoes the request id byte-for-byte (json.RawMessage, never re-marshalled).
type flakyUpstream struct {
	server *httptest.Server
	hits   atomic.Int32
	marker string // result payload, e.g. "primary" / "secondary"
	mode   atomic.Int32
}

// newFlakyUpstream starts the mock server and registers its cleanup so a
// slow2s handler still sleeping when the test ends is closed with the test.
func newFlakyUpstream(t *testing.T, marker string) *flakyUpstream {
	t.Helper()
	m := &flakyUpstream{marker: marker}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"foo":1}`))
			return
		}

		switch m.mode.Load() {
		case 1: // http500
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("boom"))
			return
		case 2: // garbage JSON with 200
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"foo":1}`))
			return
		case 3: // slow: sleep 2s, then fall through to the ok response
			time.Sleep(2 * time.Second)
		}

		if len(env.ID) == 0 {
			w.WriteHeader(http.StatusOK)
			return
		}
		out, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"result":  "from-" + m.marker,
			"id":      env.ID, // json.RawMessage: byte-exact echo
		})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(out)
	}))
	t.Cleanup(m.server.Close)
	return m
}

// startFailoverGateway builds the gateway under test exactly like
// routing_test.go's startGateway, but for a single chain ("1", ethereum
// adapter) with the given upstream candidates. No circuit breaker is
// configured in these e2e tests.
func startFailoverGateway(t *testing.T, ups []config.Upstream, timeouts map[string]int) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{
			Timeouts:         timeouts,
			MaxBatchElements: 10,
			MaxBodyBytes:     1 << 20,
		},
		Chains: []config.Chain{{ChainID: "1", Adapter: "ethereum", Upstreams: ups}},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	rt, err := router.New(cfg, logger)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	h := api.New(rt, cfg.Server.MaxBodyBytes, cfg.Server.MaxBatchElements, logger)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// failoverUpstreams wraps the two flaky upstreams as config candidates in
// primary-then-secondary order.
func failoverUpstreams(primary, secondary *flakyUpstream) []config.Upstream {
	return []config.Upstream{
		{Name: "primary", URL: primary.server.URL},
		{Name: "secondary", URL: secondary.server.URL},
	}
}

var chainHeader = map[string]string{"X-Chain-Id": "1"}

// TestFailoverOnHTTPError: primary returns HTTP 500, secondary is healthy;
// a safe read method must fail over and return the secondary's result.
func TestFailoverOnHTTPError(t *testing.T) {
	primary := newFlakyUpstream(t, "primary")
	secondary := newFlakyUpstream(t, "secondary")
	primary.mode.Store(1) // http500
	gw := startFailoverGateway(t, failoverUpstreams(primary, secondary), nil)

	status, body := post(t, gw.URL, chainHeader, `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xabc","latest"],"id":1}`)
	if status != 200 {
		t.Fatalf("status: got %d, body %s", status, body)
	}
	if !bytes.Contains(body, []byte(`"result":"from-secondary"`)) {
		t.Errorf("expected result from secondary, got %s", body)
	}
	if primary.hits.Load() != 1 || secondary.hits.Load() != 1 {
		t.Errorf("hits: primary=%d secondary=%d, want 1/1", primary.hits.Load(), secondary.hits.Load())
	}
}

// TestFailoverOnInvalidResponse: primary returns garbage JSON, secondary is
// healthy; the safe read must fail over to the secondary.
func TestFailoverOnInvalidResponse(t *testing.T) {
	primary := newFlakyUpstream(t, "primary")
	secondary := newFlakyUpstream(t, "secondary")
	primary.mode.Store(2) // garbage
	gw := startFailoverGateway(t, failoverUpstreams(primary, secondary), nil)

	status, body := post(t, gw.URL, chainHeader, `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xabc","latest"],"id":1}`)
	if status != 200 {
		t.Fatalf("status: got %d, body %s", status, body)
	}
	if !bytes.Contains(body, []byte(`"result":"from-secondary"`)) {
		t.Errorf("expected result from secondary, got %s", body)
	}
	if primary.hits.Load() != 1 || secondary.hits.Load() != 1 {
		t.Errorf("hits: primary=%d secondary=%d, want 1/1", primary.hits.Load(), secondary.hits.Load())
	}
}

// TestFailoverOnTimeout: primary sleeps 2s, timeout is 1s total so each of
// the 2 attempts gets 500ms; the attempt timeout must fire and fail over to
// the secondary well before the primary would have replied.
func TestFailoverOnTimeout(t *testing.T) {
	primary := newFlakyUpstream(t, "primary")
	secondary := newFlakyUpstream(t, "secondary")
	primary.mode.Store(3) // slow2s
	gw := startFailoverGateway(t, failoverUpstreams(primary, secondary), map[string]int{"default": 1})

	start := time.Now()
	status, body := post(t, gw.URL, chainHeader, `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xabc","latest"],"id":1}`)
	elapsed := time.Since(start)
	if status != 200 {
		t.Fatalf("status: got %d, body %s", status, body)
	}
	if !bytes.Contains(body, []byte(`"result":"from-secondary"`)) {
		t.Errorf("expected result from secondary, got %s", body)
	}
	if primary.hits.Load() != 1 {
		t.Errorf("primary hits: got %d want 1", primary.hits.Load())
	}
	if secondary.hits.Load() != 1 {
		t.Errorf("secondary hits: got %d want 1", secondary.hits.Load())
	}
	if elapsed >= 1800*time.Millisecond {
		t.Errorf("failover took %v, want < 1800ms", elapsed)
	}
}

// TestWriteMethodExactlyOnceOnFailure: eth_sendRawTransaction is a write
// method — exactly one attempt on the primary, no failover to the secondary,
// and the failure surfaces as -32001.
func TestWriteMethodExactlyOnceOnFailure(t *testing.T) {
	primary := newFlakyUpstream(t, "primary")
	secondary := newFlakyUpstream(t, "secondary")
	primary.mode.Store(1) // http500
	gw := startFailoverGateway(t, failoverUpstreams(primary, secondary), nil)

	status, body := post(t, gw.URL, chainHeader, `{"jsonrpc":"2.0","method":"eth_sendRawTransaction","params":["0xdeadbeef"],"id":1}`)
	if status != 200 {
		t.Fatalf("status: got %d, body %s", status, body)
	}
	code, _, _ := decodeError(t, body)
	if code != jsonrpc.CodeUpstreamUnavailable {
		t.Errorf("code: got %d want -32001 (body %s)", code, body)
	}
	if primary.hits.Load() != 1 {
		t.Errorf("primary hits: got %d want exactly 1 (no retry for write methods)", primary.hits.Load())
	}
	if secondary.hits.Load() != 0 {
		t.Errorf("secondary hits: got %d want 0 (no failover for write methods)", secondary.hits.Load())
	}
}

// TestAllUpstreamsUnavailable: every candidate fails; the gateway must
// exhaust all attempts and return -32001 (Upstream unavailable).
func TestAllUpstreamsUnavailable(t *testing.T) {
	primary := newFlakyUpstream(t, "primary")
	secondary := newFlakyUpstream(t, "secondary")
	primary.mode.Store(1)   // http500
	secondary.mode.Store(1) // http500
	gw := startFailoverGateway(t, failoverUpstreams(primary, secondary), nil)

	status, body := post(t, gw.URL, chainHeader, `{"jsonrpc":"2.0","method":"eth_chainId","id":1}`)
	if status != 200 {
		t.Fatalf("status: got %d, body %s", status, body)
	}
	code, _, _ := decodeError(t, body)
	if code != jsonrpc.CodeUpstreamUnavailable {
		t.Errorf("code: got %d want -32001 (body %s)", code, body)
	}
	if primary.hits.Load() < 1 || secondary.hits.Load() < 1 {
		t.Errorf("hits: primary=%d secondary=%d, want both >= 1", primary.hits.Load(), secondary.hits.Load())
	}
}
