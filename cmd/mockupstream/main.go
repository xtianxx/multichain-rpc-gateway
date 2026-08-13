// Command mockupstream is a standalone mock JSON-RPC upstream (T045, Phase 7).
//
// It impersonates a single per-chain Ethereum-compatible upstream so the demo
// scripts (`make demo`, `make demo-failover`, `make load`) can run the gateway
// against a fake chain. It serves:
//
//	POST /        JSON-RPC 2.0 handler (single request or batch)
//	POST /_ctl    fault injection control: {"mode":"ok"|"500"|"timeout"|"garbage"}
//	GET  /_ctl    current fault mode as JSON
//	GET  /_health plain "ok" for scripts to wait on
//
// No graceful shutdown handling is needed: the demo scripts terminate this
// process with a trap/kill, so a plain ListenAndServe loop is sufficient.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// Fault injection modes for the JSON-RPC endpoint.
const (
	modeOK int32 = iota
	mode500
	modeTimeout
	modeGarbage
)

// timeoutDelay is how long the "timeout" fault mode sleeps before answering.
// The gateway's default upstream timeout is 10s, so a 3s sleep still returns
// (only a gateway configured with a tighter deadline sees the timeout).
const timeoutDelay = 3 * time.Second

// Fixed mock results (plausible minimal values).
const (
	mockBlockNumber = "0x10b9d7"
	mockBalance     = "0xde0b6b3a7640000"
	mockGasPrice    = "0x3b9aca00"
	mockTxCount     = "0x1a"
	mockCallGas     = "0x5208"
	mockClientVer   = "MockUpstream/v0.1"
)

// rpcRequest is the JSON-RPC 2.0 request envelope. ID is a pointer so that a
// missing "id" member (a notification) is distinguishable from id:null.
type rpcRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params"`
	ID      *json.RawMessage `json:"id"`
}

// rpcError is the JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcResponse is the JSON-RPC 2.0 response envelope. The ID is echoed
// byte-for-byte as json.RawMessage.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	ID      json.RawMessage `json:"id"`
}

// block is the minimal eth_getBlockByNumber result.
type block struct {
	Number string `json:"number"`
	Hash   string `json:"hash"`
}

// mockUpstream carries the chain identity, the fault mode, and the
// transaction-hash counter. The mode is an atomic int so /_ctl toggles are
// thread-safe against concurrent JSON-RPC requests.
type mockUpstream struct {
	chainID int
	mode    atomic.Int32
	counter atomic.Uint64
	logger  *log.Logger
}

// newMockUpstream builds an upstream impersonating chainID, logging to stderr.
func newMockUpstream(chainID int) *mockUpstream {
	return &mockUpstream{
		chainID: chainID,
		logger:  log.New(os.Stderr, "", log.LstdFlags),
	}
}

// newMockHandler assembles the HTTP routing for the mock upstream. Factored as
// a function so tests can mount it on an httptest server without binding ports.
func newMockHandler(u *mockUpstream) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", u.handleRPC)
	mux.HandleFunc("/_ctl", u.handleCtl)
	mux.HandleFunc("/_health", u.handleHealth)
	return mux
}

// handleRPC serves the JSON-RPC endpoint, applying the current fault mode.
// Request payloads are never logged.
func (u *mockUpstream) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch u.mode.Load() {
	case mode500:
		w.WriteHeader(http.StatusInternalServerError)
		return
	case modeTimeout:
		time.Sleep(timeoutDelay)
	case modeGarbage:
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "not json")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		u.writeParseError(w)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		u.writeParseError(w)
		return
	}
	if trimmed[0] == '[' {
		u.handleBatch(w, trimmed)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		u.writeParseError(w)
		return
	}
	if req.ID == nil {
		// Notification: process (side effects such as the tx counter) but
		// return no response body.
		_, _ = u.resultFor(req.Method)
		return
	}
	result, rpcErr := u.resultFor(req.Method)
	u.writeResponse(w, *req.ID, result, rpcErr)
}

// handleBatch serves an array of requests, preserving order. Notifications
// (no "id" member) produce no response element; if every element is a
// notification the server returns nothing, per the JSON-RPC 2.0 spec.
func (u *mockUpstream) handleBatch(w http.ResponseWriter, body []byte) {
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		u.writeParseError(w)
		return
	}

	responses := make([]json.RawMessage, 0, len(raw))
	for _, el := range raw {
		var req rpcRequest
		if err := json.Unmarshal(el, &req); err != nil {
			responses = append(responses, mustMarshal(rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32700, Message: "Parse error"},
				ID:      json.RawMessage("null"),
			}))
			continue
		}
		if req.ID == nil {
			continue // notification
		}
		result, rpcErr := u.resultFor(req.Method)
		responses = append(responses, mustMarshal(rpcResponse{
			JSONRPC: "2.0",
			Result:  result,
			Error:   rpcErr,
			ID:      *req.ID,
		}))
	}

	if len(responses) == 0 {
		return // all notifications: empty body
	}
	w.Header().Set("Content-Type", "application/json")
	// A []json.RawMessage marshals as a JSON array of its raw contents.
	_ = json.NewEncoder(w).Encode(responses)
}

// resultFor maps a method name to its mock result or a JSON-RPC error.
func (u *mockUpstream) resultFor(method string) (json.RawMessage, *rpcError) {
	switch method {
	case "eth_chainId":
		return json.RawMessage(strconv.Quote(fmt.Sprintf("0x%x", u.chainID))), nil
	case "net_version":
		return json.RawMessage(strconv.Quote(strconv.Itoa(u.chainID))), nil
	case "eth_blockNumber":
		return json.RawMessage(strconv.Quote(mockBlockNumber)), nil
	case "eth_getBalance":
		return json.RawMessage(strconv.Quote(mockBalance)), nil
	case "eth_gasPrice":
		return json.RawMessage(strconv.Quote(mockGasPrice)), nil
	case "eth_getTransactionCount":
		return json.RawMessage(strconv.Quote(mockTxCount)), nil
	case "eth_call", "eth_estimateGas":
		return json.RawMessage(strconv.Quote(mockCallGas)), nil
	case "eth_getLogs":
		return json.RawMessage("[]"), nil
	case "eth_getBlockByNumber":
		b, err := json.Marshal(block{Number: mockBlockNumber, Hash: u.blockHash()})
		if err != nil {
			return nil, &rpcError{Code: -32603, Message: "Internal error"}
		}
		return b, nil
	case "eth_getTransactionByHash":
		return json.RawMessage("null"), nil
	case "web3_clientVersion":
		return json.RawMessage(strconv.Quote(mockClientVer)), nil
	case "eth_sendRawTransaction", "eth_sendTransaction":
		return json.RawMessage(strconv.Quote(u.newTxHash())), nil
	default:
		return nil, &rpcError{Code: -32601, Message: "Method not found"}
	}
}

// writeResponse writes a single JSON-RPC response envelope.
func (u *mockUpstream) writeResponse(w http.ResponseWriter, id json.RawMessage, result json.RawMessage, rpcErr *rpcError) {
	resp := rpcResponse{JSONRPC: "2.0", Result: result, Error: rpcErr, ID: id}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// writeParseError responds -32700 with a null id (the id cannot be known when
// the envelope is malformed).
func (u *mockUpstream) writeParseError(w http.ResponseWriter) {
	u.writeResponse(w, json.RawMessage("null"), nil, &rpcError{Code: -32700, Message: "Parse error"})
}

// handleCtl toggles the fault mode (POST {"mode":"ok"|"500"|"timeout"|"garbage"})
// and reports it (GET).
func (u *mockUpstream) handleCtl(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		u.writeMode(w)
	case http.MethodPost:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var req struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		m, ok := parseMode(req.Mode)
		if !ok {
			http.Error(w, "unknown mode", http.StatusBadRequest)
			return
		}
		if u.mode.Swap(m) != m {
			u.logger.Printf("mockupstream mode=%s", modeName(m))
		}
		u.writeMode(w)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleHealth is a plain liveness endpoint for scripts to wait on.
func (u *mockUpstream) handleHealth(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "ok")
}

// writeMode writes the current fault mode as {"mode":"..."}.
func (u *mockUpstream) writeMode(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"mode":%q}`, modeName(u.mode.Load()))
}

// blockHash is a deterministic per-chain block hash (0x + 64 hex chars).
func (u *mockUpstream) blockHash() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("block-%d", u.chainID)))
	return "0x" + hex.EncodeToString(sum[:])
}

// newTxHash returns a deterministic 0x + 64 hex char hash derived from a
// monotonically increasing counter.
func (u *mockUpstream) newTxHash() string {
	n := u.counter.Add(1)
	sum := sha256.Sum256([]byte(fmt.Sprintf("tx-%d-%d", u.chainID, n)))
	return "0x" + hex.EncodeToString(sum[:])
}

// parseMode maps a mode string to its constant.
func parseMode(s string) (int32, bool) {
	switch s {
	case "ok":
		return modeOK, true
	case "500":
		return mode500, true
	case "timeout":
		return modeTimeout, true
	case "garbage":
		return modeGarbage, true
	default:
		return 0, false
	}
}

// modeName maps a mode constant back to its string.
func modeName(m int32) string {
	switch m {
	case modeOK:
		return "ok"
	case mode500:
		return "500"
	case modeTimeout:
		return "timeout"
	case modeGarbage:
		return "garbage"
	default:
		return "unknown"
	}
}

// mustMarshal marshals a value that cannot fail to serialize.
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func main() {
	chainID := flag.Int("chain-id", 1, "decimal chain id this upstream impersonates")
	listen := flag.String("listen", ":19545", "listen address for the JSON-RPC endpoint")
	flag.Parse()

	u := newMockUpstream(*chainID)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           newMockHandler(u),
		ReadHeaderTimeout: 10 * time.Second,
	}

	u.logger.Printf("mockupstream starting chain_id=%d listen=%s", *chainID, *listen)
	// No graceful shutdown: the demo scripts kill this process via trap, so a
	// plain ListenAndServe loop is sufficient.
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		u.logger.Fatalf("mockupstream: %v", err)
	}
}
