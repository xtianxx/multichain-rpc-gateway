package jsonrpc

import (
	"encoding/json"
	"fmt"
)

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Gateway-specific error codes (stable contract; range -32000..-32099).
const (
	CodeChainNotConfigured      = -32000
	CodeUpstreamUnavailable     = -32001
	CodeInvalidUpstreamResponse = -32002
	CodeBatchTooLarge           = -32003
	CodeBodyTooLarge            = -32004
	CodeUpstreamTimeout         = -32005
)

// CodeMessage returns the stable English message for a JSON-RPC or gateway
// error code, or "" for unknown codes. The messages are contractual and must
// not be reworded.
func CodeMessage(code int) string {
	switch code {
	case CodeParseError:
		return "Parse error"
	case CodeInvalidRequest:
		return "Invalid Request"
	case CodeMethodNotFound:
		return "Method not found"
	case CodeInvalidParams:
		return "Invalid params"
	case CodeInternalError:
		return "Internal error"
	case CodeChainNotConfigured:
		return "Chain not configured"
	case CodeUpstreamUnavailable:
		return "Upstream unavailable"
	case CodeInvalidUpstreamResponse:
		return "Invalid upstream response"
	case CodeBatchTooLarge:
		return "Batch too large"
	case CodeBodyTooLarge:
		return "Request body too large"
	case CodeUpstreamTimeout:
		return "Upstream timeout"
	default:
		return ""
	}
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message != "" {
		return fmt.Sprintf("%d %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%d", e.Code)
}

// NewErrorResponse builds an error response echoing id, with the stable
// message for code and optional structured data context.
func NewErrorResponse(id json.RawMessage, code int, data any) *Response {
	return &Response{
		Error: &Error{Code: code, Message: CodeMessage(code), Data: data},
		ID:    id,
	}
}

// NewResultResponse builds a success response echoing id with the raw result
// embedded verbatim.
func NewResultResponse(id json.RawMessage, result json.RawMessage) *Response {
	return &Response{Result: result, ID: id}
}
