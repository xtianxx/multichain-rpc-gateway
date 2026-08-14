// Batch orchestration end-to-end tests (T024/T027/T028): batches are
// parsed, element-count limited, routed per element (X-Chain-Id header or
// per-element x-chain-id override), and answered in request order with
// byte-exact id echo. Mock upstreams: eth marker 0x1 (chain 1), base marker
// 0x2105 (chain 8453).
package integration

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
)

// decodeBatch decodes body as a JSON array of response objects. Numbers are
// decoded with UseNumber so large ids survive without precision loss.
func decodeBatch(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var out []map[string]any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("batch body is not a JSON array: %v (%s)", err, body)
	}
	return out
}

// errorCode extracts the code of element's error member (0 when absent).
func errorCode(el map[string]any) int {
	errObj, ok := el["error"].(map[string]any)
	if !ok {
		return 0
	}
	n, ok := errObj["code"].(json.Number)
	if !ok {
		return 0
	}
	code, _ := n.Int64()
	return int(code)
}

// errorData extracts the data member of element's error object (nil when
// absent).
func errorData(el map[string]any) map[string]any {
	errObj, ok := el["error"].(map[string]any)
	if !ok {
		return nil
	}
	data, _ := errObj["data"].(map[string]any)
	return data
}

// batchIDs returns the id members of a batch response in order, as raw
// bytes (byte-exact, so numbers and strings keep their original form).
func batchIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var out []struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode batch ids: %v (%s)", err, body)
	}
	ids := make([]string, 0, len(out))
	for _, el := range out {
		ids = append(ids, string(el.ID))
	}
	return ids
}

// assertBatchIDs checks the raw id bytes of a batch response, in order.
func assertBatchIDs(t *testing.T, body []byte, want ...string) {
	t.Helper()
	got := batchIDs(t, body)
	if len(got) != len(want) {
		t.Fatalf("ids: got %v want %v (body %s)", got, want, body)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids: got %v want %v", got, want)
		}
	}
}

func TestBatchMixedChainsOrdered(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_chainId","id":1},
		{"jsonrpc":"2.0","method":"eth_chainId","x-chain-id":"8453","id":2},
		{"jsonrpc":"2.0","method":"eth_chainId","id":3}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	els := decodeBatch(t, body)
	if len(els) != 3 {
		t.Fatalf("response count: got %d want 3 (%s)", len(els), body)
	}
	assertBatchIDs(t, body, "1", "2", "3")
	wantResults := []string{"0x1", "0x2105", "0x1"}
	for i, want := range wantResults {
		if got, _ := els[i]["result"].(string); got != want {
			t.Errorf("element %d result: got %q want %q (body %s)", i, got, want, body)
		}
	}
	for _, id := range []string{`1`, `2`, `3`} {
		if !bytes.Contains(body, []byte(`"id":`+id)) {
			t.Errorf("id %s not echoed byte-for-byte: %s", id, body)
		}
	}
	if eth.hits.Load() != 2 || base.hits.Load() != 1 {
		t.Errorf("hits: eth=%d base=%d, want 2/1", eth.hits.Load(), base.hits.Load())
	}
}

func TestBatchOverrideBeatsHeader(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_chainId","x-chain-id":8453,"id":1},
		{"jsonrpc":"2.0","method":"eth_chainId","x-chain-id":"1","id":2}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	els := decodeBatch(t, body)
	if len(els) != 2 {
		t.Fatalf("response count: got %d want 2 (%s)", len(els), body)
	}
	if got, _ := els[0]["result"].(string); got != "0x2105" {
		t.Errorf("element 0 (numeric override 8453): got %q want base result (body %s)", got, body)
	}
	if got, _ := els[1]["result"].(string); got != "0x1" {
		t.Errorf("element 1 (string override 1): got %q want eth result (body %s)", got, body)
	}
	if eth.hits.Load() != 1 || base.hits.Load() != 1 {
		t.Errorf("hits: eth=%d base=%d, want 1/1", eth.hits.Load(), base.hits.Load())
	}
}

func TestBatchNotificationNoResponseSlot(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_chainId","id":1},
		{"jsonrpc":"2.0","method":"eth_chainId"},
		{"jsonrpc":"2.0","method":"eth_chainId","x-chain-id":"8453","id":2}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	els := decodeBatch(t, body)
	if len(els) != 2 {
		t.Fatalf("response count: got %d want 2 (body %s)", len(els), body)
	}
	assertBatchIDs(t, body, "1", "2")
	if got, _ := els[0]["result"].(string); got != "0x1" {
		t.Errorf("element 0 result: got %q want eth (body %s)", got, body)
	}
	if got, _ := els[1]["result"].(string); got != "0x2105" {
		t.Errorf("element 1 result: got %q want base (body %s)", got, body)
	}
	if eth.hits.Load() != 2 || base.hits.Load() != 1 {
		t.Errorf("hits: eth=%d base=%d, want 2/1 (notification forwarded)", eth.hits.Load(), base.hits.Load())
	}
}

func TestBatchAllNotificationsEmptyBody(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_chainId"},
		{"jsonrpc":"2.0","method":"eth_chainId"}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	if len(body) != 0 {
		t.Errorf("all-notification batch must produce an empty body, got %q", body)
	}
	if eth.hits.Load() != 2 {
		t.Errorf("notifications must be forwarded: hits %d want 2", eth.hits.Load())
	}
}

func TestBatchEmptyArrayInvalidRequest(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[]`)
	if status != 200 {
		t.Fatalf("status: got %d want 200", status)
	}
	if code, _, _ := decodeError(t, body); code != jsonrpc.CodeInvalidRequest {
		t.Errorf("code: got %d want -32600 (body %s)", code, body)
	}
}

func TestBatchElementChainErrorsIsolated(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_chainId","id":1},
		{"jsonrpc":"2.0","method":"eth_chainId","x-chain-id":"999","id":2}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	els := decodeBatch(t, body)
	if len(els) != 2 {
		t.Fatalf("response count: got %d want 2 (%s)", len(els), body)
	}
	if got, _ := els[0]["result"].(string); got != "0x1" {
		t.Errorf("sibling of failed element must still succeed: got %q (body %s)", got, body)
	}
	if code := errorCode(els[1]); code != jsonrpc.CodeChainNotConfigured {
		t.Errorf("element 1 code: got %d want -32000", code)
	}
	if data := errorData(els[1]); data["chain_id"] != "999" {
		t.Errorf("element 1 data.chain_id: got %v want 999", data["chain_id"])
	}
	assertBatchIDs(t, body, "1", "2")
	if eth.hits.Load() != 1 || base.hits.Load() != 0 {
		t.Errorf("unknown chain must not be forwarded: hits eth=%d base=%d", eth.hits.Load(), base.hits.Load())
	}
}

func TestBatchElementNoChainAnywhere(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, nil, `[
		{"jsonrpc":"2.0","method":"eth_chainId","id":1},
		{"jsonrpc":"2.0","method":"eth_chainId","id":2}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	els := decodeBatch(t, body)
	if len(els) != 2 {
		t.Fatalf("response count: got %d want 2 (%s)", len(els), body)
	}
	for i, el := range els {
		if code := errorCode(el); code != jsonrpc.CodeChainNotConfigured {
			t.Errorf("element %d code: got %d want -32000", i, code)
		}
	}
	if eth.hits.Load() != 0 || base.hits.Load() != 0 {
		t.Errorf("no chain anywhere: nothing may be forwarded, hits eth=%d base=%d", eth.hits.Load(), base.hits.Load())
	}
}

func TestBatchInvalidElementDoesNotBlock(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_chainId","id":1},
		1,
		{"jsonrpc":"2.0"}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	els := decodeBatch(t, body)
	if len(els) != 3 {
		t.Fatalf("response count: got %d want 3 (%s)", len(els), body)
	}
	if got, _ := els[0]["result"].(string); got != "0x1" {
		t.Errorf("element 0 must succeed: got %q (body %s)", got, body)
	}
	for i := 1; i <= 2; i++ {
		if code := errorCode(els[i]); code != jsonrpc.CodeInvalidRequest {
			t.Errorf("element %d code: got %d want -32600", i, code)
		}
		if _, ok := els[i]["id"]; !ok {
			t.Errorf("element %d error must carry id null", i)
		}
	}
	assertBatchIDs(t, body, "1", "null", "null")
	if eth.hits.Load() != 1 {
		t.Errorf("only the valid element may be forwarded: hits %d want 1", eth.hits.Load())
	}
}

func TestBatchSubscribeElementRejected(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_chainId","id":1},
		{"jsonrpc":"2.0","method":"eth_subscribe","params":["newHeads"],"id":2}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	els := decodeBatch(t, body)
	if len(els) != 2 {
		t.Fatalf("response count: got %d want 2 (%s)", len(els), body)
	}
	if got, _ := els[0]["result"].(string); got != "0x1" {
		t.Errorf("element 0 must succeed: got %q (body %s)", got, body)
	}
	if code := errorCode(els[1]); code != jsonrpc.CodeMethodNotFound {
		t.Errorf("subscribe element code: got %d want -32601", code)
	}
	if data := errorData(els[1]); data["method"] != "eth_subscribe" {
		t.Errorf("subscribe element data.method: got %v want eth_subscribe", data["method"])
	}
	if eth.hits.Load() != 1 {
		t.Errorf("eth_subscribe must not be forwarded: hits %d want 1", eth.hits.Load())
	}
}

func TestBatchSubscribeNotificationNotForwarded(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_subscribe","params":["newHeads"]},
		{"jsonrpc":"2.0","method":"eth_chainId","id":1}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	els := decodeBatch(t, body)
	if len(els) != 1 {
		t.Fatalf("subscribe notification must produce no element: got %d want 1 (%s)", len(els), body)
	}
	if got, _ := els[0]["result"].(string); got != "0x1" {
		t.Errorf("element 0 must succeed: got %q (body %s)", got, body)
	}
	if eth.hits.Load() != 1 {
		t.Errorf("subscribe notification must not be forwarded: hits %d want 1", eth.hits.Load())
	}
}

func TestBatchUpstreamErrorIsolated(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_error","id":1},
		{"jsonrpc":"2.0","method":"eth_chainId","id":2}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	els := decodeBatch(t, body)
	if len(els) != 2 {
		t.Fatalf("response count: got %d want 2 (%s)", len(els), body)
	}
	if code := errorCode(els[0]); code != -32601 {
		t.Errorf("upstream error must pass through: element 0 code %d want -32601", code)
	}
	if got, _ := els[1]["result"].(string); got != "0x1" {
		t.Errorf("element 1 must succeed after upstream error: got %q (body %s)", got, body)
	}
	if eth.hits.Load() != 2 {
		t.Errorf("hits: %d want 2", eth.hits.Load())
	}
}

func TestBatchTooManyElements(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 2)

	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_chainId","id":1},
		{"jsonrpc":"2.0","method":"eth_chainId","id":2},
		{"jsonrpc":"2.0","method":"eth_chainId","id":3}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	if code, _, _ := decodeError(t, body); code != jsonrpc.CodeBatchTooLarge {
		t.Errorf("code: got %d want -32003 (body %s)", code, body)
	}
	if eth.hits.Load() != 0 || base.hits.Load() != 0 {
		t.Errorf("oversized batch must not be forwarded: hits eth=%d base=%d", eth.hits.Load(), base.hits.Load())
	}
}

func TestBatchMalformedBody400(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	for _, body := range []string{`[`, `[{}]x`} {
		status, out := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, body)
		if status != 400 {
			t.Errorf("body %q: status got %d want 400", body, status)
		}
		if code, _, _ := decodeError(t, out); code != jsonrpc.CodeParseError {
			t.Errorf("body %q: code got %d want -32700", body, code)
		}
		if !bytes.Contains(out, []byte(`"id":null`)) {
			t.Errorf("body %q: parse error must carry id null: %s", body, out)
		}
	}
	if eth.hits.Load() != 0 || base.hits.Load() != 0 {
		t.Errorf("malformed batch must not be forwarded: hits eth=%d base=%d", eth.hits.Load(), base.hits.Load())
	}
}

func TestBatchIDByteEcho(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	ids := []string{`1`, `"abc-1"`, `-7`, `9007199254740993`}
	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_chainId","id":`+ids[0]+`},
		{"jsonrpc":"2.0","method":"eth_chainId","id":`+ids[1]+`},
		{"jsonrpc":"2.0","method":"eth_chainId","id":`+ids[2]+`},
		{"jsonrpc":"2.0","method":"eth_chainId","id":`+ids[3]+`}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	assertBatchIDs(t, body, ids...)
	for _, id := range ids {
		if !bytes.Contains(body, []byte(`"id":`+id)) {
			t.Errorf("id %s not echoed byte-for-byte: %s", id, body)
		}
	}
}

func TestBatchInvalidNotificationNotForwarded(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	// A no-id element with envelope-invalid params is rejected at parse
	// time (never forwarded) and produces NO response element: the batch
	// array only carries the id-bearing sibling.
	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_chainId","params":1},
		{"jsonrpc":"2.0","method":"eth_chainId","id":1}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	els := decodeBatch(t, body)
	if len(els) != 1 {
		t.Fatalf("response count: got %d want 1 (invalid notification must produce no element; body %s)", len(els), body)
	}
	assertBatchIDs(t, body, "1")
	if got, _ := els[0]["result"].(string); got != "0x1" {
		t.Errorf("valid sibling must succeed: got %q (body %s)", got, body)
	}
	if eth.hits.Load() != 1 {
		t.Errorf("invalid notification must not be forwarded: hits %d want 1", eth.hits.Load())
	}
}

func TestBatchInvalidParamsEchoesID(t *testing.T) {
	eth := newMockUpstream("0x1")
	defer eth.close()
	base := newMockUpstream("0x2105")
	defer base.close()
	gw := startGateway(t, eth.server.URL, base.server.URL, nil, 0, 100)

	// A params-invalid element WITH an id member is answered with -32602
	// echoing the raw id byte-for-byte (never re-marshaled: numeric 42
	// stays 42, string "42" stays "42"). An explicit "id":null is a
	// request and gets a response element with id null.
	status, body := post(t, gw.server.URL, map[string]string{"X-Chain-Id": "1"}, `[
		{"jsonrpc":"2.0","method":"eth_chainId","params":1,"id":42},
		{"jsonrpc":"2.0","method":"eth_chainId","params":1,"id":"42"},
		{"jsonrpc":"2.0","method":"eth_chainId","params":1,"id":null}
	]`)
	if status != 200 {
		t.Fatalf("status: got %d", status)
	}
	els := decodeBatch(t, body)
	if len(els) != 3 {
		t.Fatalf("response count: got %d want 3 (%s)", len(els), body)
	}
	for i := range els {
		if code := errorCode(els[i]); code != jsonrpc.CodeInvalidParams {
			t.Errorf("element %d code: got %d want -32602 (body %s)", i, code, body)
		}
	}
	assertBatchIDs(t, body, "42", `"42"`, "null")
	if eth.hits.Load() != 0 || base.hits.Load() != 0 {
		t.Errorf("invalid-params elements must not be forwarded: hits eth=%d base=%d", eth.hits.Load(), base.hits.Load())
	}
}
