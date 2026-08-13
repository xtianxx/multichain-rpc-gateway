package upstream

import (
	"errors"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
)

// ErrNonRetryableMethod marks a method class that must never be retried.
var ErrNonRetryableMethod = errors.New("non-retryable method")

// writeMethods blacklists write methods by exact full-method match. The
// blacklist always wins over the whitelist prefixes.
var writeMethods = map[string]bool{
	"eth_sendTransaction":    true,
	"eth_sendRawTransaction": true,
}

// readPrefixes whitelists method classes that are safe to retry after an
// upstream failure.
var readPrefixes = []string{
	"eth_get",
	"eth_call",
	"eth_estimateGas",
	"eth_blockNumber",
	"eth_chainId",
	"eth_syncing",
	"net_",
	"web3_",
}

// Retryable reports whether method may be retried after an upstream failure.
// The write-method blacklist (exact full-method match) always wins over the
// whitelist prefixes; unclassified methods default to retryable (read
// assumption).
func Retryable(method string) bool {
	if writeMethods[method] {
		return false
	}
	for _, prefix := range readPrefixes {
		if strings.HasPrefix(method, prefix) {
			return true
		}
	}
	return true
}

// permanentBackOff is a backoff policy that never retries: exactly one
// attempt, no failover. It carries the permanent error so callers can match
// it with errors.Is. (backoff v4.3.0's PermanentError does not implement
// backoff.BackOff, hence this local wrapper with identical semantics.)
type permanentBackOff struct {
	err error
}

func (b *permanentBackOff) NextBackOff() time.Duration { return backoff.Stop }
func (b *permanentBackOff) Reset()                     {}
func (b *permanentBackOff) Error() string              { return b.err.Error() }
func (b *permanentBackOff) Unwrap() error              { return b.err }

// Backoff builds the retry backoff for a method: exponential (base delay x2,
// full jitter, capped by max_elapsed) for safe methods; an immediate
// permanent stop for the blacklist (exactly one attempt, no failover).
func Backoff(cfg config.Retry, method string) backoff.BackOff {
	if !Retryable(method) {
		return &permanentBackOff{err: ErrNonRetryableMethod}
	}

	base := cfg.BaseDelay.Std()
	if base <= 0 {
		base = 10 * time.Millisecond
	}
	maxElapsed := cfg.MaxElapsed.Std()
	if maxElapsed <= 0 {
		maxElapsed = 30 * time.Second
	}

	bo := &backoff.ExponentialBackOff{
		InitialInterval:     base,
		RandomizationFactor: 1.0, // full jitter: delay in [0, 2*interval)
		Multiplier:          2.0,
		MaxInterval:         maxElapsed,
		MaxElapsedTime:      maxElapsed,
		// NextBackOff returns this field (not the backoff.Stop constant)
		// once the elapsed cap trips; without it a tripped backoff would
		// return 0 and be retried forever.
		Stop:  backoff.Stop,
		Clock: backoff.SystemClock,
	}
	// Initialize currentInterval and startTime; without this the elapsed
	// time is measured from the zero time and the first NextBackOff would
	// immediately return Stop.
	bo.Reset()
	return bo
}
