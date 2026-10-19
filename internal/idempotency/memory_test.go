package idempotency_test

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency/storetest"
)

func TestMemoryStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Harness {
		c := clock.NewFake(time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC))
		return storetest.Harness{Store: idempotency.NewMemory(c), Advance: c.Advance}
	})
}

// The Redis store against miniredis (in process); the integration test runs it on real Redis 7.
func TestRedisStoreMiniredis(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Harness {
		mr := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = client.Close() })
		return storetest.Harness{Store: idempotency.NewRedis(client), Advance: mr.FastForward}
	})
}
