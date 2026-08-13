package jsonrpc

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestParseBatchBodyErrors covers body-level failures: the body does not
// decode as JSON at all, or valid JSON is followed by trailing content. Both
// must yield (nil, -32700).
func TestParseBatchBodyErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty-body", ``},
		{"whitespace-only", `   `},
		{"unclosed-bracket", `[`},
		{"invalid-element-json", `[{"jsonrpc":`},
		{"unclosed-element-object", `[{"jsonrpc":"2.0"`},
		{"truncated-number", `[1,`},
		{"trailing-garbage", `[{"jsonrpc":"2.0","method":"m","id":1}] x`},
		{"trailing-second-value", `[{"jsonrpc":"2.0","method":"m","id":1}] [2]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			elements, jrErr := ParseBatch([]byte(tc.body))
			if jrErr == nil {
				t.Fatalf("ParseBatch(%q): expected -32700, got %+v", tc.body, elements)
			}
			if jrErr.Code != CodeParseError {
				t.Errorf("code: got %d want %d (body %q)", jrErr.Code, CodeParseError, tc.body)
			}
			if jrErr.Message != CodeMessage(CodeParseError) {
				t.Errorf("message: got %q want %q", jrErr.Message, CodeMessage(CodeParseError))
			}
			if elements != nil {
				t.Errorf("elements must be nil on body-level error, got %+v", elements)
			}
		})
	}
}

// TestParseBatchTopLevelRejects covers bodies that are valid JSON but not an
// array: object, scalars, and the empty array (empty-batch invalid-request
// case). All yield (nil, -32600).
func TestParseBatchTopLevelRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"object", `{"jsonrpc":"2.0","method":"m","id":1}`},
		{"number", `1`},
		{"string", `"x"`},
		{"bool", `true`},
		{"null", `null`},
		{"empty-array", `[]`},
		{"empty-array-whitespace", ` [ ] `},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			elements, jrErr := ParseBatch([]byte(tc.body))
			if jrErr == nil {
				t.Fatalf("ParseBatch(%q): expected -32600, got %+v", tc.body, elements)
			}
			if jrErr.Code != CodeInvalidRequest {
				t.Errorf("code: got %d want %d (body %q)", jrErr.Code, CodeInvalidRequest, tc.body)
			}
			if jrErr.Message != CodeMessage(CodeInvalidRequest) {
				t.Errorf("message: got %q want %q", jrErr.Message, CodeMessage(CodeInvalidRequest))
			}
			if elements != nil {
				t.Errorf("elements must be nil on body-level error, got %+v", elements)
			}
		})
	}
}

// TestParseBatchNonObjectElements covers elements that are valid JSON but not
// request objects: numbers, strings, bools, null, and nested arrays. Each is
// an element-level -32600; valid siblings are unaffected and order preserved.
func TestParseBatchNonObjectElements(t *testing.T) {
	body := `[
		{"jsonrpc":"2.0","method":"a","id":1},
		5,
		"x",
		true,
		null,
		[1,2],
		{"jsonrpc":"2.0","method":"b","id":2}
	]`
	elements, jrErr := ParseBatch([]byte(body))
	if jrErr != nil {
		t.Fatalf("ParseBatch: unexpected body-level error: %v", jrErr)
	}
	if len(elements) != 7 {
		t.Fatalf("element count: got %d want 7", len(elements))
	}
	// Valid siblings at indexes 0 and 6.
	for idx, wantMethod := range map[int]string{0: "a", 6: "b"} {
		if elements[idx].Request == nil || elements[idx].Request.Method != wantMethod {
			t.Errorf("elements[%d]: want request with method %q, got %+v", idx, wantMethod, elements[idx])
		}
		if elements[idx].Err != nil {
			t.Errorf("elements[%d]: unexpected error %v", idx, elements[idx].Err)
		}
	}
	// Non-object elements 1..5 are element-level -32600.
	for idx := 1; idx <= 5; idx++ {
		if elements[idx].Request != nil {
			t.Errorf("elements[%d]: request must be nil, got %+v", idx, elements[idx].Request)
		}
		if elements[idx].ChainOverride != nil {
			t.Errorf("elements[%d]: chain override must be nil, got %q", idx, elements[idx].ChainOverride)
		}
		if elements[idx].Err == nil {
			t.Errorf("elements[%d]: expected element error, got %+v", idx, elements[idx])
			continue
		}
		if elements[idx].Err.Code != CodeInvalidRequest {
			t.Errorf("elements[%d]: code got %d want %d", idx, elements[idx].Err.Code, CodeInvalidRequest)
		}
		if elements[idx].Err.Message != CodeMessage(CodeInvalidRequest) {
			t.Errorf("elements[%d]: message got %q want %q", idx, elements[idx].Err.Message, CodeMessage(CodeInvalidRequest))
		}
	}
}

// TestParseBatchInvalidEnvelopeElements covers object elements that fail
// ParseSingle's envelope validation; the error code from ParseSingle is
// propagated per element.
func TestParseBatchInvalidEnvelopeElements(t *testing.T) {
	body := `[
		{"jsonrpc":"2.0","method":"m","id":1},
		{"jsonrpc":"2.0","id":2},
		{"jsonrpc":"1.0","method":"m","id":3},
		{"jsonrpc":"2.0","method":"m","params":"x","id":4}
	]`
	elements, jrErr := ParseBatch([]byte(body))
	if jrErr != nil {
		t.Fatalf("ParseBatch: unexpected body-level error: %v", jrErr)
	}
	if len(elements) != 4 {
		t.Fatalf("element count: got %d want 4", len(elements))
	}
	wantCodes := []int{0, CodeInvalidRequest, CodeInvalidRequest, CodeInvalidParams}
	for idx, want := range wantCodes {
		if want == 0 {
			if elements[idx].Err != nil || elements[idx].Request == nil {
				t.Errorf("elements[%d]: want valid request, got %+v", idx, elements[idx])
			}
			continue
		}
		if elements[idx].Request != nil {
			t.Errorf("elements[%d]: request must be nil, got %+v", idx, elements[idx].Request)
		}
		if elements[idx].Err == nil {
			t.Errorf("elements[%d]: expected error code %d, got nil", idx, want)
			continue
		}
		if elements[idx].Err.Code != want {
			t.Errorf("elements[%d]: code got %d want %d", idx, elements[idx].Err.Code, want)
		}
		if elements[idx].Err.Message != CodeMessage(want) {
			t.Errorf("elements[%d]: message got %q want %q", idx, elements[idx].Err.Message, CodeMessage(want))
		}
	}
}

// TestParseBatchNotificationDetection checks IsNotification on elements: a
// request without an id member is a notification, one with an id is not, and
// an explicit null id is still a regular (non-notification) request.
func TestParseBatchNotificationDetection(t *testing.T) {
	body := `[
		{"jsonrpc":"2.0","method":"notify","params":[]},
		{"jsonrpc":"2.0","method":"m","id":1},
		{"jsonrpc":"2.0","method":"m","id":null}
	]`
	elements, jrErr := ParseBatch([]byte(body))
	if jrErr != nil {
		t.Fatalf("ParseBatch: unexpected error: %v", jrErr)
	}
	if len(elements) != 3 {
		t.Fatalf("element count: got %d want 3", len(elements))
	}
	if elements[0].Request == nil || !IsNotification(elements[0].Request) {
		t.Errorf("elements[0]: want notification, got %+v", elements[0].Request)
	}
	if elements[1].Request == nil || IsNotification(elements[1].Request) {
		t.Errorf("elements[1]: want id-bearing request, got %+v", elements[1].Request)
	}
	if elements[2].Request == nil || IsNotification(elements[2].Request) {
		t.Errorf("elements[2]: explicit null id must not be a notification, got %+v", elements[2].Request)
	}
}

// TestParseBatchChainOverride covers raw x-chain-id extraction: string form,
// number form, absent, and null (present but null). The raw bytes are
// preserved without any type validation — the router owns that.
func TestParseBatchChainOverride(t *testing.T) {
	body := `[
		{"jsonrpc":"2.0","method":"a","id":1,"x-chain-id":"8453"},
		{"jsonrpc":"2.0","method":"b","id":2,"x-chain-id":8453},
		{"jsonrpc":"2.0","method":"c","id":3},
		{"jsonrpc":"2.0","method":"d","id":4,"x-chain-id":null},
		{"jsonrpc":"2.0","method":"e","id":5,"x-chain-id":true}
	]`
	elements, jrErr := ParseBatch([]byte(body))
	if jrErr != nil {
		t.Fatalf("ParseBatch: unexpected error: %v", jrErr)
	}
	if len(elements) != 5 {
		t.Fatalf("element count: got %d want 5", len(elements))
	}
	wantOverrides := []string{`"8453"`, `8453`, ``, `null`, `true`}
	for idx, want := range wantOverrides {
		if elements[idx].Err != nil || elements[idx].Request == nil {
			t.Fatalf("elements[%d]: want valid request, got %+v", idx, elements[idx])
		}
		var got string
		if elements[idx].ChainOverride != nil {
			got = string(elements[idx].ChainOverride)
		}
		if got != want {
			t.Errorf("elements[%d] x-chain-id: got %q want %q", idx, got, want)
		}
	}
}

// TestParseBatchMixedIsolation locks in the US2 core guarantee: one bad
// element never poisons its siblings, and element order is the request order.
func TestParseBatchMixedIsolation(t *testing.T) {
	body := `[{"jsonrpc":"2.0","method":"a","id":1}, 7, {"jsonrpc":"2.0","method":"b","id":"two"}]`
	elements, jrErr := ParseBatch([]byte(body))
	if jrErr != nil {
		t.Fatalf("ParseBatch: unexpected error: %v", jrErr)
	}
	if len(elements) != 3 {
		t.Fatalf("element count: got %d want 3", len(elements))
	}
	if elements[0].Request == nil || elements[0].Request.Method != "a" || string(elements[0].Request.ID) != "1" {
		t.Errorf("elements[0]: want method a id 1, got %+v", elements[0].Request)
	}
	if elements[1].Err == nil || elements[1].Err.Code != CodeInvalidRequest {
		t.Errorf("elements[1]: want -32600, got %+v", elements[1].Err)
	}
	if elements[2].Request == nil || elements[2].Request.Method != "b" || string(elements[2].Request.ID) != `"two"` {
		t.Errorf("elements[2]: want method b id \"two\", got %+v", elements[2].Request)
	}
}

// TestMarshalBatchEmpty: an empty response list serializes to nil, nil — the
// caller decides the empty-body behavior (all-notification batches).
func TestMarshalBatchEmpty(t *testing.T) {
	out, err := MarshalBatch(nil)
	if out != nil || err != nil {
		t.Errorf("MarshalBatch(nil): got %q, %v want nil, nil", out, err)
	}
	out, err = MarshalBatch([]*Response{})
	if out != nil || err != nil {
		t.Errorf("MarshalBatch([]): got %q, %v want nil, nil", out, err)
	}
}

// TestMarshalBatchOrdering verifies ordered single-line output joining each
// Response.Marshal() with commas, no HTML escaping, and no trailing newline.
func TestMarshalBatchOrdering(t *testing.T) {
	responses := []*Response{
		NewResultResponse(json.RawMessage(`1`), json.RawMessage(`{"ok":true}`)),
		NewErrorResponse(json.RawMessage(`"two"`), CodeMethodNotFound, nil),
	}
	out, err := MarshalBatch(responses)
	if err != nil {
		t.Fatalf("MarshalBatch: %v", err)
	}
	want := `[{"jsonrpc":"2.0","result":{"ok":true},"id":1},{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not found"},"id":"two"}]`
	if string(out) != want {
		t.Errorf("MarshalBatch:\n got %s\nwant %s", out, want)
	}
	if bytes.Contains(out, []byte("\n")) {
		t.Errorf("output must be a single line: %q", out)
	}
	if bytes.Contains(out, []byte(`\u`)) {
		t.Errorf("HTML escaping must be disabled: %q", out)
	}
	// Round-trip: the output must be one valid JSON array.
	var decoded []map[string]json.RawMessage
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("output not valid JSON: %v (%s)", err, out)
	}
	if len(decoded) != 2 {
		t.Fatalf("decoded length: got %d want 2", len(decoded))
	}
	if string(decoded[0]["result"]) != `{"ok":true}` || string(decoded[1]["error"]) == "" {
		t.Errorf("order or members wrong: %s", out)
	}
}

// TestParseBatchMarshalBatchIDRoundTrip verifies byte-for-byte id echo
// through the full batch path: ParseBatch keeps the raw id bytes and
// MarshalBatch emits them unchanged, including big integers beyond float64
// precision.
func TestParseBatchMarshalBatchIDRoundTrip(t *testing.T) {
	ids := []string{`1`, `"abc-1"`, `null`, `-7`, `9007199254740993`}
	for _, id := range ids {
		body := `[{"jsonrpc":"2.0","method":"m","id":` + id + `}]`
		elements, jrErr := ParseBatch([]byte(body))
		if jrErr != nil {
			t.Fatalf("ParseBatch(%s): %v", body, jrErr)
		}
		if len(elements) != 1 || elements[0].Request == nil || elements[0].Err != nil {
			t.Fatalf("ParseBatch(%s): expected one valid element, got %+v", body, elements)
		}
		if string(elements[0].Request.ID) != id {
			t.Errorf("parsed id: got %q want %q", elements[0].Request.ID, id)
		}
		resp := NewResultResponse(elements[0].Request.ID, json.RawMessage(`{"ok":true}`))
		out, err := MarshalBatch([]*Response{resp})
		if err != nil {
			t.Fatalf("MarshalBatch: %v", err)
		}
		if !bytes.Contains(out, []byte(`"id":`+id)) {
			t.Errorf("marshaled batch %s does not echo id %s byte-for-byte", out, id)
		}
	}
}
