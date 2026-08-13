// Package upstream implements the per-upstream forwarding client: pooled
// HTTP transports, bounded request timeouts, and strict JSON-RPC response
// validation (constitution II/III).
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/xtianxx/multichain-rpc-gateway/internal/chain"
	"github.com/xtianxx/multichain-rpc-gateway/internal/jsonrpc"
)

// Sentinel errors mapped by the router onto the stable gateway error codes:
// ErrUpstreamTimeout -> -32005, ErrUnreachable -> -32001,
// ErrInvalidResponse -> -32002.
var (
	ErrUpstreamTimeout = errors.New("upstream timeout")
	ErrUnreachable     = errors.New("upstream unreachable")
	ErrInvalidResponse = errors.New("invalid upstream response")
)

// NewHTTPClient builds a per-upstream http.Client with a pooled transport.
// Every upstream gets its own client so a single slow endpoint can never
// exhaust the connection pool of another (constitution III).
func NewHTTPClient() *http.Client {
	transport := &http.Transport{
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &http.Client{Transport: transport}
}

// Forward sends the prebuilt JSON-RPC envelope body to the upstream and
// returns the raw response body. The caller's context carries the per-method
// deadline. The response is accepted only when it is a complete JSON-RPC 2.0
// response whose id matches sentID byte-for-byte; 4xx/5xx statuses with
// non-JSON-RPC bodies are classified as unreachable, 2xx non-JSON-RPC bodies
// as invalid responses.
func Forward(ctx context.Context, u *chain.Upstream, body []byte, sentID json.RawMessage) ([]byte, error) {
	client := u.Client
	if client == nil {
		client = NewHTTPClient()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.URL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrUpstreamTimeout
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, ErrUpstreamTimeout
		}
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrUnreachable, err)
	}

	if jsonrpc.ValidUpstreamResponse(respBody, sentID) {
		return respBody, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%w: upstream returned HTTP %d", ErrUnreachable, resp.StatusCode)
	}
	return nil, ErrInvalidResponse
}
