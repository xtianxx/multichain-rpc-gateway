// Package router resolves the target chain from the X-Chain-Id header,
// selects upstreams (health/latency preference, US3), and records routing
// outcomes. Safe methods are retried with failover across candidates; write
// methods are forwarded exactly once.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/xtianxx/multichain-rpc-gateway/internal/chain"
	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
	"github.com/xtianxx/multichain-rpc-gateway/internal/upstream"
)

// RoutingRecord is the transient per-request outcome: chain, method,
// upstream, outcome ("success" or an error code string), latency, retries.
// It carries no payload and feeds logs/metrics only (constitution V).
type RoutingRecord struct {
	ChainID  string
	Method   string
	Upstream string
	Outcome  string
	Latency  time.Duration
	Retries  int
}

// Router holds the chain registry and configuration.
type Router struct {
	cfg    *config.Config
	chains map[string]*chain.Chain
	logger *slog.Logger
}

// New builds a Router from configuration: every configured chain is
// constructed (adapter must be registered), and every upstream gets its own
// pooled HTTP client.
func New(cfg *config.Config, logger *slog.Logger) (*Router, error) {
	r := &Router{cfg: cfg, chains: map[string]*chain.Chain{}, logger: logger}
	for i, cc := range cfg.Chains {
		ups := make([]*chain.Upstream, 0, len(cc.Upstreams))
		for _, uc := range cc.Upstreams {
			parsed, err := url.Parse(uc.URL)
			if err != nil {
				return nil, fmt.Errorf("chains[%d] upstream %q: %w", i, uc.URL, err)
			}
			name := uc.Name
			if name == "" {
				name = parsed.Host // host-only default alias; never embeds credentials
			}
			ups = append(ups, &chain.Upstream{
				Name:   name,
				URL:    parsed,
				Client: upstream.NewHTTPClient(),
			})
		}
		// Wire per-upstream circuit breakers at startup (US3). A zero
		// fail_threshold disables breaking (nil breaker -> direct execution).
		if r.cfg.Circuit.FailThreshold > 0 {
			for _, u := range ups {
				u.SetBreaker(upstream.NewBreaker(u.Name, r.cfg.Circuit.FailThreshold, r.cfg.Circuit.Cooldown.Std()))
			}
		}
		ch, err := chain.NewChain(cc.ChainID, cc.Adapter, ups)
		if err != nil {
			return nil, fmt.Errorf("chains[%d] %q: %w", i, cc.ChainID, err)
		}
		r.chains[ch.ChainID] = ch
	}
	return r, nil
}

// ResolveChain maps an X-Chain-Id header value to a configured chain. A
// missing header, a non-canonical value, or an unconfigured chain id all
// yield -32000 with structured data context (never forwarded upstream).
func (r *Router) ResolveChain(headerValue string) (*chain.Chain, *jsonrpc.Error) {
	if headerValue == "" {
		return nil, chainNotConfigured(nil)
	}
	canonical, err := chain.ParseChainID(headerValue)
	if err != nil {
		return nil, chainNotConfigured(headerValue)
	}
	ch, ok := r.chains[canonical]
	if !ok {
		return nil, chainNotConfigured(canonical)
	}
	return ch, nil
}

func chainNotConfigured(raw any) *jsonrpc.Error {
	return &jsonrpc.Error{
		Code:    jsonrpc.CodeChainNotConfigured,
		Message: jsonrpc.CodeMessage(jsonrpc.CodeChainNotConfigured),
		Data:    map[string]any{"chain_id": raw},
	}
}

// Route forwards req through ch's upstreams with retry and failover, and
// returns the shaped result, a JSON-RPC error (gateway or upstream
// passthrough), and the routing record. The per-method-class timeout bounds
// the whole attempt sequence; each attempt gets an equal slice of it.
func (r *Router) Route(ctx context.Context, ch *chain.Chain, req *jsonrpc.Request) (result json.RawMessage, jrErr *jsonrpc.Error, rec RoutingRecord) {
	rec = RoutingRecord{ChainID: ch.ChainID, Method: req.Method}
	start := time.Now()
	defer func() {
		rec.Latency = time.Since(start)
		r.logRecord(rec)
	}()

	ctx, cancel := context.WithTimeout(ctx, r.methodTimeout(req.Method))
	defer cancel()

	params, err := ch.Adapter.NormalizeParams(req.Params)
	if err != nil {
		jrErr := &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: jsonrpc.CodeMessage(jsonrpc.CodeInvalidParams)}
		rec.Outcome = strconv.Itoa(jrErr.Code)
		return nil, jrErr, rec
	}

	// Rebuild a clean envelope: gateway metadata (x-chain-id etc.) is dropped
	// by construction; unknown methods pass through untouched.
	envelope := map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"2.0"`),
		"method":  json.RawMessage(strconv.Quote(req.Method)),
	}
	if params != nil {
		envelope["params"] = params
	}
	if req.ID != nil {
		envelope["id"] = req.ID
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		jrErr := &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: jsonrpc.CodeMessage(jsonrpc.CodeInternalError)}
		rec.Outcome = strconv.Itoa(jrErr.Code)
		return nil, jrErr, rec
	}

	cands := r.candidates(ch)
	if len(cands) == 0 {
		// Every upstream is breaker-open: nothing to try.
		jrErr := &jsonrpc.Error{
			Code:    jsonrpc.CodeUpstreamUnavailable,
			Message: jsonrpc.CodeMessage(jsonrpc.CodeUpstreamUnavailable),
			Data:    map[string]any{"chain_id": ch.ChainID},
		}
		rec.Outcome = strconv.Itoa(jrErr.Code)
		return nil, jrErr, rec
	}

	// Write methods are never retried; notifications are forwarded exactly
	// once (no response slot to fill, so a retry only doubles upstream load).
	// Everything else follows the configured attempt budget (guarded to two
	// attempts when unset).
	maxAttempts := r.cfg.Retry.MaxAttempts
	if jsonrpc.IsNotification(req) || !upstream.Retryable(req.Method) {
		maxAttempts = 1
	} else if maxAttempts <= 0 {
		maxAttempts = 2
	}

	total := r.methodTimeout(req.Method)
	attemptTimeout := total / time.Duration(maxAttempts)
	if attemptTimeout <= 0 {
		attemptTimeout = total
	}
	bo := upstream.Backoff(r.cfg.Retry, req.Method)

	var lastErr error
retryLoop:
	for attempt := 0; attempt < maxAttempts; attempt++ {
		up := cands[min(attempt, len(cands)-1)]
		rec.Upstream = up.Name
		if rec.Upstream == "" {
			rec.Upstream = up.URL.Host
		}

		attemptCtx, attemptCancel := context.WithTimeout(ctx, attemptTimeout)
		attemptStart := time.Now()
		respBody, err := up.Execute(func() ([]byte, error) {
			return upstream.Forward(attemptCtx, up, body, req.ID)
		})
		attemptLatency := time.Since(attemptStart)
		attemptCancel()

		if err == nil {
			up.RecordLatency(attemptLatency)
			rec.Latency = attemptLatency
			rec.Retries = attempt

			var parsed struct {
				Result json.RawMessage `json:"result"`
				Error  *jsonrpc.Error  `json:"error"`
			}
			if err := json.Unmarshal(respBody, &parsed); err != nil {
				jrErr := &jsonrpc.Error{Code: jsonrpc.CodeInvalidUpstreamResponse, Message: jsonrpc.CodeMessage(jsonrpc.CodeInvalidUpstreamResponse)}
				rec.Outcome = strconv.Itoa(jrErr.Code)
				return nil, jrErr, rec
			}
			if parsed.Error != nil {
				// Upstream JSON-RPC errors pass through to the client untouched
				// (no failover on valid responses).
				rec.Outcome = strconv.Itoa(parsed.Error.Code)
				return nil, parsed.Error, rec
			}
			rec.Outcome = "success"
			return ch.Adapter.ShapeResponse(parsed.Result), nil, rec
		}

		// Retryable failure: back off (aborting early once the request
		// deadline fires) and move to the next candidate.
		lastErr = err
		if attempt+1 >= maxAttempts {
			break
		}
		d := bo.NextBackOff()
		if d == backoff.Stop {
			break
		}
		if d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				lastErr = upstream.ErrUpstreamTimeout
				break retryLoop
			}
		}
	}

	jrErr = r.mapUpstreamError(lastErr, rec.Upstream)
	rec.Outcome = strconv.Itoa(jrErr.Code)
	return nil, jrErr, rec
}

// candidates returns the try order for ch: upstreams with an open circuit
// breaker are excluded; healthy upstreams come first (lowest EWMA latency
// first, stable), then unknown ones, then unhealthy ones, each group in
// config order.
func (r *Router) candidates(ch *chain.Chain) []*chain.Upstream {
	var healthy, unknown, unhealthy []*chain.Upstream
	for _, u := range ch.Upstreams {
		if u.BreakerOpen() {
			continue
		}
		switch u.Health() {
		case chain.HealthHealthy:
			healthy = append(healthy, u)
		case chain.HealthUnhealthy:
			unhealthy = append(unhealthy, u)
		default:
			unknown = append(unknown, u)
		}
	}
	sort.SliceStable(healthy, func(i, j int) bool { return healthy[i].Latency() < healthy[j].Latency() })
	return append(append(healthy, unknown...), unhealthy...)
}

// Chains returns all configured chains sorted by chain id ascending (decimal
// numeric order; "2" sorts before "10").
func (r *Router) Chains() []*chain.Chain {
	out := make([]*chain.Chain, 0, len(r.chains))
	for _, ch := range r.chains {
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].ChainID, out[j].ChainID
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return a < b
	})
	return out
}

// mapUpstreamError translates upstream client sentinel errors onto the stable
// gateway error codes, with structured context.
func (r *Router) mapUpstreamError(err error, upstreamName string) *jsonrpc.Error {
	code := jsonrpc.CodeUpstreamUnavailable
	switch {
	case errors.Is(err, upstream.ErrUpstreamTimeout):
		code = jsonrpc.CodeUpstreamTimeout
	case errors.Is(err, upstream.ErrInvalidResponse):
		code = jsonrpc.CodeInvalidUpstreamResponse
	case errors.Is(err, upstream.ErrCircuitOpen):
		code = jsonrpc.CodeUpstreamUnavailable
	}
	return &jsonrpc.Error{
		Code:    code,
		Message: jsonrpc.CodeMessage(code),
		Data:    map[string]any{"upstream": upstreamName},
	}
}

// methodTimeout returns the per-method-class timeout: longest prefix match
// against cfg.Server.Timeouts, falling back to the "default" entry.
func (r *Router) methodTimeout(method string) time.Duration {
	timeouts := r.cfg.Server.Timeouts
	seconds, ok := timeouts["default"]
	if !ok || seconds <= 0 {
		seconds = 10
	}
	best := ""
	for prefix, s := range timeouts {
		if prefix != "default" && s > 0 && strings.HasPrefix(method, prefix) && len(prefix) > len(best) {
			best, seconds = prefix, s
		}
	}
	return time.Duration(seconds) * time.Second
}

func (r *Router) logRecord(rec RoutingRecord) {
	r.logger.Info("request",
		"chain_id", rec.ChainID,
		"method", rec.Method,
		"upstream", rec.Upstream,
		"outcome", rec.Outcome,
		"latency", rec.Latency.String(),
		"retries", rec.Retries,
	)
}
