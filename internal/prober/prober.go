// Package prober actively probes every upstream with eth_chainId on a fixed
// interval, feeding the health state machine and the circuit breakers.
package prober

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/xtianxx/multichain-rpc-gateway/internal/chain"
	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
	"github.com/xtianxx/multichain-rpc-gateway/internal/upstream"
)

// HealthProbeResult records the outcome of one eth_chainId probe.
type HealthProbeResult struct {
	Upstream  *chain.Upstream
	OK        bool
	Latency   time.Duration
	CheckedAt time.Time
}

// Prober actively probes every upstream with eth_chainId on a fixed
// interval, feeding the health state machine and the circuit breakers.
type Prober struct {
	chains        []*chain.Chain
	interval      time.Duration
	timeout       time.Duration
	failThreshold int
	logger        *slog.Logger
	id            atomic.Uint64
}

// New builds a Prober; defaults when unset: interval 10s, timeout 5s,
// failThreshold 3.
func New(chains []*chain.Chain, cfg config.Prober, logger *slog.Logger) *Prober {
	interval := cfg.Interval.Std()
	if interval <= 0 {
		interval = 10 * time.Second
	}
	timeout := cfg.Timeout.Std()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	failThreshold := cfg.FailThreshold
	if failThreshold <= 0 {
		failThreshold = 3
	}
	return &Prober{
		chains:        chains,
		interval:      interval,
		timeout:       timeout,
		failThreshold: failThreshold,
		logger:        logger,
	}
}

// Start runs the probe loop until ctx is cancelled: every interval it probes
// every upstream of every chain (each probe bounded by the probe timeout).
// Returns when ctx is done. Never panics.
func (p *Prober) Start(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			if p.logger != nil {
				p.logger.Warn("probe loop recovered from panic", "panic", r)
			}
		}
	}()

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		p.probeAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// probeAll probes every upstream of every chain once.
func (p *Prober) probeAll(ctx context.Context) {
	for _, c := range p.chains {
		for _, u := range c.Upstreams {
			p.Probe(ctx, u)
		}
	}
}

// Probe performs one eth_chainId round trip through the upstream's breaker
// and updates health state, failure streak, and EWMA latency. The probe body
// is {"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":<N>} with a
// monotonically increasing decimal id from an atomic counter. Behavior:
//
//	success -> RecordProbeOK, RecordLatency(latency), SetHealth(HealthHealthy)
//	failure -> RecordProbeFail(); when the returned streak >= failThreshold,
//	           SetHealth(HealthUnhealthy)
//
// Failures are warn-logged (logger.Warn with upstream name + error) WITHOUT
// the request/response payload. Returns HealthProbeResult.
func (p *Prober) Probe(ctx context.Context, u *chain.Upstream) HealthProbeResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	id := p.id.Add(1)
	body := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":%d}`, id))
	idRaw := json.RawMessage(strconv.FormatUint(id, 10))

	_, err := u.Execute(func() ([]byte, error) {
		return upstream.Forward(ctx, u, body, idRaw)
	})

	res := HealthProbeResult{
		Upstream:  u,
		Latency:   time.Since(start),
		CheckedAt: time.Now(),
	}
	if err == nil {
		res.OK = true
		u.RecordProbeOK()
		u.RecordLatency(res.Latency)
		u.SetHealth(chain.HealthHealthy)
		return res
	}

	streak := u.RecordProbeFail()
	if streak >= p.failThreshold {
		u.SetHealth(chain.HealthUnhealthy)
	}
	if p.logger != nil {
		p.logger.Warn("upstream probe failed", "upstream", u.Name, "error", err)
	}
	return res
}
