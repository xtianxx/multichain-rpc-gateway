// Package integration exercises the gateway end-to-end against in-process
// mock upstreams (httptest), per tasks.md T018 (US1).
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtianxx/multichain-rpc-gateway/internal/api"
	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
	"github.com/xtianxx/multichain-rpc-gateway/internal/router"
)

// mockUpstream serves a JSON-RPC upstream whose eth_chainId result is marker,
// with special methods for fault injection.
type mockUpstream struct {
	server *httptest.Server
	hits   atomic.Int32
	marker string // chainId result, e.g. "0x1" / "0x2105"
}

func newMockUpstream(marker string) *mockUpstream {
	m := &mockUpstream{marker: marker}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		var env map[string]json.RawMessage
		if err := json.Unmarshal(body, &env); err != nil {
			w.WriteHeader(200)
			w.Write([]byte(`{"garbage":true}`))
			return
		}
		var method string
		_ = json.Unmarshal(env["method"], &method)
		id := env["id"]

		writeResp := func(result string) {
			if len(id) == 0 {
				w.WriteHeader(200)
				return
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","result":%s,"id":%s}`, result, id)
		}

		switch method {
		case "eth_chainId":
			writeResp(strconv.Quote(m.marker))
		case "eth_slow":
			time.Sleep(2 * time.Second)
			writeResp(strconv.Quote("slow"))
		case "eth_bad":
			w.WriteHeader(200)
			w.Write([]byte(`{"foo":1}`))
		case "eth_error":
			if len(id) > 0 {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not found"},"id":%s}`, id)
			} else {
				w.WriteHeader(200)
			}
		default:
			writeResp(strconv.Quote("from-" + m.marker))
		}
	}))
	return m
}

func (m *mockUpstream) close() { m.server.Close() }

type gateway struct {
	server *httptest.Server
}

func startGateway(t *testing.T, ethURL, baseURL string, timeouts map[string]int, maxBodyBytes int64, maxBatchElements int) *gateway {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{Timeouts: timeouts, MaxBodyBytes: maxBodyBytes},
		Chains: []config.Chain{
			{ChainID: "1", Adapter: "ethereum", Upstreams: []config.Upstream{{Name: "eth-a", URL: ethURL}}},
			{ChainID: "8453", Adapter: "base", Upstreams: []config.Upstream{{Name: "base-a", URL: baseURL}}},
		},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	rt, err := router.New(cfg, logger)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	if maxBodyBytes == 0 {
		maxBodyBytes = 1048576
	}
	if maxBatchElements == 0 {
		maxBatchElements = 100
	}
	h := api.New(rt, maxBodyBytes, maxBatchElements, logger)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &gateway{server: srv}
}

func post(t *testing.T, url string, headers map[string]string, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// decodeError asserts the body is a JSON-RPC error and returns its fields.
func decodeError(t *testing.T, body []byte) (int, string, map[string]any) {
	t.Helper()
	var env struct {
		Error struct {
			Code    int            `json:"code"`
			Message string         `json:"message"`
			Data    map[string]any `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response not valid JSON: %v (%s)", err, body)
	}
	return env.Error.Code, env.Error.Message, env.Error.Data
}

func TestRoutingByChainHeader(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `{"jsonrpc":"2.0","method":"eth_chainId","id":1}`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	if !bytes.Contains(body, []byte(`"result":"0x1"`)) {
		t.Errorf("ethereum result missing: %s", body)
	}

	status, body = post(t, gw.server.URL, map[string]string{"X-Chain-Id": "8453"}, `{"jsonrpc":"2.0","method":"eth_chainId","id":2}`)
	if status != 200 || !bytes.Contains(body, []byte(`"result":"0x2105"`)) {
		t.Errorf("base result missing: status %d body %s", status, body)
	}

	if eth.hits.Load() != 1 || base.hits.Load() != 1 {
		t.Errorf("hits: eth=%d base=%d, want 1/1", eth.hits.Load(), base.hits.Load())
	}
}

func TestUnknownChainNotForwarded(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "999"}, `{"jsonrpc":"2.0","method":"eth_chainId","id":1}`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	code, _, data := decodeError(t, body)
	if code != jsonrpc.CodeChainNotConfigured {
		t.Errorf("code: got %d want -32000", code)
	}
	if data["chain_id"] != "999" {
		t.Errorf("data.chain_id: got %v", data["chain_id"])
	}
	if eth.hits.Load() != 0 || base.hits.Load() != 0 {
		t.Error("unknown chain must not be forwarded upstream")
	}
}

func TestMissingAndInvalidChainHeader(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, nil, `{"jsonrpc":"2.0","method":"eth_chainId","id":1}`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	if code, _, _ := decodeError(t, body); code != jsonrpc.CodeChainNotConfigured {
		t.Errorf("missing header: got code %d want -32000", code)
	}

	status, body = post(t, gw.server.URL, map[string]string{"X-Chain-Id": "0x2105"}, `{"jsonrpc":"2.0","method":"eth_chainId","id":1}`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	code, _, data := decodeError(t, body)
	if code != jsonrpc.CodeChainNotConfigured {
		t.Errorf("invalid header: got code %d want -32000", code)
	}
	if data["chain_id"] != "0x2105" {
		t.Errorf("data.chain_id: got %v", data["chain_id"])
	}
}

func TestEthSubscribeRejectedWithoutForwarding(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `{"jsonrpc":"2.0","method":"eth_subscribe","params":["newHeads"],"id":5}`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	code, _, _ := decodeError(t, body)
	if code != jsonrpc.CodeMethodNotFound {
		t.Errorf("code: got %d want -32601", code)
	}
	if eth.hits.Load() != 0 {
		t.Error("eth_subscribe must be rejected gateway-side without forwarding")
	}
}

func TestIDByteExactEcho(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	for _, id := range []string{`"abc-1"`, `1`, `"1"`, `null`, `-7`} {
		status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `{"jsonrpc":"2.0","method":"eth_chainId","id":`+id+`}`)
		if status != 200 {
			t.Fatalf("id %s: status %d", id, status)
		}
		if !bytes.Contains(body, []byte(`"id":`+id)) {
			t.Errorf("id %s not echoed byte-for-byte: %s", id, body)
		}
	}
}

func TestNotificationProducesNoResponse(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `{"jsonrpc":"2.0","method":"eth_chainId"}`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	if len(body) != 0 {
		t.Errorf("notification must produce an empty body, got %s", body)
	}
	if eth.hits.Load() != 1 {
		t.Errorf("notification must still be forwarded upstream: hits %d", eth.hits.Load())
	}
}

func TestInvalidJSONReturns400ParseError(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `{`)
	if status != 400 {
		t.Fatalf("status: got %d want 400", status)
	}
	code, _, _ := decodeError(t, body)
	if code != jsonrpc.CodeParseError {
		t.Errorf("code: got %d want -32700", code)
	}
}

func TestBodyTooLargeReturns400(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 64, 100)

	big := `{"jsonrpc":"2.0","method":"eth_getBalance","params":["` + string(bytes.Repeat([]byte("a"), 200)) + `"],"id":1}`
	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, big)
	if status != 400 {
		t.Fatalf("status: got %d want 400", status)
	}
	code, _, _ := decodeError(t, body)
	if code != jsonrpc.CodeBodyTooLarge {
		t.Errorf("code: got %d want -32004", code)
	}
}

func TestInvalidUpstreamResponse(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `{"jsonrpc":"2.0","method":"eth_bad","id":1}`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	code, _, _ := decodeError(t, body)
	if code != jsonrpc.CodeInvalidUpstreamResponse {
		t.Errorf("code: got %d want -32002", code)
	}
}

func TestUpstreamErrorPassthrough(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `{"jsonrpc":"2.0","method":"eth_error","id":1}`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	code, msg, _ := decodeError(t, body)
	if code != -32601 || msg != "Method not found" {
		t.Errorf("upstream error must pass through: code %d msg %q", code, msg)
	}
}

func TestUpstreamTimeout(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, map[string]int{"default": 1}, 0, 100)

	start := time.Now()
	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `{"jsonrpc":"2.0","method":"eth_slow","id":1}`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	code, _, _ := decodeError(t, body)
	if code != jsonrpc.CodeUpstreamTimeout {
		t.Errorf("code: got %d want -32005", code)
	}
	if elapsed := time.Since(start); elapsed > 1900*time.Millisecond {
		t.Errorf("gateway must return at its own deadline, took %v", elapsed)
	}
}

func TestUnreachableUpstream(t *testing.T) {
	base := newMockUpstream("0x2105")
	defer base.close()
	// eth upstream points at a closed port.
	gw := startGateway(t, "http://127.0.0.1:1", base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `{"jsonrpc":"2.0","method":"eth_chainId","id":1}`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	code, _, _ := decodeError(t, body)
	if code != jsonrpc.CodeUpstreamUnavailable {
		t.Errorf("code: got %d want -32001 (body %s)", code, body)
	}
}
