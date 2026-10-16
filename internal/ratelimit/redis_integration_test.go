//go:build integration

package ratelimit_test

import (
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/ratelimit"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/redistest"
)

func TestTokenBucketRealRedis(t *testing.T) {
	c := clock.NewFake(start)
	l := ratelimit.NewRedis(redistest.New(t), tenants(), c)
	for i := range 10 {
		if v, err := l.Allow(t.Context(), "t_beta", "agent-support"); err != nil || !v.Allowed {
			t.Fatalf("call %d: %+v %v", i+1, v, err)
		}
		c.Advance(10 * time.Millisecond)
	}
	v, err := l.Allow(t.Context(), "t_beta", "agent-support")
	if err != nil || v.Allowed || v.Reason != ratelimit.RateLimited || v.RetryAfter <= 0 {
		t.Fatalf("11th call: %+v %v", v, err)
	}
	for range 2 {
		_, _ = l.Allow(t.Context(), "t_small", "a")
	}
	_, _ = l.Allow(t.Context(), "t_small", "a")
	if v, _ := l.Allow(t.Context(), "t_small", "a"); v.Reason != ratelimit.BudgetExhausted {
		t.Fatalf("budget: %+v", v)
	}
}
