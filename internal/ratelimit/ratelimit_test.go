package ratelimit_test

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"pgregory.net/rapid"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/ratelimit"
)

var start = time.Date(2026, 10, 16, 23, 59, 0, 0, time.UTC)

func tenants() map[string]config.Tenant {
	return map[string]config.Tenant{
		"t_beta":  {RatePerSecond: 5, Burst: 10, AgentRatePerSecond: 5, AgentBurst: 10, DailyBudget: 1000},
		"t_small": {RatePerSecond: 100, Burst: 100, AgentRatePerSecond: 100, AgentBurst: 100, DailyBudget: 3},
		"t_agent": {RatePerSecond: 100, Burst: 100, AgentRatePerSecond: 1, AgentBurst: 2, DailyBudget: 1000},
	}
}

type backend struct {
	name string
	new  func(t *testing.T, c *clock.Fake) ratelimit.Limiter
}

func backends() []backend {
	return []backend{
		{"memory", func(_ *testing.T, c *clock.Fake) ratelimit.Limiter { return ratelimit.NewMemory(tenants(), c) }},
		{"redis", func(t *testing.T, c *clock.Fake) ratelimit.Limiter {
			mr := miniredis.RunT(t)
			rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rc.Close() })
			return ratelimit.NewRedis(rc, tenants(), c)
		}},
	}
}

func TestTokenBucketRedis(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			c := clock.NewFake(start)
			l := b.new(t, c)
			for i := range 10 {
				v, err := l.Allow(t.Context(), "t_beta", "agent-support")
				if err != nil || !v.Allowed {
					t.Fatalf("call %d: %+v %v", i+1, v, err)
				}
				c.Advance(10 * time.Millisecond) // 11 calls within one second
			}
			v, err := l.Allow(t.Context(), "t_beta", "agent-support")
			if err != nil || v.Allowed || v.Reason != ratelimit.RateLimited || v.RetryAfter <= 0 || v.RetryAfter > 200*time.Millisecond {
				t.Fatalf("11th call: %+v %v, want rate_limited with retry_after in (0, 200ms]", v, err)
			}
			c.Advance(v.RetryAfter)
			if v, _ := l.Allow(t.Context(), "t_beta", "agent-support"); !v.Allowed {
				t.Fatalf("after retry_after: %+v", v)
			}
		})
	}
}

func TestAgentBucketIsSeparate(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			c := clock.NewFake(start)
			l := b.new(t, c)
			for range 2 {
				if v, _ := l.Allow(t.Context(), "t_agent", "noisy"); !v.Allowed {
					t.Fatal("within agent burst")
				}
			}
			if v, _ := l.Allow(t.Context(), "t_agent", "noisy"); v.Allowed {
				t.Fatal("agent bucket not enforced")
			}
			if v, _ := l.Allow(t.Context(), "t_agent", "quiet"); !v.Allowed {
				t.Fatal("one agent exhausted another agent's bucket")
			}
		})
	}
}

func TestDailyBudgetResets(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			c := clock.NewFake(start) // 23:59 UTC
			l := b.new(t, c)
			for range 3 {
				if v, _ := l.Allow(t.Context(), "t_small", "a"); !v.Allowed {
					t.Fatal("within budget")
				}
			}
			v, _ := l.Allow(t.Context(), "t_small", "a")
			if v.Allowed || v.Reason != ratelimit.BudgetExhausted || !v.ResetsAt.Equal(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)) {
				t.Fatalf("over budget: %+v", v)
			}
			c.Advance(59 * time.Second) // still 23:59:59
			if v, _ := l.Allow(t.Context(), "t_small", "a"); v.Allowed {
				t.Fatal("budget reset before midnight")
			}
			c.Advance(time.Second) // 00:00:00 UTC
			if v, _ := l.Allow(t.Context(), "t_small", "a"); !v.Allowed {
				t.Fatalf("budget did not reset at 00:00 UTC: %+v", v)
			}
		})
	}
}

func TestUnknownTenantUsesDefaults(t *testing.T) {
	c := clock.NewFake(start)
	l := ratelimit.NewMemory(tenants(), c)
	allowed := 0
	for range 50 {
		if v, _ := l.Allow(t.Context(), "t_unknown", "a"); v.Allowed {
			allowed++
		}
	}
	if allowed != ratelimit.DefaultTenant.Burst {
		t.Fatalf("unknown tenant allowed %d, want the default burst %d", allowed, ratelimit.DefaultTenant.Burst)
	}
}

// Property: over any schedule, allowed calls never exceed burst + rate * elapsed.
func TestTokenBucketNeverExceedsBurst(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		c := clock.NewFake(start)
		l := ratelimit.NewMemory(tenants(), c)
		gaps := rapid.SliceOfN(rapid.IntRange(0, 400), 1, 200).Draw(rt, "gaps_ms")
		var elapsed time.Duration
		allowed := 0
		for _, g := range gaps {
			c.Advance(time.Duration(g) * time.Millisecond)
			elapsed += time.Duration(g) * time.Millisecond
			if v, _ := l.Allow(t.Context(), "t_beta", "a"); v.Allowed {
				allowed++
			}
			limit := 10 + int(5*elapsed.Seconds()) + 1
			if allowed > limit {
				rt.Fatalf("allowed %d calls in %s, limit %d", allowed, elapsed, limit)
			}
		}
	})
}
