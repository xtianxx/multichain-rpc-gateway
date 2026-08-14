package jsonrpc

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestParseSingleValid covers the happy path: params in every legal form and
// a missing id (notification).
func TestParseSingleValid(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		method     string
		wantParams string
		hasParams  bool
	}{
		{"params-array", `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xabc","latest"],"id":1}`, "eth_getBalance", `["0xabc","latest"]`, true},
		{"params-object", `{"jsonrpc":"2.0","method":"eth_call","params":{"to":"0xabc"},"id":2}`, "eth_call", `{"to":"0xabc"}`, true},
		{"params-null", `{"jsonrpc":"2.0","method":"m","params":null,"id":3}`, "m", "null", true},
		{"params-absent", `{"jsonrpc":"2.0","method":"m","id":4}`, "m", "", false},
		{"notification", `{"jsonrpc":"2.0","method":"m","params":[]}`, "m", `[]`, true},
		{"leading-whitespace", `  {"jsonrpc":"2.0","method":"m","id":5}`, "m", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, jrErr := ParseSingle([]byte(tc.body))
			if jrErr != nil {
				t.Fatalf("ParseSingle(%s): unexpected error: %v", tc.body, jrErr)
			}
			if req == nil {
				t.Fatal("ParseSingle returned nil request")
			}
			if req.Method != tc.method {
				t.Errorf("method: got %q want %q", req.Method, tc.method)
			}
			if tc.hasParams {
				if req.Params == nil || string(req.Params) != tc.wantParams {
					t.Errorf("params: got %q want %q", req.Params, tc.wantParams)
				}
			} else if req.Params != nil {
				t.Errorf("params: expected nil, got %q", req.Params)
			}
		})
	}
}

// TestParseSingleInvalidParamsCarriesRequest verifies that an invalid-params
// failure (-32602) still returns the request envelope: callers can tell
// whether it was a notification (id member absent), read the raw id for
// byte-exact echoing, and see the offending params. An explicit "id":null
// is a request, not a notification.
func TestParseSingleInvalidParamsCarriesRequest(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		notification bool
		wantID       string // raw id bytes; "" when the member is absent
	}{
		{"notification-number-params", `{"jsonrpc":"2.0","method":"m","params":5}`, true, ""},
		{"notification-string-params", `{"jsonrpc":"2.0","method":"m","params":"x"}`, true, ""},
		{"notification-bool-params", `{"jsonrpc":"2.0","method":"m","params":true}`, true, ""},
		{"id-number", `{"jsonrpc":"2.0","method":"m","params":5,"id":42}`, false, "42"},
		{"id-string", `{"jsonrpc":"2.0","method":"m","params":5,"id":"42"}`, false, `"42"`},
		{"id-null-is-request", `{"jsonrpc":"2.0","method":"m","params":5,"id":null}`, false, "null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, jrErr := ParseSingle([]byte(tc.body))
			if jrErr == nil || jrErr.Code != CodeInvalidParams {
				t.Fatalf("ParseSingle(%s): want -32602, got %v", tc.body, jrErr)
			}
			if req == nil {
				t.Fatalf("ParseSingle(%s): invalid-params failure must return the request", tc.body)
			}
			if req.Method != "m" {
				t.Errorf("method: got %q want m", req.Method)
			}
			if IsNotification(req) != tc.notification {
				t.Errorf("IsNotification: got %v want %v (body %s)", IsNotification(req), tc.notification, tc.body)
			}
			if tc.wantID == "" {
				if req.ID != nil {
					t.Errorf("id: want nil, got %q", req.ID)
				}
			} else if string(req.ID) != tc.wantID {
				t.Errorf("id: got %q want %q (raw bytes)", req.ID, tc.wantID)
			}
			if string(req.Params) == "" {
				t.Errorf("params: raw invalid params must be preserved (body %s)", tc.body)
			}
		})
	}
}

// TestIsNotification checks that absence of the id member (and only that)
// marks a notification; an explicit null id is NOT a notification.
func TestIsNotification(t *testing.T) {
	req, jrErr := ParseSingle([]byte(`{"jsonrpc":"2.0","method":"m"}`))
	if jrErr != nil {
		t.Fatalf("ParseSingle: %v", jrErr)
	}
	if req.ID != nil {
		t.Errorf("notification id: got %q want nil", req.ID)
	}
	if !IsNotification(req) {
		t.Error("request without id must be a notification")
	}

	req, jrErr = ParseSingle([]byte(`{"jsonrpc":"2.0","method":"m","id":1}`))
	if jrErr != nil {
		t.Fatalf("ParseSingle: %v", jrErr)
	}
	if IsNotification(req) {
		t.Error("request with id must not be a notification")
	}

	// Explicit null id is a valid id, not a notification.
	req, jrErr = ParseSingle([]byte(`{"jsonrpc":"2.0","method":"m","id":null}`))
	if jrErr != nil {
		t.Fatalf("ParseSingle: %v", jrErr)
	}
	if IsNotification(req) {
		t.Error("request with explicit null id must not be a notification")
	}
}

// TestIDByteExactEcho verifies the response id is byte-for-byte identical to
// the request id, including the distinct string "1" vs number 1 cases.
func TestIDByteExactEcho(t *testing.T) {
	ids := []string{`"abc-1"`, `1`, `"1"`, `null`, `0`, `-7`, `"0x1"`, `"8453"`}
	for _, id := range ids {
		body := `{"jsonrpc":"2.0","method":"m","id":` + id + `}`
		req, jrErr := ParseSingle([]byte(body))
		if jrErr != nil {
			t.Fatalf("ParseSingle(%s): %v", body, jrErr)
		}
		if string(req.ID) != id {
			t.Errorf("id: got %q want %q", req.ID, id)
		}
		resp := NewResultResponse(req.ID, json.RawMessage(`{"ok":true}`))
		out, err := resp.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !bytes.Contains(out, []byte(`"id":`+id)) {
			t.Errorf("marshaled response %s does not echo id %s byte-for-byte", out, id)
		}
	}
}

// TestParseSingleErrors is the strictness table: parse errors, envelope
// errors, params errors and id lexical errors.
func TestParseSingleErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
	}{
		{"unclosed-brace", `{`, CodeParseError},
		{"bad-member", `{"jsonrpc":}`, CodeParseError},
		{"trailing-garbage", `{"jsonrpc":"2.0"} x`, CodeParseError},
		{"empty-body", ``, CodeParseError},
		{"array-empty", `[]`, CodeInvalidRequest},
		{"array-elements", `[1,2]`, CodeInvalidRequest},
		{"string-scalar", `"x"`, CodeInvalidRequest},
		{"number-scalar", `5`, CodeInvalidRequest},
		{"bool-scalar", `true`, CodeInvalidRequest},
		{"null-scalar", `null`, CodeInvalidRequest},
		{"jsonrpc-1.0", `{"jsonrpc":"1.0","method":"m","id":1}`, CodeInvalidRequest},
		{"jsonrpc-missing", `{"method":"m","id":1}`, CodeInvalidRequest},
		{"jsonrpc-number", `{"jsonrpc":2.0,"method":"m","id":1}`, CodeInvalidRequest},
		{"jsonrpc-null", `{"jsonrpc":null,"method":"m","id":1}`, CodeInvalidRequest},
		{"method-number", `{"jsonrpc":"2.0","method":5,"id":1}`, CodeInvalidRequest},
		{"method-missing", `{"jsonrpc":"2.0","id":1}`, CodeInvalidRequest},
		{"method-null", `{"jsonrpc":"2.0","method":null,"id":1}`, CodeInvalidRequest},
		{"method-object", `{"jsonrpc":"2.0","method":{"a":1},"id":1}`, CodeInvalidRequest},
		{"params-string", `{"jsonrpc":"2.0","method":"m","params":"x","id":1}`, CodeInvalidParams},
		{"params-number", `{"jsonrpc":"2.0","method":"m","params":5,"id":1}`, CodeInvalidParams},
		{"params-bool", `{"jsonrpc":"2.0","method":"m","params":true,"id":1}`, CodeInvalidParams},
		{"id-bool", `{"jsonrpc":"2.0","method":"m","id":true}`, CodeInvalidRequest},
		{"id-float", `{"jsonrpc":"2.0","method":"m","id":1.5}`, CodeInvalidRequest},
		{"id-exponent", `{"jsonrpc":"2.0","method":"m","id":1e3}`, CodeInvalidRequest},
		{"id-fractional-zero", `{"jsonrpc":"2.0","method":"m","id":1.0}`, CodeInvalidRequest},
		{"id-object", `{"jsonrpc":"2.0","method":"m","id":{}}`, CodeInvalidRequest},
		{"id-array", `{"jsonrpc":"2.0","method":"m","id":[]}`, CodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, jrErr := ParseSingle([]byte(tc.body))
			if jrErr == nil {
				t.Fatalf("ParseSingle(%s): expected error, got request %+v", tc.body, req)
			}
			if jrErr.Code != tc.code {
				t.Errorf("code: got %d want %d (body %s)", jrErr.Code, tc.code, tc.body)
			}
			if jrErr.Message != CodeMessage(tc.code) {
				t.Errorf("message: got %q want %q", jrErr.Message, CodeMessage(tc.code))
			}
		})
	}
}

// TestParseSingleErrorTable re-runs the error table with the params array
// case removed and a fresh object-level check for valid arrays.
func TestParamsArrayValid(t *testing.T) {
	_, jrErr := ParseSingle([]byte(`{"jsonrpc":"2.0","method":"m","params":["a"],"id":1}`))
	if jrErr != nil {
		t.Fatalf("params array of strings must be valid: %v", jrErr)
	}
}

// TestUnknownMemberTolerated locks in gateway metadata (x-chain-id) tolerance.
func TestUnknownMemberTolerated(t *testing.T) {
	body := `{"jsonrpc":"2.0","method":"eth_chainId","id":1,"x-chain-id":"8453"}`
	req, jrErr := ParseSingle([]byte(body))
	if jrErr != nil {
		t.Fatalf("unknown member must be tolerated: %v", jrErr)
	}
	if req.Method != "eth_chainId" {
		t.Errorf("method: got %q", req.Method)
	}
	if string(req.ID) != "1" {
		t.Errorf("id: got %q want 1", req.ID)
	}
}

// TestCodeValues pins the constant values from the stable contract.
func TestCodeValues(t *testing.T) {
	want := map[int]int{
		CodeParseError: -32700, CodeInvalidRequest: -32600, CodeMethodNotFound: -32601,
		CodeInvalidParams: -32602, CodeInternalError: -32603,
		CodeChainNotConfigured: -32000, CodeUpstreamUnavailable: -32001,
		CodeInvalidUpstreamResponse: -32002, CodeBatchTooLarge: -32003,
		CodeBodyTooLarge: -32004, CodeUpstreamTimeout: -32005,
	}
	for code, wantVal := range want {
		if code != wantVal {
			t.Errorf("constant mismatch: got %d want %d", code, wantVal)
		}
	}
}

// TestCodeMessage is the full 11-entry stable message table.
func TestCodeMessage(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{CodeParseError, "Parse error"},
		{CodeInvalidRequest, "Invalid Request"},
		{CodeMethodNotFound, "Method not found"},
		{CodeInvalidParams, "Invalid params"},
		{CodeInternalError, "Internal error"},
		{CodeChainNotConfigured, "Chain not configured"},
		{CodeUpstreamUnavailable, "Upstream unavailable"},
		{CodeInvalidUpstreamResponse, "Invalid upstream response"},
		{CodeBatchTooLarge, "Batch too large"},
		{CodeBodyTooLarge, "Request body too large"},
		{CodeUpstreamTimeout, "Upstream timeout"},
	}
	for _, tc := range cases {
		if got := CodeMessage(tc.code); got != tc.want {
			t.Errorf("CodeMessage(%d): got %q want %q", tc.code, got, tc.want)
		}
	}
	if got := CodeMessage(0); got != "" {
		t.Errorf("CodeMessage(0): got %q want empty for unknown code", got)
	}
}

// TestMarshalResult verifies single-line output, "jsonrpc":"2.0" first,
// raw result bytes embedded without HTML escaping, and id echo.
func TestMarshalResult(t *testing.T) {
	result := json.RawMessage(`{"html":"<tag>","x":1}`)
	resp := NewResultResponse(json.RawMessage(`"abc-1"`), result)
	out, err := resp.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(out, []byte(`"jsonrpc":"2.0"`)) {
		t.Errorf("missing jsonrpc member: %s", out)
	}
	if !bytes.Contains(out, result) {
		t.Errorf("result raw bytes not embedded verbatim: %s", out)
	}
	if bytes.Contains(out, []byte(`\u003c`)) {
		t.Errorf("HTML escaping applied: %s", out)
	}
	if !bytes.Contains(out, []byte(`"id":"abc-1"`)) {
		t.Errorf("id not echoed: %s", out)
	}
	if bytes.Contains(out, []byte("\n")) {
		t.Errorf("output must be a single line: %q", out)
	}
	if bytes.Contains(out, []byte(`"error"`)) {
		t.Errorf("result response must not contain error: %s", out)
	}

	// Round-trip: re-parse the serialized response.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not valid JSON: %v (%s)", err, out)
	}
	if string(m["jsonrpc"]) != `"2.0"` || string(m["result"]) != string(result) || string(m["id"]) != `"abc-1"` {
		t.Errorf("round-trip mismatch: %s", out)
	}
	if _, hasErr := m["error"]; hasErr {
		t.Errorf("round-trip: unexpected error member: %s", out)
	}
}

// TestMarshalError verifies error serialization: code, message, optional
// data, no result member, and nil data omitted.
func TestMarshalError(t *testing.T) {
	resp := NewErrorResponse(json.RawMessage(`1`), CodeUpstreamUnavailable, map[string]any{"chain_id": "999"})
	out, err := resp.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(out, []byte(`"error":`)) {
		t.Errorf("missing error member: %s", out)
	}
	if bytes.Contains(out, []byte(`"result"`)) {
		t.Errorf("error response must not contain result: %s", out)
	}
	if !bytes.Contains(out, []byte(`"code":-32001`)) {
		t.Errorf("missing code: %s", out)
	}
	if !bytes.Contains(out, []byte(`"message":"Upstream unavailable"`)) {
		t.Errorf("missing stable message: %s", out)
	}
	if !bytes.Contains(out, []byte(`"data":{"chain_id":"999"}`)) {
		t.Errorf("missing data: %s", out)
	}
	if !bytes.Contains(out, []byte(`"id":1`)) {
		t.Errorf("missing id: %s", out)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	var e Error
	if err := json.Unmarshal(m["error"], &e); err != nil {
		t.Fatalf("error member not an error object: %v", err)
	}
	if e.Code != CodeUpstreamUnavailable || e.Message != "Upstream unavailable" {
		t.Errorf("decoded error mismatch: %+v", e)
	}

	// nil data must be omitted entirely.
	out2, err := NewErrorResponse(json.RawMessage(`1`), CodeInternalError, nil).Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(out2, []byte(`"data"`)) {
		t.Errorf("nil data must be omitted: %s", out2)
	}
}

// TestErrorError covers the error interface implementation.
func TestErrorError(t *testing.T) {
	e := &Error{Code: CodeChainNotConfigured, Message: CodeMessage(CodeChainNotConfigured)}
	s := e.Error()
	if !strings.Contains(s, "Chain not configured") {
		t.Errorf("Error() = %q, want it to contain the message", s)
	}
}

// TestIsBatch checks first-non-whitespace-byte detection.
func TestIsBatch(t *testing.T) {
	if !IsBatch([]byte(`[1,2]`)) {
		t.Error("[1,2] must be a batch")
	}
	if !IsBatch([]byte(`  [1]`)) {
		t.Error("batch with leading whitespace must be detected")
	}
	if IsBatch([]byte(`{"a":1}`)) {
		t.Error("object body must not be a batch")
	}
	if IsBatch([]byte(``)) {
		t.Error("empty body must not be a batch")
	}
	if IsBatch([]byte(`   `)) {
		t.Error("whitespace-only body must not be a batch")
	}
}

// TestValidUpstreamResponse covers acceptance and rejection of upstream
// response bodies.
func TestValidUpstreamResponse(t *testing.T) {
	valid := []byte(`{"jsonrpc":"2.0","result":"0x1","id":1}`)
	if !ValidUpstreamResponse(valid, json.RawMessage(`1`)) {
		t.Error("valid matching response must pass")
	}
	if ValidUpstreamResponse(valid, json.RawMessage(`2`)) {
		t.Error("id mismatch must fail")
	}
	if !ValidUpstreamResponse([]byte(`{"jsonrpc":"2.0","result":null,"id":"abc"}`), json.RawMessage(`"abc"`)) {
		t.Error("string id response must pass")
	}
	if ValidUpstreamResponse([]byte(`not json`), json.RawMessage(`1`)) {
		t.Error("non-JSON body must fail")
	}
	if !ValidUpstreamResponse([]byte(`{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not found"},"id":1}`), json.RawMessage(`1`)) {
		t.Error("error-only response must be valid")
	}
	if ValidUpstreamResponse([]byte(`{"jsonrpc":"2.0","result":1,"error":{"code":1,"message":"x"},"id":1}`), json.RawMessage(`1`)) {
		t.Error("result and error together must fail")
	}
	if ValidUpstreamResponse([]byte(`{"result":1,"id":1}`), json.RawMessage(`1`)) {
		t.Error("missing jsonrpc must fail")
	}
	if ValidUpstreamResponse([]byte(`{"jsonrpc":"2.0","result":1}`), json.RawMessage(`1`)) {
		t.Error("missing id must fail")
	}
	if ValidUpstreamResponse([]byte(`[1]`), json.RawMessage(`1`)) {
		t.Error("array body must fail")
	}
	if ValidUpstreamResponse([]byte(`{"jsonrpc":"2.0","result":1,"id":1} trailing`), json.RawMessage(`1`)) {
		t.Error("trailing content must fail")
	}
}
