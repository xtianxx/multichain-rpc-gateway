package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rpcResp mirrors the JSON-RPC response envelope for assertions.
type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	ID json.RawMessage `json:"id"`
}

// newTestServer mounts the mock handler in-process without binding a fixed port.
func newTestServer(t *testing.T, chainID int) (*httptest.Server, *mockUpstream) {
	t.Helper()
	u := newMockUpstream(chainID)
	u.logger = log.New(io.Discard, "", 0) // keep test output clean
	srv := httptest.NewServer(newMockHandler(u))
	t.Cleanup(srv.Close)
	return srv, u
}

func post(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, out
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, out
}

func decodeResp(t *testing.T, body []byte) rpcResp {
	t.Helper()
	var r rpcResp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("response not valid JSON: %v (%s)", err, body)
	}
	return r
}

func TestEthChainID(t *testing.T) {
	cases := []struct {
		chainID int
		want    string
	}{
		{1, "0x1"},
		{8453, "0x2105"},
	}
	for _, tc := range cases {
		srv, _ := newTestServer(t, tc.chainID)
		status, body := post(t, srv.URL+"/", `{"jsonrpc":"2.0","method":"eth_chainId","id":1}`)
		if status != http.StatusOK {
			t.Fatalf("chain %d: status %d", tc.chainID, status)
		}
		r := decodeResp(t, body)
		if string(r.Result) != `"`+tc.want+`"` {
			t.Errorf("chain %d: result %s, want %q", tc.chainID, r.Result, tc.want)
		}
		if string(r.ID) != "1" {
			t.Errorf("chain %d: id %s, want 1", tc.chainID, r.ID)
		}
	}
}

func TestUnknownMethod(t *testing.T) {
	srv, _ := newTestServer(t, 1)
	status, body := post(t, srv.URL+"/", `{"jsonrpc":"2.0","method":"eth_noSuchMethod","id":7}`)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	r := decodeResp(t, body)
	if r.Error == nil || r.Error.Code != -32601 || r.Error.Message != "Method not found" {
		t.Errorf("error: got %+v, want -32601 Method not found", r.Error)
	}
	if len(r.Result) != 0 {
		t.Errorf("result must be absent on error, got %s", r.Result)
	}
	if string(r.ID) != "7" {
		t.Errorf("id %s, want 7", r.ID)
	}
}

func TestBatchOrderAndNotification(t *testing.T) {
	srv, _ := newTestServer(t, 8453)
	batch := `[
		{"jsonrpc":"2.0","method":"eth_chainId","id":1},
		{"jsonrpc":"2.0","method":"eth_chainId"},
		{"jsonrpc":"2.0","method":"eth_noSuchMethod","id":2},
		{"jsonrpc":"2.0","method":"eth_chainId","id":"c"}
	]`
	status, body := post(t, srv.URL+"/", batch)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	var resp []rpcResp
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("batch response not an array: %v (%s)", err, body)
	}
	// The notification (element 2) must produce no response element, so the
	// array has 3 entries in request order: id 1, id 2, id "c".
	if len(resp) != 3 {
		t.Fatalf("len %d, want 3 (notification must be omitted): %s", len(resp), body)
	}
	if string(resp[0].ID) != "1" || string(resp[0].Result) != `"0x2105"` {
		t.Errorf("resp[0]: id %s result %s", resp[0].ID, resp[0].Result)
	}
	if string(resp[1].ID) != "2" || resp[1].Error == nil || resp[1].Error.Code != -32601 {
		t.Errorf("resp[1]: id %s error %+v", resp[1].ID, resp[1].Error)
	}
	if string(resp[2].ID) != `"c"` || string(resp[2].Result) != `"0x2105"` {
		t.Errorf("resp[2]: id %s result %s", resp[2].ID, resp[2].Result)
	}
}

func TestAllNotificationBatchReturnsEmpty(t *testing.T) {
	srv, _ := newTestServer(t, 1)
	status, body := post(t, srv.URL+"/", `[
		{"jsonrpc":"2.0","method":"eth_chainId"},
		{"jsonrpc":"2.0","method":"eth_blockNumber"}
	]`)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if len(body) != 0 {
		t.Errorf("all-notification batch must return an empty body, got %s", body)
	}
}

func TestFault500(t *testing.T) {
	srv, _ := newTestServer(t, 1)
	status, _ := post(t, srv.URL+"/_ctl", `{"mode":"500"}`)
	if status != http.StatusOK {
		t.Fatalf("ctl status %d", status)
	}
	status, body := post(t, srv.URL+"/", `{"jsonrpc":"2.0","method":"eth_chainId","id":1}`)
	if status != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", status)
	}
	if len(body) != 0 {
		t.Errorf("body must be empty, got %s", body)
	}
}

func TestFaultGarbage(t *testing.T) {
	srv, _ := newTestServer(t, 1)
	post(t, srv.URL+"/_ctl", `{"mode":"garbage"}`)
	status, body := post(t, srv.URL+"/", `{"jsonrpc":"2.0","method":"eth_chainId","id":1}`)
	if status != http.StatusOK {
		t.Errorf("status %d, want 200", status)
	}
	if string(body) != "not json" {
		t.Errorf("body %q, want %q", body, "not json")
	}
}

func TestFaultTimeout(t *testing.T) {
	srv, _ := newTestServer(t, 1)
	post(t, srv.URL+"/_ctl", `{"mode":"timeout"}`)
	start := time.Now()
	status, body := post(t, srv.URL+"/", `{"jsonrpc":"2.0","method":"eth_chainId","id":1}`)
	elapsed := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if elapsed < timeoutDelay {
		t.Errorf("elapsed %v, want >= %v (timeout mode must sleep)", elapsed, timeoutDelay)
	}
	r := decodeResp(t, body)
	if string(r.Result) != `"0x1"` {
		t.Errorf("result %s, want \"0x1\"", r.Result)
	}
}

func TestCtlToggle(t *testing.T) {
	srv, _ := newTestServer(t, 1)

	status, body := get(t, srv.URL+"/_ctl")
	if status != http.StatusOK || string(body) != `{"mode":"ok"}` {
		t.Fatalf("initial mode: status %d body %s", status, body)
	}

	status, _ = post(t, srv.URL+"/_ctl", `{"mode":"garbage"}`)
	if status != http.StatusOK {
		t.Fatalf("set mode status %d", status)
	}
	_, body = get(t, srv.URL+"/_ctl")
	if string(body) != `{"mode":"garbage"}` {
		t.Errorf("mode after toggle: %s, want garbage", body)
	}

	// Unknown mode must be rejected and leave the current mode unchanged.
	status, _ = post(t, srv.URL+"/_ctl", `{"mode":"nope"}`)
	if status != http.StatusBadRequest {
		t.Errorf("invalid mode status %d, want 400", status)
	}
	_, body = get(t, srv.URL+"/_ctl")
	if string(body) != `{"mode":"garbage"}` {
		t.Errorf("mode after invalid toggle: %s, want garbage", body)
	}

	post(t, srv.URL+"/_ctl", `{"mode":"ok"}`)
	_, body = get(t, srv.URL+"/_ctl")
	if string(body) != `{"mode":"ok"}` {
		t.Errorf("mode after reset: %s, want ok", body)
	}
}

func TestHealth(t *testing.T) {
	srv, _ := newTestServer(t, 1)
	status, body := get(t, srv.URL+"/_health")
	if status != http.StatusOK || string(body) != "ok" {
		t.Errorf("health: status %d body %q", status, body)
	}
}

func TestSendTransactionHashFormat(t *testing.T) {
	srv, _ := newTestServer(t, 1)
	status, body := post(t, srv.URL+"/", `{"jsonrpc":"2.0","method":"eth_sendTransaction","params":[{}],"id":1}`)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	r := decodeResp(t, body)
	hash := strings.TrimPrefix(string(r.Result), `"`)
	hash = strings.TrimSuffix(hash, `"`)
	if !strings.HasPrefix(hash, "0x") || len(hash) != 66 {
		t.Errorf("tx hash %q, want 0x + 64 hex chars", hash)
	}
}
