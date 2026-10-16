// Package ratelimit enforces per-tenant and per-agent token buckets and a daily per-tenant call
// budget (reset at 00:00 UTC), with limits from config/tenants.yaml. Tokens are counted in
// micro-tokens with integer arithmetic so refills are exact.
package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/config"
)

// Verdict reasons.
const (
	RateLimited     = "rate_limited"
	BudgetExhausted = "budget_exhausted"
)

// DefaultTenant applies to tenants missing from the tenants file (conservative).
var DefaultTenant = config.Tenant{RatePerSecond: 1, Burst: 5, AgentRatePerSecond: 1, AgentBurst: 5, DailyBudget: 100}

// Verdict is the outcome of Allow.
type Verdict struct {
	Allowed    bool
	Reason     string        // RateLimited or BudgetExhausted when denied
	RetryAfter time.Duration // rate limited: when one token is available again
	ResetsAt   time.Time     // budget exhausted: next 00:00 UTC
}

// Limiter decides whether a call may proceed and, if so, consumes one token and one unit of budget.
type Limiter interface {
	Allow(ctx context.Context, tenant, agent string) (Verdict, error)
}

const micro = 1_000_000

func limitsFor(tenants map[string]config.Tenant, tenant string) config.Tenant {
	if t, ok := tenants[tenant]; ok {
		if t.AgentRatePerSecond == 0 {
			t.AgentRatePerSecond, t.AgentBurst = t.RatePerSecond, t.Burst
		}
		return t
	}
	return DefaultTenant
}

// ratePerMs is the refill in micro-tokens per millisecond.
func ratePerMs(perSecond float64) int64 { return max(1, int64(math.Round(perSecond*1000))) }

// NextMidnight returns the next 00:00 UTC after t.
func NextMidnight(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}

// BudgetKey is the Redis key of a tenant's budget counter for t's UTC day.
func BudgetKey(tenant string, t time.Time) string {
	return "mcpgw:budget:{" + tenant + "}:" + t.UTC().Format("20060102")
}

// Memory is an in-process limiter (per replica). It backs reads when Redis is unavailable.
type Memory struct {
	mu      sync.Mutex
	tenants map[string]config.Tenant
	clock   clock.Clock
	buckets map[string]*bucket
	budgets map[string]int64 // key: tenant + day
}

type bucket struct {
	tokens int64 // micro-tokens
	at     time.Time
}

// NewMemory returns an in-process limiter.
func NewMemory(tenants map[string]config.Tenant, c clock.Clock) *Memory {
	if c == nil {
		c = clock.Real{}
	}
	return &Memory{tenants: tenants, clock: c, buckets: map[string]*bucket{}, budgets: map[string]int64{}}
}

func (m *Memory) level(key string, perSecond float64, burst int, now time.Time) *bucket {
	b, ok := m.buckets[key]
	if !ok {
		b = &bucket{tokens: int64(burst) * micro, at: now}
		m.buckets[key] = b
	}
	if elapsed := now.Sub(b.at).Milliseconds(); elapsed > 0 {
		b.tokens = min(int64(burst)*micro, b.tokens+elapsed*ratePerMs(perSecond))
		b.at = b.at.Add(time.Duration(elapsed) * time.Millisecond)
	}
	return b
}

func waitMs(tokens int64, perSecond float64) time.Duration {
	need := micro - tokens
	r := ratePerMs(perSecond)
	return time.Duration((need+r-1)/r) * time.Millisecond
}

// Allow implements Limiter.
func (m *Memory) Allow(_ context.Context, tenant, agent string) (Verdict, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lim := limitsFor(m.tenants, tenant)
	now := m.clock.Now()
	dayKey := BudgetKey(tenant, now)
	if m.budgets[dayKey] >= lim.DailyBudget {
		return Verdict{Reason: BudgetExhausted, ResetsAt: NextMidnight(now)}, nil
	}
	tb := m.level("t\x00"+tenant, lim.RatePerSecond, lim.Burst, now)
	ab := m.level("a\x00"+tenant+"\x00"+agent, lim.AgentRatePerSecond, lim.AgentBurst, now)
	if tb.tokens < micro || ab.tokens < micro {
		var wait time.Duration
		if tb.tokens < micro {
			wait = waitMs(tb.tokens, lim.RatePerSecond)
		}
		if ab.tokens < micro {
			wait = max(wait, waitMs(ab.tokens, lim.AgentRatePerSecond))
		}
		return Verdict{Reason: RateLimited, RetryAfter: wait}, nil
	}
	tb.tokens -= micro
	ab.tokens -= micro
	m.budgets[dayKey]++
	return Verdict{Allowed: true}, nil
}
