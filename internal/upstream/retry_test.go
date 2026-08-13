package upstream

import (
	"errors"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
)

// fakeClock is a controllable backoff.Clock for deterministic backoff tests
// (no sleeps, no flaky timing).
type fakeClock struct {
	now time.Time
}

func (f *fakeClock) Now() time.Time { return f.now }

func TestRetryable(t *testing.T) {
	tests := []struct {
		method string
		want   bool
	}{
		// whitelisted read prefixes
		{"eth_getBalance", true},
		{"eth_getLogs", true},
		{"eth_call", true},
		{"eth_estimateGas", true},
		{"eth_blockNumber", true},
		{"eth_chainId", true},
		{"eth_syncing", true},
		{"net_version", true},
		{"web3_clientVersion", true},
		// exact-match write blacklist
		{"eth_sendTransaction", false},
		{"eth_sendRawTransaction", false},
		// unclassified methods default to retryable (read assumption)
		{"eth_foo", true},
		{"debug_traceTransaction", true},
		// near-miss: the blacklist is exact full-method match only
		{"eth_sendRawTransactionExtra", true},
	}
	for _, tt := range tests {
		if got := Retryable(tt.method); got != tt.want {
			t.Errorf("Retryable(%q) = %v, want %v", tt.method, got, tt.want)
		}
	}
}

func TestBackoffBlacklistPermanent(t *testing.T) {
	cfg := config.Retry{
		MaxAttempts: 3,
		BaseDelay:   config.Duration(10 * time.Millisecond),
		MaxElapsed:  config.Duration(time.Second),
	}
	bo := Backoff(cfg, "eth_sendRawTransaction")

	// Exactly one attempt: the very first NextBackOff returns Stop.
	if d := bo.NextBackOff(); d != backoff.Stop {
		t.Fatalf("blacklist backoff NextBackOff() = %v, want backoff.Stop", d)
	}

	// The permanent stop carries the sentinel for errors.Is matching.
	perr, ok := bo.(error)
	if !ok {
		t.Fatalf("blacklist backoff %T does not expose its permanent error", bo)
	}
	if !errors.Is(perr, ErrNonRetryableMethod) {
		t.Fatalf("errors.Is(backoff, ErrNonRetryableMethod) = false, got %v", perr)
	}
}

func TestBackoffRetryable(t *testing.T) {
	const base = 10 * time.Millisecond
	const maxElapsed = 100 * time.Millisecond
	cfg := config.Retry{
		MaxAttempts: 2,
		BaseDelay:   config.Duration(base),
		MaxElapsed:  config.Duration(maxElapsed),
	}

	bo := Backoff(cfg, "eth_call").(*backoff.ExponentialBackOff)
	clock := &fakeClock{now: time.Unix(0, 0)}
	bo.Clock = clock
	bo.Reset()

	// First delay: full jitter (RandomizationFactor 1.0) in [0, 2*base).
	d := bo.NextBackOff()
	if d < 0 || d > 2*base+time.Millisecond {
		t.Fatalf("first NextBackOff() = %v, want in [0, %v)", d, 2*base)
	}

	// After Reset, attempt i is bounded by 2*base*2^i; keep elapsed well
	// below maxElapsed so the bound (not the time cap) governs.
	const slack = time.Millisecond
	bo.Reset()
	for i := 0; i < 3; i++ {
		clock.now = clock.now.Add(base / 10)
		d := bo.NextBackOff()
		bound := 2*base*(1<<uint(i)) + slack
		if d < 0 || d > bound {
			t.Fatalf("attempt %d: NextBackOff() = %v, want in [0, %v]", i, d, bound)
		}
		if d == backoff.Stop {
			t.Fatalf("attempt %d: unexpected Stop (elapsed %v < maxElapsed %v)", i, clock.now.Sub(time.Unix(0, 0)), maxElapsed)
		}
	}

	// Tiny maxElapsed: once elapsed time passes, NextBackOff returns Stop
	// (loop bounded; deterministic via the fake clock).
	tinyCfg := config.Retry{
		MaxAttempts: 2,
		BaseDelay:   config.Duration(base),
		MaxElapsed:  config.Duration(time.Millisecond),
	}
	boTiny := Backoff(tinyCfg, "eth_blockNumber").(*backoff.ExponentialBackOff)
	clockTiny := &fakeClock{now: time.Unix(0, 0)}
	boTiny.Clock = clockTiny
	boTiny.Reset()

	stopped := false
	for i := 0; i < 100; i++ {
		if boTiny.NextBackOff() == backoff.Stop {
			stopped = true
			break
		}
		clockTiny.now = clockTiny.now.Add(2 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("backoff did not return Stop after max elapsed time passed")
	}
}

func TestBackoffDefaults(t *testing.T) {
	bo := Backoff(config.Retry{}, "eth_call").(*backoff.ExponentialBackOff)
	if bo.InitialInterval != 10*time.Millisecond {
		t.Errorf("default InitialInterval = %v, want 10ms", bo.InitialInterval)
	}
	if bo.MaxElapsedTime != 30*time.Second {
		t.Errorf("default MaxElapsedTime = %v, want 30s", bo.MaxElapsedTime)
	}
	if bo.MaxInterval != 30*time.Second {
		t.Errorf("default MaxInterval = %v, want 30s (capped by max_elapsed)", bo.MaxInterval)
	}
	if bo.RandomizationFactor != 1.0 {
		t.Errorf("RandomizationFactor = %v, want 1.0 (full jitter)", bo.RandomizationFactor)
	}
	if bo.Multiplier != 2.0 {
		t.Errorf("Multiplier = %v, want 2.0", bo.Multiplier)
	}
}

func TestBreaker(t *testing.T) {
	const cooldown = 50 * time.Millisecond
	b := NewBreaker("test", 2, cooldown)
	if got := b.State(); got != "closed" {
		t.Fatalf("initial State() = %q, want closed", got)
	}

	// Closed: fn runs and its result/error pass through verbatim.
	calls := 0
	res, err := b.Execute(func() ([]byte, error) { calls++; return []byte("ok"), nil })
	if err != nil || string(res) != "ok" || calls != 1 {
		t.Fatalf("closed Execute: res = %q err = %v calls = %d, want ok/nil/1", res, err, calls)
	}

	fail := func() ([]byte, error) { return nil, errors.New("boom") }

	// One failure: threshold (2) not reached, still closed.
	if _, err := b.Execute(fail); err == nil {
		t.Fatal("Execute(fail) = nil error, want failure")
	}
	if got := b.State(); got != "closed" {
		t.Fatalf("State() after 1 failure = %q, want closed", got)
	}

	// Second consecutive failure: threshold reached, circuit opens.
	if _, err := b.Execute(fail); err == nil {
		t.Fatal("Execute(fail) = nil error, want failure")
	}
	if got := b.State(); got != "open" {
		t.Fatalf("State() after 2 failures = %q, want open", got)
	}

	// Open: Execute returns ErrCircuitOpen without running fn.
	calls = 0
	_, err = b.Execute(func() ([]byte, error) { calls++; return []byte("x"), nil })
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("open Execute err = %v, want ErrCircuitOpen", err)
	}
	if calls != 0 {
		t.Fatalf("fn ran %d times while circuit open, want 0", calls)
	}

	// After the cooldown the circuit is half-open: one trial request runs,
	// and a success closes it again.
	time.Sleep(cooldown + 100*time.Millisecond)
	calls = 0
	res, err = b.Execute(func() ([]byte, error) { calls++; return []byte("ok"), nil })
	if err != nil || string(res) != "ok" || calls != 1 {
		t.Fatalf("half-open trial Execute: res = %q err = %v calls = %d, want ok/nil/1", res, err, calls)
	}
	if got := b.State(); got != "closed" {
		t.Fatalf("State() after successful trial = %q, want closed", got)
	}
}

func TestBreakerDefaults(t *testing.T) {
	// failThreshold <= 0 -> 5; no sleep needed: only the open transition
	// matters, the 30s cooldown is never waited out.
	b := NewBreaker("defaults", 0, 0)
	if got := b.State(); got != "closed" {
		t.Fatalf("initial State() = %q, want closed", got)
	}
	fail := func() ([]byte, error) { return nil, errors.New("boom") }
	for i := 0; i < 4; i++ {
		if _, err := b.Execute(fail); err == nil {
			t.Fatalf("Execute(fail) #%d = nil error, want failure", i+1)
		}
		if got := b.State(); got != "closed" {
			t.Fatalf("State() after %d failures = %q, want closed (default threshold 5)", i+1, got)
		}
	}
	if _, err := b.Execute(fail); err == nil {
		t.Fatal("Execute(fail) #5 = nil error, want failure")
	}
	if got := b.State(); got != "open" {
		t.Fatalf("State() after 5 failures = %q, want open", got)
	}
}

func TestErrCircuitOpen(t *testing.T) {
	b := NewBreaker("sanity", 1, time.Minute)
	if _, err := b.Execute(func() ([]byte, error) { return nil, errors.New("boom") }); err == nil {
		t.Fatal("Execute(fail) = nil error, want failure")
	}
	_, err := b.Execute(func() ([]byte, error) { return []byte("x"), nil })
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("errors.Is(err, ErrCircuitOpen) = false, got %v", err)
	}
}
