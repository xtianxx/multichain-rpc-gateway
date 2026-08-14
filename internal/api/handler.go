// Package api implements the gateway HTTP handler: the single POST /
// JSON-RPC entry point. It lives in a non-main package deliberately (a
// deviation from tasks.md's cmd/gateway/handler.go wording): package main is
// not importable, and the in-process integration tests (T018) must exercise
// the real handler through httptest.
//
// HTTP semantics (jsonrpc-api contract): malformed JSON -> 400 + -32700;
// body over limit -> 400 + -32004; everything else (gateway and upstream
// errors included) -> 200.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
	"github.com/xtianxx/multichain-rpc-gateway/internal/metrics"
	"github.com/xtianxx/multichain-rpc-gateway/internal/router"
)

// Handler serves the JSON-RPC endpoint.
type Handler struct {
	router           *router.Router
	maxBodyBytes     int64
	maxBatchElements int
	logger           *slog.Logger
}

// New builds the JSON-RPC handler. maxBatchElements bounds the number of
// elements in a batch request; a non-positive value falls back to 100 (the
// config default).
func New(rt *router.Router, maxBodyBytes int64, maxBatchElements int, logger *slog.Logger) *Handler {
	if maxBatchElements <= 0 {
		maxBatchElements = 100
	}
	return &Handler{router: rt, maxBodyBytes: maxBodyBytes, maxBatchElements: maxBatchElements, logger: logger}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			metrics.RecordRequest("-", "-", "-", "-32004")
			h.writeError(w, http.StatusBadRequest, nil, jsonrpc.CodeBodyTooLarge, nil)
			return
		}
		metrics.RecordRequest("-", "-", "-", "-32603")
		h.writeError(w, http.StatusInternalServerError, nil, jsonrpc.CodeInternalError, nil)
		return
	}

	// Batch orchestration (T027): parse, bound, then serve per element in
	// request order. Malformed JSON -> 400 + -32700; empty/non-array batch
	// -> 200 + -32600; element count over the limit -> 200 + -32003 with
	// nothing forwarded.
	if jsonrpc.IsBatch(body) {
		h.serveBatch(w, r, body)
		return
	}

	req, jrErr := jsonrpc.ParseSingle(body)
	if jrErr != nil {
		metrics.RecordRequest("-", "-", "-", strconv.Itoa(jrErr.Code))
		status := http.StatusOK
		if jrErr.Code == jsonrpc.CodeParseError {
			status = http.StatusBadRequest
		}
		// An invalid-params notification (envelope without an id member)
		// is rejected but never answered: no response element, mirroring
		// the valid-notification path. A request with a determinable id
		// echoes it byte-for-byte; -32700/-32600/-32603 keep id null.
		if req != nil {
			if jsonrpc.IsNotification(req) {
				w.WriteHeader(http.StatusOK)
				return
			}
			h.writeError(w, status, req.ID, jrErr.Code, jrErr.Data)
			return
		}
		h.writeError(w, status, nil, jrErr.Code, nil)
		return
	}

	// Notifications never produce a response element, even on error.
	notification := jsonrpc.IsNotification(req)

	if req.Method == "eth_subscribe" {
		metrics.RecordRequest("-", "-", "eth_subscribe", "-32601")
		if notification {
			w.WriteHeader(http.StatusOK)
			return
		}
		h.writeError(w, http.StatusOK, req.ID, jsonrpc.CodeMethodNotFound,
			map[string]any{"method": "eth_subscribe"})
		return
	}

	ch, jrErr := h.router.ResolveChain(r.Header.Get("X-Chain-Id"))
	if jrErr != nil {
		// Counted before the notification swallow check so swallowed
		// notifications still show up in metrics.
		metrics.RecordRequest("-", "-", req.Method, strconv.Itoa(jrErr.Code))
		if notification {
			w.WriteHeader(http.StatusOK)
			return
		}
		h.writeError(w, http.StatusOK, req.ID, jrErr.Code, jrErr.Data)
		return
	}

	result, jrErr, _ := h.router.Route(r.Context(), ch, req)
	if notification {
		w.WriteHeader(http.StatusOK)
		return
	}
	if jrErr != nil {
		h.writeError(w, http.StatusOK, req.ID, jrErr.Code, jrErr.Data)
		return
	}
	h.writeJSON(w, http.StatusOK, jsonrpc.NewResultResponse(req.ID, result))
}

func (h *Handler) writeError(w http.ResponseWriter, status int, id json.RawMessage, code int, data any) {
	h.writeJSON(w, status, jsonrpc.NewErrorResponse(id, code, data))
}

// serveBatch handles a batch body: per-element resolution and routing in
// request order. Errors are isolated per element; notifications produce no
// response element regardless of outcome (validate-before-forward: invalid
// notifications are never forwarded and never leak an error).
func (h *Handler) serveBatch(w http.ResponseWriter, r *http.Request, body []byte) {
	els, batchErr := jsonrpc.ParseBatch(body)
	if batchErr != nil {
		metrics.RecordRequest("-", "-", "batch", strconv.Itoa(batchErr.Code))
		status := http.StatusOK
		if batchErr.Code == jsonrpc.CodeParseError {
			status = http.StatusBadRequest
		}
		h.writeError(w, status, nil, batchErr.Code, nil)
		return
	}
	if len(els) > h.maxBatchElements {
		// Limit check precedes any per-element work: nothing is forwarded.
		metrics.RecordRequest("-", "-", "batch", "-32003")
		h.writeError(w, http.StatusOK, nil, jsonrpc.CodeBatchTooLarge, nil)
		return
	}

	headerChain := r.Header.Get("X-Chain-Id")
	responses := make([]*jsonrpc.Response, 0, len(els))
	for _, el := range els {
		switch {
		case el.Err != nil:
			// Invalid element, sibling isolated. An invalid-params
			// notification (no id member) is rejected but produces no
			// response element; everything else gets an error response
			// echoing the raw id when determinable (-32602) or id null
			// (-32700/-32600/-32603).
			metrics.RecordRequest("-", "-", "-", strconv.Itoa(el.Err.Code))
			if el.Notification {
				continue
			}
			responses = append(responses, jsonrpc.NewErrorResponse(el.ID, el.Err.Code, el.Err.Data))
		case el.Request.Method == "eth_subscribe":
			// No WebSocket support: never forward, notification or not.
			// A subscribe notification is swallowed without a response
			// element, mirroring the single-request path.
			metrics.RecordRequest("-", "-", "eth_subscribe", "-32601")
			if !jsonrpc.IsNotification(el.Request) {
				responses = append(responses, jsonrpc.NewErrorResponse(el.Request.ID, jsonrpc.CodeMethodNotFound,
					map[string]any{"method": "eth_subscribe"}))
			}
		case jsonrpc.IsNotification(el.Request):
			// Notifications never produce a response element; resolution
			// and routing errors are swallowed (but still counted).
			ch, jrErr := h.router.ResolveChainForElement(el.ChainOverride, headerChain)
			if jrErr != nil {
				metrics.RecordRequest("-", "-", el.Request.Method, strconv.Itoa(jrErr.Code))
			} else {
				_, _, _ = h.router.Route(r.Context(), ch, el.Request)
			}
		default:
			ch, jrErr := h.router.ResolveChainForElement(el.ChainOverride, headerChain)
			if jrErr != nil {
				metrics.RecordRequest("-", "-", el.Request.Method, strconv.Itoa(jrErr.Code))
				responses = append(responses, jsonrpc.NewErrorResponse(el.Request.ID, jrErr.Code, jrErr.Data))
				continue
			}
			result, jrErr, _ := h.router.Route(r.Context(), ch, el.Request)
			if jrErr != nil {
				responses = append(responses, jsonrpc.NewErrorResponse(el.Request.ID, jrErr.Code, jrErr.Data))
				continue
			}
			responses = append(responses, jsonrpc.NewResultResponse(el.Request.ID, result))
		}
	}

	if len(responses) == 0 {
		// All notifications: bare 200, no body, no Content-Type (mirrors
		// the single-notification path).
		w.WriteHeader(http.StatusOK)
		return
	}
	out, err := jsonrpc.MarshalBatch(responses)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, resp *jsonrpc.Response) {
	out, err := resp.Marshal()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}
