package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/config"
)

// Redis is the shared limiter: one Lua script checks the budget and both buckets and consumes
// atomically. Keys: mcpgw:rl:{tenant}, mcpgw:rl:{tenant}:{agent}, mcpgw:budget:{tenant}:{yyyymmdd}.
type Redis struct {
	client  redis.UniversalClient
	tenants map[string]config.Tenant
	clock   clock.Clock
}

// NewRedis returns the Redis limiter.
func NewRedis(client redis.UniversalClient, tenants map[string]config.Tenant, c clock.Clock) *Redis {
	if c == nil {
		c = clock.Real{}
	}
	return &Redis{client: client, tenants: tenants, clock: c}
}

// The script works in micro-tokens; ARGV: now_ms, rate_t (micro/ms), burst_t (micro), rate_a,
// burst_a, budget, budget_ttl_ms. Returns {0, 0} allowed, {1, wait_ms} rate limited, {2, 0} budget.
var allowScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local used = tonumber(redis.call('GET', KEYS[3]) or '0')
if used >= tonumber(ARGV[6]) then return {2, 0} end
local function level(key, rate, burst)
  local v = redis.call('HMGET', key, 'tokens', 'ts')
  local tokens, ts = tonumber(v[1]), tonumber(v[2])
  if tokens == nil then return burst, now end
  local elapsed = now - ts
  if elapsed > 0 then
    tokens = math.min(burst, tokens + elapsed * rate)
    ts = ts + elapsed
  end
  return tokens, ts
end
local rt, bt, ra, ba = tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4]), tonumber(ARGV[5])
local t1, ts1 = level(KEYS[1], rt, bt)
local t2, ts2 = level(KEYS[2], ra, ba)
if t1 < 1000000 or t2 < 1000000 then
  local w = 0
  if t1 < 1000000 then w = math.ceil((1000000 - t1) / rt) end
  if t2 < 1000000 then w = math.max(w, math.ceil((1000000 - t2) / ra)) end
  return {1, w}
end
redis.call('HSET', KEYS[1], 'tokens', string.format('%d', t1 - 1000000), 'ts', string.format('%d', ts1))
redis.call('PEXPIRE', KEYS[1], math.ceil(bt / rt) + 60000)
redis.call('HSET', KEYS[2], 'tokens', string.format('%d', t2 - 1000000), 'ts', string.format('%d', ts2))
redis.call('PEXPIRE', KEYS[2], math.ceil(ba / ra) + 60000)
redis.call('INCR', KEYS[3])
redis.call('PEXPIRE', KEYS[3], ARGV[7])
return {0, 0}
`)

// Allow implements Limiter.
func (r *Redis) Allow(ctx context.Context, tenant, agent string) (Verdict, error) {
	lim := limitsFor(r.tenants, tenant)
	now := r.clock.Now()
	midnight := NextMidnight(now)
	keys := []string{"mcpgw:rl:{" + tenant + "}", "mcpgw:rl:{" + tenant + "}:" + agent, BudgetKey(tenant, now)}
	res, err := allowScript.Run(ctx, r.client, keys, now.UnixMilli(),
		ratePerMs(lim.RatePerSecond), int64(lim.Burst)*micro, ratePerMs(lim.AgentRatePerSecond), int64(lim.AgentBurst)*micro,
		lim.DailyBudget, midnight.Sub(now).Milliseconds()+int64(time.Hour/time.Millisecond)).Int64Slice()
	if err != nil {
		return Verdict{}, fmt.Errorf("redis rate limit: %w", err)
	}
	switch res[0] {
	case 0:
		return Verdict{Allowed: true}, nil
	case 1:
		return Verdict{Reason: RateLimited, RetryAfter: time.Duration(res[1]) * time.Millisecond}, nil
	default:
		return Verdict{Reason: BudgetExhausted, ResetsAt: midnight}, nil
	}
}
