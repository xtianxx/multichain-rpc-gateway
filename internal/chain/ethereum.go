package chain

import "encoding/json"

// EthereumAdapter adapts Ethereum mainnet requests: EIP-1898 block-parameter
// normalization, identity response shaping.
type EthereumAdapter struct{}

// Name returns the registered adapter name.
func (EthereumAdapter) Name() string { return "ethereum" }

// NormalizeParams applies EIP-1898 block-param normalization.
func (EthereumAdapter) NormalizeParams(params json.RawMessage) (json.RawMessage, error) {
	return normalizeBlockParams(params)
}

// ShapeResponse passes results through unchanged.
func (EthereumAdapter) ShapeResponse(result json.RawMessage) json.RawMessage {
	return result
}

func init() {
	RegisterAdapter(EthereumAdapter{})
}
