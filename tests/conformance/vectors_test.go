// Package conformance holds the JSON-RPC 2.0 conformance vector suite
// (T023). Every top-level test is prefixed TestConformance because the
// Makefile gate runs `go test -run TestConformance ./tests/conformance/`.
package conformance

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
)

// TestConformanceID verifies byte-for-byte id echo through
// ParseSingle + Response.Marshal for the full vector set, including
// 9007199254740993 which exceeds float64 precision.
func TestConformanceID(t *testing.T) {
	ids := []string{`1`, `"abc-1"`, `null`, `-7`, `0`, `9007199254740993`}
	for _, id := range ids {
		body := `{"jsonrpc":"2.0","method":"eth_chainId","id":` + id + `}`
		req, jrErr := jsonrpc.ParseSingle([]byte(body))
		if jrErr != nil {
			t.Fatalf("ParseSingle(%s): %v", body, jrErr)
		}
		if string(req.ID) != id {
			t.Errorf("parsed id: got %q want %q", req.ID, id)
		}
		out, err := jsonrpc.NewResultResponse(req.ID, json.RawMessage(`"0x1"`)).Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !bytes.Contains(out, []byte(`"id":`+id)) {
			t.Errorf("marshaled response %s does not echo id %s byte-for-byte", out, id)
		}
	}
}

// TestConformanceResultErrorExclusivity covers the response-shape contract:
// a response carries exactly one of result or error, and upstream bodies are
// accepted only when they obey the same rule plus id matching.
func TestConformanceResultErrorExclusivity(t *testing.T) {
	// Result response: result member present, error member absent.
	out, err := jsonrpc.NewResultResponse(json.RawMessage(`1`), json.RawMessage(`"0x1"`)).Marshal()
	if err != nil {
		t.Fatalf("Marshal result: %v", err)
	}
	if !bytes.Contains(out, []byte(`"result"`)) || bytes.Contains(out, []byte(`"error"`)) {
		t.Errorf("result response must contain result and no error: %s", out)
	}

	// Error response: error member present, result member absent.
	out, err = jsonrpc.NewErrorResponse(json.RawMessage(`1`), jsonrpc.CodeMethodNotFound, nil).Marshal()
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if !bytes.Contains(out, []byte(`"error"`)) || bytes.Contains(out, []byte(`"result"`)) {
		t.Errorf("error response must contain error and no result: %s", out)
	}

	// ValidUpstreamResponse acceptance table.
	validResult := []byte(`{"jsonrpc":"2.0","result":"0x1","id":1}`)
	validError := []byte(`{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not found"},"id":1}`)
	cases := []struct {
		name   string
		body   []byte
		sentID json.RawMessage
		want   bool
	}{
		{"result-only", validResult, json.RawMessage(`1`), true},
		{"error-object-only", validError, json.RawMessage(`1`), true},
		{"both-result-and-error", []byte(`{"jsonrpc":"2.0","result":1,"error":{"code":1,"message":"x"},"id":1}`), json.RawMessage(`1`), false},
		{"neither", []byte(`{"jsonrpc":"2.0","id":1}`), json.RawMessage(`1`), false},
		{"id-mismatch", validResult, json.RawMessage(`2`), false},
		{"garbage-object", []byte(`{"garbage":true}`), json.RawMessage(`1`), false},
		{"trailing-content", []byte(`{"jsonrpc":"2.0","result":1,"id":1} trailing`), json.RawMessage(`1`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsonrpc.ValidUpstreamResponse(tc.body, tc.sentID); got != tc.want {
				t.Errorf("ValidUpstreamResponse(%s): got %v want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestConformanceErrorCodeTable pins every locked error code to its exact
// stable English message, for both the standard JSON-RPC range and the
// gateway range; unknown codes map to the empty string.
func TestConformanceErrorCodeTable(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{jsonrpc.CodeParseError, "Parse error"},
		{jsonrpc.CodeInvalidRequest, "Invalid Request"},
		{jsonrpc.CodeMethodNotFound, "Method not found"},
		{jsonrpc.CodeInvalidParams, "Invalid params"},
		{jsonrpc.CodeInternalError, "Internal error"},
		{jsonrpc.CodeChainNotConfigured, "Chain not configured"},
		{jsonrpc.CodeUpstreamUnavailable, "Upstream unavailable"},
		{jsonrpc.CodeInvalidUpstreamResponse, "Invalid upstream response"},
		{jsonrpc.CodeBatchTooLarge, "Batch too large"},
		{jsonrpc.CodeBodyTooLarge, "Request body too large"},
		{jsonrpc.CodeUpstreamTimeout, "Upstream timeout"},
	}
	for _, tc := range cases {
		if got := jsonrpc.CodeMessage(tc.code); got != tc.want {
			t.Errorf("CodeMessage(%d): got %q want %q", tc.code, got, tc.want)
		}
	}
	if got := jsonrpc.CodeMessage(-32099); got != "" {
		t.Errorf("CodeMessage(-32099): got %q want empty for unknown code", got)
	}
}

// TestConformanceInvalidParamsIDEcho locks the T052 contract: a
// gateway-generated -32602 on a request with a determinable id echoes the
// raw id byte-for-byte (numeric 42 stays 42), and an invalid-params
// notification is reported with the id absent so no error element is
// produced.
func TestConformanceInvalidParamsIDEcho(t *testing.T) {
	req, jrErr := jsonrpc.ParseSingle([]byte(`{"jsonrpc":"2.0","method":"m","params":5,"id":42}`))
	if jrErr == nil || jrErr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("ParseSingle: want -32602, got %v", jrErr)
	}
	if req == nil || string(req.ID) != "42" {
		t.Fatalf("invalid-params request must carry raw id 42, got %+v", req)
	}
	out, err := jsonrpc.NewErrorResponse(req.ID, jrErr.Code, jrErr.Data).Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"jsonrpc":"2.0","error":{"code":-32602,"message":"Invalid params"},"id":42}`
	if string(out) != want {
		t.Errorf("error response:\n got %s\nwant %s", out, want)
	}

	// A notification with invalid params carries no id: callers must not
	// produce a response element.
	req, jrErr = jsonrpc.ParseSingle([]byte(`{"jsonrpc":"2.0","method":"m","params":5}`))
	if jrErr == nil || jrErr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("ParseSingle: want -32602, got %v", jrErr)
	}
	if req == nil || req.ID != nil || !jsonrpc.IsNotification(req) {
		t.Fatalf("invalid-params notification must be reported as notification, got %+v", req)
	}
}

// TestConformanceBatchOrdering locks the US2 batch contract: ParseBatch
// yields elements in request order (notification in the middle included),
// and MarshalBatch of the corresponding responses preserves order while
// skipping the notification slot.
func TestConformanceBatchOrdering(t *testing.T) {
	body := `[
		{"jsonrpc":"2.0","method":"eth_chainId","id":1},
		{"jsonrpc":"2.0","method":"eth_subscribe","params":["newHeads"]},
		{"jsonrpc":"2.0","method":"eth_blockNumber","id":"two"}
	]`
	elements, jrErr := jsonrpc.ParseBatch([]byte(body))
	if jrErr != nil {
		t.Fatalf("ParseBatch: unexpected body-level error: %v", jrErr)
	}
	if len(elements) != 3 {
		t.Fatalf("element count: got %d want 3", len(elements))
	}
	if elements[0].Request == nil || elements[0].Request.Method != "eth_chainId" {
		t.Errorf("elements[0]: want eth_chainId, got %+v", elements[0].Request)
	}
	if elements[1].Request == nil || elements[1].Request.Method != "eth_subscribe" || !jsonrpc.IsNotification(elements[1].Request) {
		t.Errorf("elements[1]: want eth_subscribe notification, got %+v", elements[1].Request)
	}
	if elements[2].Request == nil || elements[2].Request.Method != "eth_blockNumber" {
		t.Errorf("elements[2]: want eth_blockNumber, got %+v", elements[2].Request)
	}

	// Respond to elements 0 and 2 only; the notification slot is skipped.
	responses := []*jsonrpc.Response{
		jsonrpc.NewResultResponse(elements[0].Request.ID, json.RawMessage(`"0x1"`)),
		jsonrpc.NewResultResponse(elements[2].Request.ID, json.RawMessage(`"0x10"`)),
	}
	out, err := jsonrpc.MarshalBatch(responses)
	if err != nil {
		t.Fatalf("MarshalBatch: %v", err)
	}
	want := `[{"jsonrpc":"2.0","result":"0x1","id":1},{"jsonrpc":"2.0","result":"0x10","id":"two"}]`
	if string(out) != want {
		t.Errorf("MarshalBatch:\n got %s\nwant %s", out, want)
	}
}

// TestConformanceNotification covers the notification contract: elements
// without an id member are notifications, a notification-only batch parses
// entirely as notifications, and the all-notification response set is empty
// so MarshalBatch returns nil (the caller then returns an empty body).
func TestConformanceNotification(t *testing.T) {
	body := `[
		{"jsonrpc":"2.0","method":"eth_subscribe","params":["newHeads"]},
		{"jsonrpc":"2.0","method":"net_version"}
	]`
	elements, jrErr := jsonrpc.ParseBatch([]byte(body))
	if jrErr != nil {
		t.Fatalf("ParseBatch: unexpected error: %v", jrErr)
	}
	if len(elements) != 2 {
		t.Fatalf("element count: got %d want 2", len(elements))
	}
	for idx := range elements {
		if elements[idx].Err != nil || elements[idx].Request == nil {
			t.Fatalf("elements[%d]: want valid request, got %+v", idx, elements[idx])
		}
		if !jsonrpc.IsNotification(elements[idx].Request) {
			t.Errorf("elements[%d]: want notification, got %+v", idx, elements[idx].Request)
		}
	}

	// All notifications -> empty response list -> MarshalBatch returns nil.
	out, err := jsonrpc.MarshalBatch(nil)
	if out != nil || err != nil {
		t.Errorf("MarshalBatch(nil): got %q, %v want nil, nil", out, err)
	}
}
