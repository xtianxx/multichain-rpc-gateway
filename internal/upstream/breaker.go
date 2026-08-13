package upstream

import (
	"errors"
	"time"

	"github.com/sony/gobreaker/v2"
)

// ErrCircuitOpen reports that the upstream circuit breaker is open.
var ErrCircuitOpen = errors.New("circuit breaker open")

// Breaker wraps a gobreaker v2 circuit breaker for an upstream and
// implements chain.Breaker.
type Breaker struct {
	cb *gobreaker.CircuitBreaker[[]byte]
}

// NewBreaker builds a breaker: failThreshold consecutive failures open the
// circuit; after cooldown it becomes half-open (single trial request).
// failThreshold <= 0 -> 5; cooldown <= 0 -> 30s.
func NewBreaker(name string, failThreshold int, cooldown time.Duration) *Breaker {
	if failThreshold <= 0 {
		failThreshold = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &Breaker{
		cb: gobreaker.NewCircuitBreaker[[]byte](gobreaker.Settings{
			Name:        name,
			MaxRequests: 1, // half-open: exactly one trial request
			Timeout:     cooldown,
			ReadyToTrip: func(counts gobreaker.Counts) bool {
				return counts.ConsecutiveFailures >= uint32(failThreshold)
			},
		}),
	}
}

// Execute runs fn through the breaker. While the circuit is open (or a
// half-open trial is in flight) it returns ErrCircuitOpen without calling
// fn; otherwise fn's result and error are returned verbatim.
func (b *Breaker) Execute(fn func() ([]byte, error)) ([]byte, error) {
	res, err := b.cb.Execute(fn)
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return nil, ErrCircuitOpen
	}
	return res, err
}

// State reports the current breaker state: "closed", "open", "half-open".
func (b *Breaker) State() string {
	return b.cb.State().String()
}
