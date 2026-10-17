package catalog_test

import (
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
)

func TestBreakerOpensAndRecovers(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC))
	b := catalog.NewBreaker(c, 5, 30*time.Second)
	for i := range 4 {
		if !b.Allow() {
			t.Fatalf("closed breaker rejected call %d", i)
		}
		b.Failure()
	}
	b.Success() // a success resets the consecutive count
	for range 4 {
		b.Allow()
		b.Failure()
	}
	if b.State() != catalog.BreakerClosed {
		t.Fatalf("4 consecutive failures after a success opened the breaker")
	}
	b.Allow()
	b.Failure()
	if b.State() != catalog.BreakerOpen || b.Allow() {
		t.Fatalf("5 consecutive failures: state %s", b.State())
	}
	c.Advance(30 * time.Second)
	if !b.Allow() || b.Allow() {
		t.Fatal("half-open must let exactly one probe through")
	}
	b.Failure() // the probe failed: open again for a full cooldown
	if b.State() != catalog.BreakerOpen {
		t.Fatalf("failed probe: state %s", b.State())
	}
	c.Advance(30 * time.Second)
	b.Allow()
	b.Success()
	if b.State() != catalog.BreakerClosed || !b.Allow() {
		t.Fatalf("successful probe: state %s", b.State())
	}
}

// TestBreakerLateFailureDoesNotExtendOpen: calls admitted before the breaker opened (slow calls
// timing out later) must not restart the cooldown; otherwise a burst of slow calls keeps the
// upstream unreachable far beyond the cooldown.
func TestBreakerLateFailureDoesNotExtendOpen(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC))
	b := catalog.NewBreaker(c, 5, 30*time.Second)
	for range 8 {
		b.Allow() // eight slow calls in flight
	}
	for range 5 {
		b.Failure()
	}
	c.Advance(20 * time.Second)
	b.Failure() // the 6th slow call times out while open
	b.Failure()
	c.Advance(10 * time.Second)
	if !b.Allow() {
		t.Fatal("cooldown restarted by failures of calls admitted before the breaker opened")
	}
}
