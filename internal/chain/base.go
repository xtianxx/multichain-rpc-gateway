package chain

import "encoding/json"

// BaseAdapter adapts Base (OP-stack) requests: EIP-1898 block-parameter
// normalization plus OP-stack error-shape normalization.
type BaseAdapter struct{}

// Name returns the registered adapter name.
func (BaseAdapter) Name() string { return "base" }

// NormalizeParams applies EIP-1898 block-param normalization.
func (BaseAdapter) NormalizeParams(params json.RawMessage) (json.RawMessage, error) {
	return normalizeBlockParams(params)
}

// ShapeResponse normalizes OP-stack error shapes that leak into result
// payloads: Base nodes surface "execution reverted" as error code 3; the
// gateway canonicalizes it to the standard -32000 with a stable message.
// Everything else passes through unchanged.
func (BaseAdapter) ShapeResponse(result json.RawMessage) json.RawMessage {
	if len(result) == 0 {
		return result
	}
	if result[0] == ' ' || result[0] == '\t' || result[0] == '\n' || result[0] == '\r' {
		for _, b := range result {
			if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
				continue
			}
			if b != '{' {
				return result
			}
			break
		}
	} else if result[0] != '{' {
		return result
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		return result
	}
	var code int64
	if err := json.Unmarshal(obj["code"], &code); err != nil {
		return result
	}
	if _, hasMsg := obj["message"]; !hasMsg || code != 3 {
		return result
	}
	// OP-stack execution-reverted shape: canonicalize code 3 -> -32000.
	out, err := json.Marshal(map[string]any{
		"code":    int64(-32000),
		"message": "execution reverted",
	})
	if err != nil {
		return result
	}
	return out
}

func init() {
	RegisterAdapter(BaseAdapter{})
}
