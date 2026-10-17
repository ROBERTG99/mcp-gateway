// Package redistest provides a real Redis 7 for integration tests: the compose service when
// MCPGW_TEST_USE_COMPOSE=1 (MCPGW_TEST_REDIS_URL), otherwise one redis:7 testcontainer per test
// binary. Each test gets its own logical database (1-15), flushed at start and leased in database 0
// until the test ends, so test binaries that share one Redis never flush each other's data.
package redistest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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

// leaseTTL bounds how long a crashed test binary keeps a database. It exceeds go test's default
// 10 min timeout, so a live test never loses its lease.
const leaseTTL = 30 * time.Minute

// release deletes a lease only if it still holds this test's token.
var release = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`)

func leaseKey(db int) string { return fmt.Sprintf("redistest:lease:%d", db) }

// New returns a client on a fresh logical database (1-15) that no other test holds.
func New(t testing.TB) *redis.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := addr(ctx)
	if err != nil {
		t.Fatalf("redistest: %v", err)
	}
	ctl := redis.NewClient(&redis.Options{Addr: a, DB: 0})
	t.Cleanup(func() { _ = ctl.Close() })
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	db, err := lease(ctx, ctl, token)
	if err != nil {
		t.Fatalf("redistest: %v", err)
	}
	c := redis.NewClient(&redis.Options{Addr: a, DB: db})
	t.Cleanup(func() {
		_ = c.Close()
		_ = release.Run(context.Background(), ctl, []string{leaseKey(db)}, token).Err()
	})
	if err := c.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("redistest: flush: %v", err)
	}
	return c
}

// lease takes the first free database, starting after the last one this binary used, and polls
// until one is free or ctx ends.
func lease(ctx context.Context, ctl *redis.Client, token string) (int, error) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		start := int(shared.db.Add(1))
		for i := range 15 {
			db := (start+i)%15 + 1
			ok, err := ctl.SetNX(ctx, leaseKey(db), token, leaseTTL).Result()
			if err != nil {
				return 0, fmt.Errorf("lease database %d: %w", db, err)
			}
			if ok {
				return db, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, errors.New("no free logical database (1-15)")
		case <-tick.C:
		}
	}
}

// Leased reports whether database db is held by a test, read through any client of the same Redis.
func Leased(ctx context.Context, c *redis.Client, db int) (bool, error) {
	ctl := redis.NewClient(&redis.Options{Addr: c.Options().Addr, DB: 0})
	defer func() { _ = ctl.Close() }()
	n, err := ctl.Exists(ctx, leaseKey(db)).Result()
	if err != nil {
		return false, fmt.Errorf("lease %d: %w", db, err)
	}
	return n == 1, nil
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
