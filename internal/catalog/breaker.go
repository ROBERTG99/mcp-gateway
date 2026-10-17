package catalog

import (
	"sync"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
)

// BreakerState is the state of an upstream circuit breaker.
type BreakerState string

// Breaker states.
const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half_open"
)

// Breaker opens after Threshold consecutive failures and lets one probe through after Cooldown.
type Breaker struct {
	mu        sync.Mutex
	clock     clock.Clock
	threshold int
	cooldown  time.Duration
	failures  int
	openedAt  time.Time
	state     BreakerState
	probing   bool
}

// NewBreaker returns a closed breaker.
func NewBreaker(c clock.Clock, threshold int, cooldown time.Duration) *Breaker {
	return &Breaker{clock: c, threshold: threshold, cooldown: cooldown, state: BreakerClosed}
}

// Allow reports whether a call may proceed. In half-open state only one probe is allowed at a time.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerOpen:
		if b.clock.Now().Sub(b.openedAt) < b.cooldown {
			return false
		}
		b.state = BreakerHalfOpen
		b.probing = true
		return true
	case BreakerHalfOpen:
		if b.probing {
			return false
		}
		b.probing = true
		return true
	default:
		return true
	}
}

// Success records a successful call and closes the breaker.
func (b *Breaker) Success() {
	b.mu.Lock()
	b.failures, b.probing, b.state = 0, false, BreakerClosed
	b.mu.Unlock()
}

// Failure records a failed call; the breaker opens at the threshold or when a probe fails. While
// open, failures of calls admitted earlier (slow calls timing out late) do not restart the cooldown.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == BreakerOpen {
		return
	}
	b.failures++
	b.probing = false
	if b.state == BreakerHalfOpen || b.failures >= b.threshold {
		b.state = BreakerOpen
		b.openedAt = b.clock.Now()
	}
}

// State returns the current state without side effects.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == BreakerOpen && b.clock.Now().Sub(b.openedAt) >= b.cooldown {
		return BreakerHalfOpen
	}
	return b.state
}
