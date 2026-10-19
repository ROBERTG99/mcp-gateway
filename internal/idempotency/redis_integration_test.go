//go:build integration

package idempotency_test

import (
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency/storetest"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/redistest"
)

func TestRedisStoreReal(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Harness {
		c := redistest.New(t)
		return storetest.Harness{Store: idempotency.NewRedis(c), Advance: func(d time.Duration) { redistest.Advance(t, c, d) }}
	})
}
