package jsonrpc

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
)

// Request is a validated single JSON-RPC 2.0 request. Unknown members (such as
// future gateway metadata) are tolerated and ignored.
type Request struct {
	Method string
	Params json.RawMessage // nil when absent; validated array/object/null
	ID     json.RawMessage // nil when notification (id member absent)
}

// Response carries exactly one of Result or Error, plus the echoed id.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
	ID     json.RawMessage `json:"id"`
}

// wire is the serialization shape: it adds the mandatory "jsonrpc":"2.0"
// member that the public Response deliberately omits.
type wire struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	ID      json.RawMessage `json:"id"`
}

var ws = func() []byte { return []byte(" \t\r\n") }

func skipWhitespace(b []byte) []byte { return bytes.TrimLeft(b, " \t\r\n") }

// IsNotification reports whether req has no id member.
func IsNotification(req *Request) bool { return req.ID == nil }

// IsBatch reports whether body starts (first non-whitespace byte) with '['.
func IsBatch(body []byte) bool {
	trimmed := skipWhitespace(body)
	return len(trimmed) > 0 && trimmed[0] == '['
}

var idNumberRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// ParseSingle validates body as exactly one JSON-RPC 2.0 request object.
// It returns a *Error carrying the relevant code on failure. The raw bytes of
// the id and params members are preserved exactly for byte-faithful echoing.
func ParseSingle(body []byte) (*Request, *Error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var first json.RawMessage
	if err := dec.Decode(&first); err != nil {
		return nil, errJSON(CodeParseError)
	}
	// Trailing content after the first value is a parse error.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, errJSON(CodeParseError)
	}
	trimmed := skipWhitespace(first)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		// Valid JSON but not an object: arrays, strings, numbers, bools, null.
		return nil, errJSON(CodeInvalidRequest)
	}

	var members map[string]json.RawMessage
	if err := json.Unmarshal(first, &members); err != nil {
		return nil, errJSON(CodeInvalidRequest)
	}

	// jsonrpc must be exactly the string "2.0".
	version, ok := members["jsonrpc"]
	if !ok || string(version) != `"2.0"` {
		return nil, errJSON(CodeInvalidRequest)
	}

	// method must be a string.
	methodRaw, ok := members["method"]
	if !ok {
		return nil, errJSON(CodeInvalidRequest)
	}
	m := skipWhitespace(methodRaw)
	if len(m) == 0 || m[0] != '"' {
		return nil, errJSON(CodeInvalidRequest) // null/number/object/array/bool
	}
	var method string
	if err := json.Unmarshal(methodRaw, &method); err != nil {
		return nil, errJSON(CodeInvalidRequest)
	}

	req := &Request{Method: method}

	// params: if present, must be array, object, or null.
	if paramsRaw, ok := members["params"]; ok {
		p := skipWhitespace(paramsRaw)
		if len(p) == 0 || (p[0] != '[' && p[0] != '{' && p[0] != 'n') {
			return nil, errJSON(CodeInvalidParams)
		}
		if p[0] == 'n' && string(p) != "null" {
			return nil, errJSON(CodeInvalidParams)
		}
		req.Params = paramsRaw
	}

	// id: if present, must be string, integer number, or null.
	if idRaw, ok := members["id"]; ok {
		id := skipWhitespace(idRaw)
		switch {
		case len(id) == 0:
			return nil, errJSON(CodeInvalidRequest)
		case id[0] == '"':
			var s string
			if err := json.Unmarshal(id, &s); err != nil {
				return nil, errJSON(CodeInvalidRequest)
			}
		case id[0] == 'n':
			if string(id) != "null" {
				return nil, errJSON(CodeInvalidRequest)
			}
		default:
			var n json.Number
			if err := json.Unmarshal(id, &n); err != nil {
				return nil, errJSON(CodeInvalidRequest)
			}
			// Reject fractions and exponents per spec strictness ("SHOULD NOT
			// contain fractional parts" is enforced here as MUST NOT).
			if !idNumberRe.MatchString(n.String()) {
				return nil, errJSON(CodeInvalidRequest)
			}
		}
		req.ID = idRaw
	}

	return req, nil
}

func errJSON(code int) *Error {
	return &Error{Code: code, Message: CodeMessage(code)}
}

// Marshal serializes r as a single-line JSON object with "jsonrpc":"2.0"
// prepended, result/error embedded verbatim, and no HTML escaping.
func (r *Response) Marshal() ([]byte, error) {
	w := wire{JSONRPC: "2.0", Result: r.Result, Error: r.Error, ID: r.ID}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(w); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ValidUpstreamResponse checks that an upstream body is a syntactically
// complete JSON-RPC 2.0 response object with exactly one of result/error, and
// that its id is byte-for-byte identical to sentID.
func ValidUpstreamResponse(body []byte, sentID json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var first json.RawMessage
	if err := dec.Decode(&first); err != nil {
		return false
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return false // trailing content
	}
	trimmed := skipWhitespace(first)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(first, &m); err != nil {
		return false
	}
	if string(m["jsonrpc"]) != `"2.0"` {
		return false
	}
	if !bytes.Equal(m["id"], sentID) {
		return false
	}
	result := skipWhitespace(m["result"])
	errMember := skipWhitespace(m["error"])
	hasResult := len(result) > 0
	hasError := len(errMember) > 0
	if hasResult == hasError {
		return false // must have exactly one
	}
	if hasError && errMember[0] != '{' {
		return false // error member must be an object
	}
	return true
}
