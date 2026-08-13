package prober

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtianxx/multichain-rpc-gateway/internal/chain"
	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
	"github.com/xtianxx/multichain-rpc-gateway/internal/upstream"
)

// discardLogger silences probe warnings during tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustParseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// newMockServer starts an httptest server that echoes the request id back on
// success or answers HTTP 500 when the ok flag is false. It counts hits.
func newMockServer() (*httptest.Server, *atomic.Bool, *atomic.Int32) {
	var ok atomic.Bool
	ok.Store(true)
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !ok.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":"0x1"}`, req.ID)
	}))
	return srv, &ok, &hits
}

func newMockUpstream(srv *httptest.Server) *chain.Upstream {
	return &chain.Upstream{Name: "mock-a", URL: mustParseURL(srv.URL), Client: upstream.NewHTTPClient()}
}

func TestProbeSuccessMarksHealthy(t *testing.T) {
	srv, ok, _ := newMockServer()
	defer srv.Close()
	ok.Store(true)

	u := newMockUpstream(srv)
	p := New(nil, config.Prober{}, discardLogger())

	res := p.Probe(context.Background(), u)

	if !res.OK {
		t.Fatal("expected a successful probe")
	}
	if u.Health() != chain.HealthHealthy {
		t.Fatalf("expected HealthHealthy, got %v", u.Health())
	}
	if u.FailStreak() != 0 {
		t.Fatalf("expected fail streak 0, got %d", u.FailStreak())
	}
	if u.Latency() <= 0 {
		t.Fatalf("expected recorded latency > 0, got %v", u.Latency())
	}
	if res.Upstream != u {
		t.Fatal("result should reference the probed upstream")
	}
	if res.CheckedAt.IsZero() {
		t.Fatal("expected a non-zero CheckedAt")
	}
}

func TestProbeFailureThresholdMarksUnhealthy(t *testing.T) {
	srv, ok, _ := newMockServer()
	defer srv.Close()
	ok.Store(false)

	u := newMockUpstream(srv)
	p := New(nil, config.Prober{FailThreshold: 2}, discardLogger())

	// First failure: streak 1, below threshold -> health stays Unknown.
	r1 := p.Probe(context.Background(), u)
	if r1.OK {
		t.Fatal("expected a failed probe")
	}
	if u.FailStreak() != 1 {
		t.Fatalf("expected fail streak 1, got %d", u.FailStreak())
	}
	if u.Health() != chain.HealthUnknown {
		t.Fatalf("expected HealthUnknown, got %v", u.Health())
	}

	// Second consecutive failure: streak 2 >= threshold -> Unhealthy.
	r2 := p.Probe(context.Background(), u)
	if r2.OK {
		t.Fatal("expected a failed probe")
	}
	if u.FailStreak() != 2 {
		t.Fatalf("expected fail streak 2, got %d", u.FailStreak())
	}
	if u.Health() != chain.HealthUnhealthy {
		t.Fatalf("expected HealthUnhealthy, got %v", u.Health())
	}

	// Upstream recovers: next probe resets the streak and marks Healthy.
	ok.Store(true)
	r3 := p.Probe(context.Background(), u)
	if !r3.OK {
		t.Fatal("expected a successful probe after recovery")
	}
	if u.FailStreak() != 0 {
		t.Fatalf("expected fail streak reset to 0, got %d", u.FailStreak())
	}
	if u.Health() != chain.HealthHealthy {
		t.Fatalf("expected HealthHealthy after recovery, got %v", u.Health())
	}
}

func TestProbeGoesThroughBreaker(t *testing.T) {
	srv, ok, hits := newMockServer()
	defer srv.Close()
	ok.Store(false)

	u := newMockUpstream(srv)
	br := upstream.NewBreaker("mock-a", 2, 50*time.Millisecond)
	u.SetBreaker(br)
	p := New(nil, config.Prober{FailThreshold: 3}, discardLogger())

	// Probe 1: fails through the closed breaker -> consecutive 1.
	r1 := p.Probe(context.Background(), u)
	if r1.OK {
		t.Fatal("expected a failed probe")
	}
	if u.FailStreak() != 1 {
		t.Fatalf("expected fail streak 1, got %d", u.FailStreak())
	}

	// Probe 2: fails -> consecutive 2, breaker trips OPEN. Both probes hit HTTP.
	r2 := p.Probe(context.Background(), u)
	if r2.OK {
		t.Fatal("expected a failed probe")
	}
	if u.FailStreak() != 2 {
		t.Fatalf("expected fail streak 2, got %d", u.FailStreak())
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("expected 2 HTTP hits before trip, got %d", got)
	}
	if !u.BreakerOpen() {
		t.Fatal("expected breaker to be open after two failures")
	}

	// Probe 3: open breaker fails fast, no HTTP round trip.
	r3 := p.Probe(context.Background(), u)
	if r3.OK {
		t.Fatal("expected a failed probe while breaker is open")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("expected no additional HTTP hit while open, got %d", got)
	}
	if !u.BreakerOpen() {
		t.Fatal("expected breaker to remain open")
	}
	// The fail-fast error surfaced through the breaker is ErrCircuitOpen.
	if _, err := br.Execute(func() ([]byte, error) { return nil, nil }); !errors.Is(err, upstream.ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen from open breaker, got %v", err)
	}
}

func TestStartProbesUntilCancelled(t *testing.T) {
	srv, ok, hits := newMockServer()
	defer srv.Close()
	ok.Store(true)

	u := newMockUpstream(srv)
	chains := []*chain.Chain{{ChainID: "1", Upstreams: []*chain.Upstream{u}}}
	p := New(chains, config.Prober{
		Interval:      config.Duration(20 * time.Millisecond),
		Timeout:       config.Duration(time.Second),
		FailThreshold: 3,
	}, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		p.Start(ctx)
		close(done)
	}()

	// Let the loop run past the 100ms deadline: expect several probe rounds.
	time.Sleep(130 * time.Millisecond)
	if got := hits.Load(); got < 2 {
		t.Fatalf("expected at least 2 probes before cancellation, got %d", got)
	}

	// Start must return once the context is done.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

func TestNewDefaults(t *testing.T) {
	p := New(nil, config.Prober{}, discardLogger())
	if p.interval != 10*time.Second {
		t.Fatalf("expected default interval 10s, got %v", p.interval)
	}
	if p.timeout != 5*time.Second {
		t.Fatalf("expected default timeout 5s, got %v", p.timeout)
	}
	if p.failThreshold != 3 {
		t.Fatalf("expected default failThreshold 3, got %d", p.failThreshold)
	}
}
