//go:build integration

package datagen_test

import (
	"io"
	"testing"

	"github.com/ROBERTG99/mcp-gateway/internal/datagen"
	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/pgtest"
)

func TestSeedTwiceGivesEqualCounts(t *testing.T) {
	ctx := t.Context()
	db := pgtest.New(t)
	if err := store.Migrate(ctx, db.URL, "up", io.Discard); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Open(ctx, db.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	d := datagen.Generate(datagen.DefaultSeed)
	var counts []datagen.Counts
	for range 2 {
		if err := datagen.Seed(ctx, pool, d); err != nil {
			t.Fatal(err)
		}
		c, err := datagen.CountRows(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		counts = append(counts, c)
	}
	want := datagen.Counts{Accounts: 600, Customers: 600, Orders: 2000, Notes: 1500}
	if counts[0] != want || counts[1] != want {
		t.Fatalf("counts = %+v, want %+v twice", counts, want)
	}
}
