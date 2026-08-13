// Package chain models configured chains, their adapters, and upstream
// runtime state. Adding a chain is a configuration + adapter exercise only:
// the routing core never branches on a specific chain (constitution I).
package chain

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HealthState is the liveness classification of an upstream.
type HealthState int32

const (
	HealthUnknown   HealthState = iota // 0: not yet probed; lowest routing priority
	HealthHealthy                      // 1: passing probes
	HealthUnhealthy                    // 2: failing probes
)

// Upstream is a configured RPC endpoint with runtime state. Health and
// latency are written by the prober and read by the router concurrently;
// access must go through the accessors (atomic).
type Upstream struct {
	Name   string
	URL    *url.URL
	Client *http.Client // wired by the upstream package (US1); nil until then

	health  int32 // HealthState
	latency int64 // nanoseconds; EWMA of probe/request latency
}

// SetHealth updates the health classification (race-safe).
func (u *Upstream) SetHealth(h HealthState) { atomic.StoreInt32(&u.health, int32(h)) }

// Health returns the current health classification (race-safe).
func (u *Upstream) Health() HealthState { return HealthState(atomic.LoadInt32(&u.health)) }

// SetLatency updates the EWMA latency (race-safe).
func (u *Upstream) SetLatency(d time.Duration) { atomic.StoreInt64(&u.latency, int64(d)) }

// Latency returns the current EWMA latency (race-safe).
func (u *Upstream) Latency() time.Duration { return time.Duration(atomic.LoadInt64(&u.latency)) }

// Chain is one configured chain: a canonical decimal chain id, its adapter,
// and its ordered upstreams.
type Chain struct {
	ChainID   string
	Adapter   Adapter
	Upstreams []*Upstream
}

// Adapter isolates chain-specific behaviour. Unknown JSON-RPC methods pass
// through adapters untouched (constitution II).
type Adapter interface {
	Name() string
	// NormalizeParams applies chain-specific request normalization to the
	// params member before forwarding upstream (EIP-1898 block-param form).
	NormalizeParams(params json.RawMessage) (json.RawMessage, error)
	// ShapeResponse post-processes an upstream result payload (chain-specific
	// error-code normalization). Identity passthrough is allowed.
	ShapeResponse(result json.RawMessage) json.RawMessage
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Adapter{}
)

// RegisterAdapter registers a chain adapter; it panics on a duplicate name.
func RegisterAdapter(a Adapter) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[a.Name()]; dup {
		panic(fmt.Sprintf("chain adapter %q already registered", a.Name()))
	}
	registry[a.Name()] = a
}

// GetAdapter returns the adapter registered under name.
func GetAdapter(name string) (Adapter, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	a, ok := registry[name]
	return a, ok
}

// AdapterNames returns the sorted names of registered adapters.
func AdapterNames() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var decimalRe = regexp.MustCompile(`^[0-9]+$`)

// ParseChainID normalizes a chain id (decimal string or JSON number) to its
// canonical decimal form. Leading zeros are stripped ("08453" -> "8453");
// non-decimal, negative, fractional, or empty inputs are rejected.
func ParseChainID(v any) (string, error) {
	var raw string
	switch t := v.(type) {
	case string:
		raw = t
	case json.Number:
		raw = t.String()
	default:
		return "", fmt.Errorf("chain id must be a decimal string or number, got %T", v)
	}
	if !decimalRe.MatchString(raw) {
		return "", fmt.Errorf("chain id %q is not a decimal non-negative integer", raw)
	}
	canonical := strings.TrimLeft(raw, "0")
	if canonical == "" {
		canonical = "0"
	}
	return canonical, nil
}

// NewChain builds a Chain; adapterName must be registered and chainID must be
// a valid decimal chain id (canonicalized on success).
func NewChain(chainID string, adapterName string, upstreams []*Upstream) (*Chain, error) {
	canonical, err := ParseChainID(chainID)
	if err != nil {
		return nil, err
	}
	adapter, ok := GetAdapter(adapterName)
	if !ok {
		return nil, fmt.Errorf("adapter %q is not registered", adapterName)
	}
	return &Chain{ChainID: canonical, Adapter: adapter, Upstreams: upstreams}, nil
}

// normalizeBlockParams implements the EIP-1898 block-parameter form shared by
// both adapters: when the last element of a positional params array is an
// object with exactly one key "blockNumber", it is replaced by that value.
// Tag names (latest/earliest/pending/safe/finalized) and block hashes pass
// through untouched. Object (named) params pass through untouched.
func normalizeBlockParams(params json.RawMessage) (json.RawMessage, error) {
	if len(params) == 0 {
		return nil, nil
	}
	trimmed := []byte(strings.TrimLeft(string(params), " \t\r\n"))
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return params, nil // named params or scalar: nothing to normalize
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(params, &elems); err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	if len(elems) == 0 {
		return params, nil
	}
	last := elems[len(elems)-1]
	lastTrimmed := strings.TrimLeft(string(last), " \t\r\n")
	if len(lastTrimmed) == 0 || lastTrimmed[0] != '{' {
		return params, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(last, &obj); err != nil {
		return params, nil // not a plain object: pass through
	}
	blockNumber, ok := obj["blockNumber"]
	if !ok || len(obj) != 1 {
		return params, nil // blockHash or multi-key block spec: pass through
	}
	elems[len(elems)-1] = blockNumber
	out, err := json.Marshal(elems)
	if err != nil {
		return nil, fmt.Errorf("normalize params: %w", err)
	}
	return out, nil
}
