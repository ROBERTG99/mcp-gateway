//go:build integration

package redistest_test

import (
	"context"
	"testing"

	"github.com/ROBERTG99/mcp-gateway/internal/testutil/redistest"
)

// TestDatabasesAreLeased checks that concurrent users of one Redis (test binaries sharing the
// compose service in make test-docker) get distinct logical databases, held by a lease in
// database 0 until the test ends.
func TestDatabasesAreLeased(t *testing.T) {
	ctx := context.Background()
	var released int
	t.Run("hold", func(t *testing.T) {
		a, b := redistest.New(t), redistest.New(t)
		da, db := a.Options().DB, b.Options().DB
		if da == db || da < 1 || db < 1 || da > 15 || db > 15 {
			t.Fatalf("databases %d and %d: want two distinct ones in 1-15", da, db)
		}
		for _, n := range []int{da, db} {
			if held, err := redistest.Leased(ctx, a, n); err != nil || !held {
				t.Fatalf("database %d: leased %v, err %v", n, held, err)
			}
		}
		released = da
	})
	c := redistest.New(t)
	if held, err := redistest.Leased(ctx, c, released); err != nil || held && c.Options().DB != released {
		t.Fatalf("database %d still leased after its test ended (err %v)", released, err)
	}
}
