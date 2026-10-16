// Package redistest provides a real Redis 7 for integration tests: the compose service when
// MCPGW_TEST_USE_COMPOSE=1 (MCPGW_TEST_REDIS_URL), otherwise one redis:7 testcontainer per test
// binary. Each test gets its own logical database, flushed at start.
package redistest

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// shared is the per-test-binary container (see pgtest for why this is package-level).
var shared struct {
	once sync.Once
	addr string
	err  error
	db   atomic.Int32
}

func addr(ctx context.Context) (string, error) {
	shared.once.Do(func() {
		if os.Getenv("MCPGW_TEST_USE_COMPOSE") == "1" {
			u := os.Getenv("MCPGW_TEST_REDIS_URL")
			if u == "" {
				u = "redis://localhost:8230/15"
			}
			opts, err := redis.ParseURL(u)
			if err != nil {
				shared.err = err
				return
			}
			shared.addr = opts.Addr
			return
		}
		c, err := tcredis.Run(ctx, "redis:7")
		if err != nil {
			shared.err = fmt.Errorf("start redis container: %w", err)
			return
		}
		ep, err := c.Endpoint(ctx, "")
		shared.addr, shared.err = ep, err
	})
	return shared.addr, shared.err
}

// New returns a client on a fresh logical database (1-15, rotating).
func New(t testing.TB) *redis.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := addr(ctx)
	if err != nil {
		t.Fatalf("redistest: %v", err)
	}
	db := int(shared.db.Add(1)%15) + 1
	c := redis.NewClient(&redis.Options{Addr: a, DB: db})
	if err := c.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("redistest: flush: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Advance makes d pass for every key's TTL in the client's database (real Redis cannot fake time,
// so remaining TTLs are shortened instead; keys whose TTL runs out are deleted).
func Advance(t testing.TB, c *redis.Client, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	keys, err := c.Keys(ctx, "*").Result()
	if err != nil {
		t.Fatalf("redistest: keys: %v", err)
	}
	for _, k := range keys {
		ttl, err := c.PTTL(ctx, k).Result()
		if err != nil || ttl < 0 {
			continue
		}
		if ttl <= d {
			c.Del(ctx, k)
		} else {
			c.PExpire(ctx, k, ttl-d)
		}
	}
}
