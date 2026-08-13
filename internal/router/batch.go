package router

import (
	"bytes"
	"encoding/json"

	"github.com/xtianxx/multichain-rpc-gateway/internal/chain"
	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
)

// ResolveChainForElement resolves the target chain for one batch element.
// The optional per-element x-chain-id override takes precedence over the
// X-Chain-Id header; a missing override inherits the header (US2/T026).
// Resolution never forwards the override anywhere: Route rebuilds a clean
// envelope without gateway metadata.
//
// Override must be a JSON string or number; any other JSON type is an
// envelope violation (-32600 Invalid Request, no data context). Chain
// resolution failures (non-decimal, negative, fractional, empty, or
// unconfigured canonical id) follow the header-path convention: -32000
// with data {"chain_id": <raw value>}.
func (r *Router) ResolveChainForElement(override json.RawMessage, headerValue string) (*chain.Chain, *jsonrpc.Error) {
	if override == nil {
		return r.ResolveChain(headerValue)
	}
	raw, ok := decodeChainOverride(override)
	if !ok {
		return nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInvalidRequest,
			Message: jsonrpc.CodeMessage(jsonrpc.CodeInvalidRequest),
		}
	}
	canonical, err := chain.ParseChainID(raw)
	if err != nil {
		return nil, chainNotConfigured(raw)
	}
	ch, ok := r.chains[canonical]
	if !ok {
		return nil, chainNotConfigured(canonical)
	}
	return ch, nil
}

// decodeChainOverride decodes an x-chain-id override into its raw string
// form: the string value for a JSON string, the json.Number string form for
// a JSON number (decoded with UseNumber so "8453" never becomes a float).
// Any other JSON type (bool, object, array, null) or malformed JSON is
// rejected.
func decodeChainOverride(override json.RawMessage) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(override))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	default:
		return "", false
	}
}
