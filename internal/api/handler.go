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

	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
	"github.com/xtianxx/multichain-rpc-gateway/internal/router"
)

// Handler serves the JSON-RPC endpoint.
type Handler struct {
	router       *router.Router
	maxBodyBytes int64
	logger       *slog.Logger
}

// New builds the JSON-RPC handler.
func New(rt *router.Router, maxBodyBytes int64, logger *slog.Logger) *Handler {
	return &Handler{router: rt, maxBodyBytes: maxBodyBytes, logger: logger}
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
			h.writeError(w, http.StatusBadRequest, nil, jsonrpc.CodeBodyTooLarge, nil)
			return
		}
		h.writeError(w, http.StatusInternalServerError, nil, jsonrpc.CodeInternalError, nil)
		return
	}

	// Batch handling is US2; until then a batch body is an invalid request.
	if jsonrpc.IsBatch(body) {
		h.writeError(w, http.StatusOK, nil, jsonrpc.CodeInvalidRequest,
			map[string]any{"detail": "batch requests are not supported yet"})
		return
	}

	req, jrErr := jsonrpc.ParseSingle(body)
	if jrErr != nil {
		status := http.StatusOK
		if jrErr.Code == jsonrpc.CodeParseError {
			status = http.StatusBadRequest
		}
		h.writeError(w, status, nil, jrErr.Code, nil)
		return
	}

	// Notifications never produce a response element, even on error.
	notification := jsonrpc.IsNotification(req)

	if req.Method == "eth_subscribe" {
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
