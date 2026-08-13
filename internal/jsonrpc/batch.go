package jsonrpc

import (
	"bytes"
	"encoding/json"
	"io"
)

// BatchElement is one validated element of a batch request. Exactly one of
// Request or Err is non-nil: Request is set for valid request objects,
// Err carries the per-element validation error for everything else.
type BatchElement struct {
	Request       *Request        // nil when Err != nil
	ChainOverride json.RawMessage // raw "x-chain-id" member; nil when absent
	Err           *Error          // per-element validation error; nil when valid
}

// ParseBatch validates body as a JSON-RPC 2.0 batch. Body-level failures
// (undecodable JSON, trailing content after the array, non-array top-level,
// empty array) return (nil, *Error). Elements are validated independently in
// request order: a bad element never affects its siblings.
func ParseBatch(body []byte) ([]BatchElement, *Error) {
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
	if len(trimmed) == 0 || trimmed[0] != '[' {
		// Valid JSON but not an array: objects, scalars, null.
		return nil, errJSON(CodeInvalidRequest)
	}

	var elements []json.RawMessage
	if err := json.Unmarshal(first, &elements); err != nil {
		// Unreachable: first is a fully decoded, valid JSON value.
		return nil, errJSON(CodeParseError)
	}
	if len(elements) == 0 {
		// Empty batch: invalid request.
		return nil, errJSON(CodeInvalidRequest)
	}

	out := make([]BatchElement, 0, len(elements))
	for _, el := range elements {
		t := skipWhitespace(el)
		if len(t) == 0 || t[0] != '{' {
			// Not a request object: number, string, bool, null, nested array.
			out = append(out, BatchElement{Err: errJSON(CodeInvalidRequest)})
			continue
		}
		req, jrErr := ParseSingle(el)
		if jrErr != nil {
			out = append(out, BatchElement{Err: jrErr})
			continue
		}
		var chainOverride json.RawMessage
		var members map[string]json.RawMessage
		if err := json.Unmarshal(el, &members); err == nil {
			// Raw bytes only; type validation is the router's job.
			chainOverride = members["x-chain-id"]
		}
		out = append(out, BatchElement{Request: req, ChainOverride: chainOverride})
	}
	return out, nil
}

// MarshalBatch serializes responses as an ordered single-line JSON array,
// joining each Response.Marshal() with commas, no HTML escaping. Returns nil
// (and no error) for an empty slice — the caller decides the empty-body
// behavior (e.g. all-notification batches).
func MarshalBatch(responses []*Response) ([]byte, error) {
	if len(responses) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, r := range responses {
		if i > 0 {
			buf.WriteByte(',')
		}
		out, err := r.Marshal()
		if err != nil {
			return nil, err
		}
		buf.Write(out)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}
