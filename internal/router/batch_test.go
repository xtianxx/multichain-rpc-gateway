package router

import (
	"encoding/json"
	"testing"

	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
)

// TestResolveChainForElement covers the per-element x-chain-id override
// (US2/T026): override wins over the X-Chain-Id header, absent override
// inherits the header, envelope violations are -32600, and every chain
// resolution failure is -32000 with structured data.chain_id context.
func TestResolveChainForElement(t *testing.T) {
	cfg := testConfig([]config.Chain{
		{ChainID: "1", Adapter: "ethereum", Upstreams: []config.Upstream{{Name: "eth-a", URL: "https://eth.example.com"}}},
		{ChainID: "8453", Adapter: "base", Upstreams: []config.Upstream{{Name: "base-a", URL: "https://base.example.com"}}},
	}, nil)
	r, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := []struct {
		name      string
		override  json.RawMessage // nil = x-chain-id absent
		header    string
		wantChain string // checked on success
		wantCode  int    // 0 = success
		wantData  any    // data.chain_id expectation; nil = must be absent
	}{
		{"string-override-beats-header", json.RawMessage(`"8453"`), "1", "8453", 0, nil},
		{"number-override-beats-header", json.RawMessage(`8453`), "1", "8453", 0, nil},
		{"string-override-without-header", json.RawMessage(`"8453"`), "", "8453", 0, nil},
		{"leading-zeros-override", json.RawMessage(`"008453"`), "999", "8453", 0, nil},
		{"absent-inherits-header", nil, "1", "1", 0, nil},
		{"absent-inherits-header-base", nil, "8453", "8453", 0, nil},
		{"absent-missing-header", nil, "", "", jsonrpc.CodeChainNotConfigured, nil},
		{"absent-invalid-header", nil, "0x2105", "", jsonrpc.CodeChainNotConfigured, "0x2105"},
		{"unknown-chain-override", json.RawMessage(`"999"`), "1", "", jsonrpc.CodeChainNotConfigured, "999"},
		{"hex-override", json.RawMessage(`"0x2105"`), "1", "", jsonrpc.CodeChainNotConfigured, "0x2105"},
		{"fractional-override", json.RawMessage(`1.5`), "1", "", jsonrpc.CodeChainNotConfigured, "1.5"},
		{"negative-override", json.RawMessage(`-1`), "1", "", jsonrpc.CodeChainNotConfigured, "-1"},
		{"empty-string-override", json.RawMessage(`""`), "1", "", jsonrpc.CodeChainNotConfigured, ""},
		{"bool-override", json.RawMessage(`true`), "1", "", jsonrpc.CodeInvalidRequest, nil},
		{"object-override", json.RawMessage(`{"a":1}`), "1", "", jsonrpc.CodeInvalidRequest, nil},
		{"array-override", json.RawMessage(`[1]`), "1", "", jsonrpc.CodeInvalidRequest, nil},
		{"null-override", json.RawMessage(`null`), "1", "", jsonrpc.CodeInvalidRequest, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, jrErr := r.ResolveChainForElement(tc.override, tc.header)
			if tc.wantCode == 0 {
				if jrErr != nil {
					t.Fatalf("unexpected error: %v", jrErr)
				}
				if ch.ChainID != tc.wantChain {
					t.Errorf("chain: got %s want %s", ch.ChainID, tc.wantChain)
				}
				return
			}
			if jrErr == nil {
				t.Fatalf("expected error code %d, got nil", tc.wantCode)
			}
			if jrErr.Code != tc.wantCode {
				t.Errorf("code: got %d want %d", jrErr.Code, tc.wantCode)
			}
			if jrErr.Message != jsonrpc.CodeMessage(tc.wantCode) {
				t.Errorf("message: got %q want %q", jrErr.Message, jsonrpc.CodeMessage(tc.wantCode))
			}
			if tc.wantCode == jsonrpc.CodeInvalidRequest {
				if jrErr.Data != nil {
					t.Errorf("data must be absent for -32600, got %v", jrErr.Data)
				}
				return
			}
			data, _ := jrErr.Data.(map[string]any)
			if data["chain_id"] != tc.wantData {
				t.Errorf("data.chain_id: got %v want %v", data["chain_id"], tc.wantData)
			}
		})
	}
}
